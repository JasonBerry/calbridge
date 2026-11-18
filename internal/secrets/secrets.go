// Package secrets reads credentials from files, environment variables and
// systemd credentials, enforces file permissions, and persists the Google
// OAuth token atomically.
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ErrInsecurePerms is returned when a secret file is readable by others or
// not owned by the current user.
var ErrInsecurePerms = errors.New("secrets: insecure file permissions")

// CheckPerms verifies that path is a regular file with no group/other
// permission bits, owned by the current user. insecureOK disables the check
// (the --insecure-perms development override).
func CheckPerms(path string, insecureOK bool) error {
	if insecureOK {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrInsecurePerms, path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %#o, want 0600", ErrInsecurePerms, path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s is owned by uid %d, not the service user (uid %d)", ErrInsecurePerms, path, st.Uid, os.Getuid())
	}
	return nil
}

// ReadSecretFile reads a secret from path after checking its permissions and
// trims surrounding whitespace.
func ReadSecretFile(path string, insecureOK bool) (string, error) {
	if err := CheckPerms(path, insecureOK); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path) // #nosec G304 -- operator-configured secret path
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("secrets: %s is empty", path)
	}
	return s, nil
}

// ICloudPasswordEnv is the environment variable holding the iCloud app-specific password.
const ICloudPasswordEnv = "CALBRIDGE_ICLOUD_APP_PASSWORD"

// ICloudCredentialName is the systemd LoadCredential= name.
const ICloudCredentialName = "icloud-app-password" // #nosec G101 -- credential name, not a value

// ICloudAppPassword returns the app-specific password, trying in order: the
// environment variable, $CREDENTIALS_DIRECTORY/icloud-app-password (systemd),
// then file. getenv is normally os.Getenv.
func ICloudAppPassword(getenv func(string) string, file string, insecureOK bool) (string, error) {
	if v := strings.TrimSpace(getenv(ICloudPasswordEnv)); v != "" {
		return v, nil
	}
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		p := filepath.Join(dir, ICloudCredentialName)
		if _, err := os.Stat(p); err == nil {
			// systemd owns the credentials directory and its permissions.
			return ReadSecretFile(p, true)
		}
	}
	if file == "" {
		return "", errors.New("secrets: no iCloud app-specific password configured")
	}
	return ReadSecretFile(file, insecureOK)
}

// LoadOAuthConfig reads a Desktop-app OAuth client JSON.
func LoadOAuthConfig(clientFile string, insecureOK bool, scopes ...string) (*oauth2.Config, error) {
	if err := CheckPerms(clientFile, insecureOK); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(clientFile) // #nosec G304 -- operator-configured path
	if err != nil {
		return nil, err
	}
	cfg, err := google.ConfigFromJSON(b, scopes...)
	if err != nil {
		return nil, fmt.Errorf("secrets: parse OAuth client %s: %w", clientFile, err)
	}
	return cfg, nil
}

// LoadToken reads an oauth2.Token saved by SaveToken.
func LoadToken(path string, insecureOK bool) (*oauth2.Token, error) {
	if err := CheckPerms(path, insecureOK); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path) // #nosec G304 -- operator-configured path
	if err != nil {
		return nil, err
	}
	var t oauth2.Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("secrets: parse token %s: %w", path, err)
	}
	if t.RefreshToken == "" {
		return nil, fmt.Errorf("secrets: token %s has no refresh token; re-run `calbridge auth`", path)
	}
	return &t, nil
}

// SaveToken writes tok to path atomically with mode 0600: temp file in the
// same directory, fsync, rename, fsync directory.
func SaveToken(path string, tok *oauth2.Token) error {
	b, err := json.MarshalIndent(tok, "", "  ") // #nosec G117 -- persisting the token is the point; file is 0600
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, b, 0o600)
}

// WriteFileAtomic writes data to path via a temp file and rename.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil { // #nosec G304 -- directory of the target file
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// PersistingTokenSource wraps src and rewrites path whenever the access or
// refresh token changes, so refreshed tokens survive restarts.
type PersistingTokenSource struct {
	src  oauth2.TokenSource
	path string

	mu   sync.Mutex
	last *oauth2.Token
}

// NewPersistingTokenSource returns a TokenSource that refreshes from initial
// using cfg and persists changes to path.
func NewPersistingTokenSource(ctx context.Context, cfg *oauth2.Config, initial *oauth2.Token, path string) *PersistingTokenSource {
	return &PersistingTokenSource{
		src:  cfg.TokenSource(ctx, initial),
		path: path,
		last: initial,
	}
}

// WrapTokenSource is NewPersistingTokenSource for an arbitrary source.
func WrapTokenSource(src oauth2.TokenSource, initial *oauth2.Token, path string) *PersistingTokenSource {
	return &PersistingTokenSource{src: src, path: path, last: initial}
}

func (p *PersistingTokenSource) Token() (*oauth2.Token, error) {
	t, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last != nil && t.RefreshToken == "" {
		// Google omits the refresh token on refresh responses; keep the old one.
		t.RefreshToken = p.last.RefreshToken
	}
	if p.last == nil || t.AccessToken != p.last.AccessToken || t.RefreshToken != p.last.RefreshToken {
		if err := SaveToken(p.path, t); err != nil {
			return nil, fmt.Errorf("secrets: persist refreshed token: %w", err)
		}
		p.last = t
	}
	return t, nil
}
