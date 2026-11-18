package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeGoogleOAuth checks the PKCE verifier against the challenge from the
// consent URL and returns a token.
type fakeGoogleOAuth struct {
	challenge   string
	redirect    string
	noRefresh   bool
	exchangeErr bool
}

func (f *fakeGoogleOAuth) tokenServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
			t.Errorf("PKCE verifier does not match challenge")
		}
		if r.Form.Get("code") != "the-code" || r.Form.Get("redirect_uri") != f.redirect {
			t.Errorf("exchange form = %v", r.Form)
		}
		if f.exchangeErr {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		resp := map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600}
		if !f.noRefresh {
			resp["refresh_token"] = "rt"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// browser simulates the user consenting: it parses the consent URL and calls
// the redirect URI, optionally tampering with it.
func (f *fakeGoogleOAuth) browser(t *testing.T, tamper func(q url.Values)) func(string) error {
	return func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" ||
			q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
			t.Errorf("consent URL missing parameters: %s", raw)
		}
		f.challenge = q.Get("code_challenge")
		f.redirect = q.Get("redirect_uri")
		cb := url.Values{"code": {"the-code"}, "state": {q.Get("state")}}
		if tamper != nil {
			tamper(cb)
		}
		go func() {
			resp, err := http.Get(f.redirect + "?" + cb.Encode()) // #nosec G107 -- test
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
}

func runFlow(t *testing.T, f *fakeGoogleOAuth, tamper func(url.Values)) (*oauth2.Token, error) {
	t.Helper()
	srv := f.tokenServer(t)
	defer srv.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	flow := LoopbackFlow{
		Config: &oauth2.Config{
			ClientID: "cid", ClientSecret: "cs",
			Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/auth", TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams},
			Scopes:   []string{"https://www.googleapis.com/auth/calendar.events"},
		},
		Listener:    ln,
		OpenBrowser: f.browser(t, tamper),
		Out:         &strings.Builder{},
		Timeout:     5 * time.Second,
	}
	return flow.Run(context.Background())
}

func TestLoopbackFlow(t *testing.T) {
	f := &fakeGoogleOAuth{}
	tok, err := runFlow(t, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "rt" || tok.AccessToken != "at" {
		t.Errorf("token = %+v", tok)
	}
	if !strings.HasPrefix(f.redirect, "http://127.0.0.1:") || !strings.HasSuffix(f.redirect, "/callback") {
		t.Errorf("redirect = %q", f.redirect)
	}
}

func TestLoopbackFlowStateMismatch(t *testing.T) {
	_, err := runFlow(t, &fakeGoogleOAuth{}, func(q url.Values) { q.Set("state", "forged") })
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoopbackFlowDenied(t *testing.T) {
	_, err := runFlow(t, &fakeGoogleOAuth{}, func(q url.Values) { q.Del("code"); q.Set("error", "access_denied") })
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoopbackFlowNoRefreshToken(t *testing.T) {
	_, err := runFlow(t, &fakeGoogleOAuth{noRefresh: true}, nil)
	if !errors.Is(err, ErrNoRefreshToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoopbackFlowRejectsNonLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	_, err = LoopbackFlow{Config: &oauth2.Config{}, Listener: ln}.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("err = %v", err)
	}
}
