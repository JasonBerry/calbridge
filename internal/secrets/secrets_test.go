package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func writeFile(t *testing.T, name, data string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(data), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil { // defeat umask
		t.Fatal(err)
	}
	return p
}

func TestCheckPerms(t *testing.T) {
	good := writeFile(t, "good", "x", 0o600)
	if err := CheckPerms(good, false); err != nil {
		t.Errorf("0600: %v", err)
	}
	for _, perm := range []os.FileMode{0o640, 0o604, 0o644} {
		p := writeFile(t, "bad", "x", perm)
		if err := CheckPerms(p, false); !errors.Is(err, ErrInsecurePerms) {
			t.Errorf("%#o: err = %v", perm, err)
		}
		if err := CheckPerms(p, true); err != nil {
			t.Errorf("%#o with override: %v", perm, err)
		}
	}
	if err := CheckPerms(t.TempDir(), false); !errors.Is(err, ErrInsecurePerms) {
		t.Errorf("directory: err = %v", err)
	}
}

func TestICloudAppPasswordOrder(t *testing.T) {
	file := writeFile(t, "pw", "file-pw\n", 0o600)
	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, ICloudCredentialName), []byte("cred-pw"), 0o400); err != nil {
		t.Fatal(err)
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"env wins", map[string]string{ICloudPasswordEnv: "env-pw", "CREDENTIALS_DIRECTORY": credDir}, "env-pw"},
		{"systemd credential", map[string]string{"CREDENTIALS_DIRECTORY": credDir}, "cred-pw"},
		{"file", map[string]string{}, "file-pw"},
	}
	for _, tc := range cases {
		got, err := ICloudAppPassword(env(tc.env), file, false)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	insecure := writeFile(t, "pw2", "x", 0o644)
	if _, err := ICloudAppPassword(env(nil), insecure, false); !errors.Is(err, ErrInsecurePerms) {
		t.Errorf("insecure file: err = %v", err)
	}
	empty := writeFile(t, "pw3", "  \n", 0o600)
	if _, err := ICloudAppPassword(env(nil), empty, false); err == nil {
		t.Error("empty file accepted")
	}
}

func TestSaveLoadToken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token.json")
	tok := &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Unix(1_900_000_000, 0).UTC()}
	if err := SaveToken(p, tok); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %#o", fi.Mode().Perm())
	}
	got, err := LoadToken(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "a" || got.RefreshToken != "r" || !got.Expiry.Equal(tok.Expiry) {
		t.Errorf("round trip: %+v", got)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("dir has %d entries", len(entries))
	}

	noRefresh := filepath.Join(t.TempDir(), "t.json")
	if err := SaveToken(noRefresh, &oauth2.Token{AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(noRefresh, false); err == nil {
		t.Error("token without refresh token accepted")
	}
}

// fakeTokenEndpoint issues a new access token per refresh and omits the
// refresh token in responses, like Google.
func fakeTokenEndpoint(t *testing.T, calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-1" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-" + string(rune('0'+n)),
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
}

func TestPersistingTokenSource(t *testing.T) {
	var calls atomic.Int32
	srv := fakeTokenEndpoint(t, &calls)
	defer srv.Close()

	cfg := &oauth2.Config{ClientID: "id", ClientSecret: "s", Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
	path := filepath.Join(t.TempDir(), "token.json")
	expired := &oauth2.Token{AccessToken: "old", RefreshToken: "refresh-1", Expiry: time.Now().Add(-time.Hour)}
	if err := SaveToken(path, expired); err != nil {
		t.Fatal(err)
	}

	ts := NewPersistingTokenSource(context.Background(), cfg, expired, path)
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-1" || tok.RefreshToken != "refresh-1" {
		t.Fatalf("token = %+v", tok)
	}
	saved, err := LoadToken(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "access-1" || saved.RefreshToken != "refresh-1" {
		t.Errorf("persisted = %+v", saved)
	}

	// A still-valid token must not trigger a refresh or a rewrite.
	before, _ := os.Stat(path)
	if _, err := ts.Token(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if calls.Load() != 1 {
		t.Errorf("refresh calls = %d, want 1", calls.Load())
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("token file rewritten without a change")
	}
}

func TestPersistingTokenSourceInvalidGrant(t *testing.T) {
	var calls atomic.Int32
	srv := fakeTokenEndpoint(t, &calls)
	defer srv.Close()
	cfg := &oauth2.Config{ClientID: "id", Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
	path := filepath.Join(t.TempDir(), "token.json")
	revoked := &oauth2.Token{AccessToken: "old", RefreshToken: "revoked", Expiry: time.Now().Add(-time.Hour)}
	ts := NewPersistingTokenSource(context.Background(), cfg, revoked, path)
	if _, err := ts.Token(); err == nil {
		t.Fatal("expected invalid_grant error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("token file written after failed refresh")
	}
}
