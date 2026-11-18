package secrets

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// LoopbackFlow runs the OAuth 2.0 authorization-code flow for a Desktop-app
// client with a loopback redirect (http://127.0.0.1:<port>/callback), PKCE
// (S256) and a random state. The device flow is not permitted for Calendar
// scopes (PLAN.md §5), so this is the only supported interactive flow.
type LoopbackFlow struct {
	Config   *oauth2.Config
	Listener net.Listener // must be bound to 127.0.0.1
	// OpenBrowser is called with the consent URL; errors are reported but not fatal.
	OpenBrowser func(url string) error
	Out         io.Writer
	Timeout     time.Duration // default 5 minutes
}

// ErrNoRefreshToken means Google did not return a refresh token.
var ErrNoRefreshToken = errors.New("oauth: no refresh token returned (revoke the app's access in the Google account and retry)")

// Run blocks until the browser redirects back, then exchanges the code.
func (f LoopbackFlow) Run(ctx context.Context) (*oauth2.Token, error) {
	timeout := f.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addr, ok := f.Listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		return nil, fmt.Errorf("oauth: listener %s is not a loopback TCP address", f.Listener.Addr())
	}
	cfg := *f.Config
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", addr.Port)

	state, err := randomString(24)
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce, // prompt=consent: guarantees a refresh token
		oauth2.S256ChallengeOption(verifier),
	)

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	send := func(r result) {
		select {
		case results <- r:
		default:
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "Authorization failed: "+html.EscapeString(e), http.StatusBadRequest)
			send(result{err: fmt.Errorf("oauth: authorization denied: %s", e)})
			return
		}
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "State mismatch", http.StatusBadRequest)
			send(result{err: errors.New("oauth: state mismatch")})
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "Missing code", http.StatusBadRequest)
			send(result{err: errors.New("oauth: callback without code")})
			return
		}
		_, _ = io.WriteString(w, "calbridge is authorized. You can close this tab.\n")
		send(result{code: code})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(f.Listener) }()
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	}()

	if f.Out != nil {
		_, _ = fmt.Fprintf(f.Out, "Open this URL in a browser signed in to the hidden Google account:\n\n%s\n\n", authURL)
		_, _ = fmt.Fprintln(f.Out, "The app is unverified: choose Advanced → Go to calbridge (unsafe). Waiting for the redirect…")
	}
	if f.OpenBrowser != nil {
		if err := f.OpenBrowser(authURL); err != nil && f.Out != nil {
			_, _ = fmt.Fprintf(f.Out, "(could not open a browser automatically: %v)\n", err)
		}
	}

	var res result
	select {
	case res = <-results:
	case <-ctx.Done():
		return nil, fmt.Errorf("oauth: waiting for redirect: %w", ctx.Err())
	}
	if res.err != nil {
		return nil, res.err
	}
	tok, err := cfg.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("oauth: exchange code: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, ErrNoRefreshToken
	}
	return tok, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
