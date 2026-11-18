package icloud

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// DumpTransport writes every request/response pair to Dir as numbered text
// files with credentials redacted. Captures still contain event data and
// real addresses: they belong in a git-ignored directory and must be redacted
// by hand before being copied to docs/phase0/ (CLAUDE.md rule 1).
type DumpTransport struct {
	Base   http.RoundTripper
	Dir    string
	Prefix string

	mu sync.Mutex
	n  int
}

var redactedHeaders = map[string]bool{
	"Authorization": true, "Cookie": true, "Set-Cookie": true, "Proxy-Authorization": true,
}

// RoundTrip implements http.RoundTripper.
func (d *DumpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := d.Base
	if base == nil {
		base = http.DefaultTransport
	}
	var reqBody []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		reqBody = b
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	resp, rtErr := base.RoundTrip(req)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s %s\n", req.Method, req.URL.String())
	writeHeaders(&buf, req.Header)
	buf.WriteString("\n")
	buf.Write(reqBody)
	buf.WriteString("\n\n----- response -----\n")
	if rtErr != nil {
		fmt.Fprintf(&buf, "transport error: %v\n", rtErr)
	} else {
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(b))
		fmt.Fprintf(&buf, "%s\n", resp.Status)
		writeHeaders(&buf, resp.Header)
		buf.WriteString("\n")
		buf.Write(b)
		buf.WriteString("\n")
	}

	d.mu.Lock()
	d.n++
	n := d.n
	d.mu.Unlock()
	name := fmt.Sprintf("%s%03d-%s-%s.txt", d.Prefix, n, req.Method, slug(req.URL.Path))
	if err := os.MkdirAll(d.Dir, 0o700); err == nil {
		_ = os.WriteFile(filepath.Join(d.Dir, name), buf.Bytes(), 0o600) // #nosec G703 -- name is slugged ([A-Za-z0-9._-] only)
	}
	return resp, rtErr
}

func writeHeaders(w *bytes.Buffer, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			if redactedHeaders[http.CanonicalHeaderKey(k)] {
				v = "REDACTED"
			}
			fmt.Fprintf(w, "%s: %s\n", k, v)
		}
	}
}

var slugRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func slug(p string) string {
	s := strings.Trim(slugRe.ReplaceAllString(p, "_"), "_")
	if len(s) > 60 {
		s = s[len(s)-60:]
	}
	if s == "" {
		s = "root"
	}
	return s
}
