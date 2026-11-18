// Package icloud is calbridge's thin CalDAV client for iCloud (PLAN.md §4,
// §11): discovery, conditional PUT/DELETE, calendar-multiget, RFC 6578
// sync-collection, getctag and getetag listing, and UID lookup. It is built on
// net/http and encoding/xml because go-webdav v0.7.0 lacks conditional writes
// and sync-collection.
package icloud

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"golang.org/x/time/rate"
)

// DefaultBaseURL is iCloud's CalDAV entry point. Never hardcode the
// per-account partition host (pNN-caldav.icloud.com); Discover finds it.
const DefaultBaseURL = "https://caldav.icloud.com/"

// Object is one calendar object resource.
type Object struct {
	Href string
	ETag string
	Cal  *ical.Calendar // nil if Raw did not parse
	Raw  []byte
}

// PutCond selects a conditional PUT.
type PutCond struct {
	IfNoneMatchAny bool   // create only
	IfMatch        string // update only if the ETag still matches
}

// DeleteOpts controls DELETE.
type DeleteOpts struct {
	IfMatch       string
	ScheduleReply *bool // nil = header omitted; false = "Schedule-Reply: F" (RFC 6638 §8.1)
}

// Member is an href with its ETag.
type Member struct{ Href, ETag string }

// SyncResult is the outcome of a sync-collection REPORT.
type SyncResult struct {
	Changed  []Member
	Removed  []string
	NewToken string
}

// CalendarInfo describes one collection under the calendar home.
type CalendarInfo struct {
	Href         string
	DisplayName  string
	Components   []string // supported-calendar-component-set, e.g. VEVENT, VTODO
	CTag         string
	SyncToken    string
	SupportsSync bool     // supported-report-set lists sync-collection
	Privileges   []string // current-user-privilege-set
	IsCalendar   bool
}

// Discovery is the result of Discover.
type Discovery struct {
	Principal, CalendarHome       string
	BaseURL                       string // partition host the home lives on
	AddressSet                    []string
	ScheduleInbox, ScheduleOutbox string
	Calendars                     []CalendarInfo
}

var (
	ErrNotFound           = errors.New("caldav: not found")
	ErrPreconditionFailed = errors.New("caldav: precondition failed (412)")
	ErrUIDConflict        = errors.New("caldav: uid conflict")
	ErrInvalidSyncToken   = errors.New("caldav: invalid sync token")
	ErrAuth               = errors.New("caldav: authorization failed")
	ErrSyncUnsupported    = errors.New("caldav: sync-collection not supported")
)

// CalDAV is what the syncer depends on.
type CalDAV interface {
	Discover(ctx context.Context) (Discovery, error)
	Get(ctx context.Context, href string) (*Object, error)
	MultiGet(ctx context.Context, calendarHref string, hrefs []string) ([]Object, error)
	Put(ctx context.Context, href string, cal *ical.Calendar, cond PutCond) (etag string, err error)
	Delete(ctx context.Context, href string, opts DeleteOpts) error
	SyncCollection(ctx context.Context, calendarHref, token string) (SyncResult, error)
	CTag(ctx context.Context, calendarHref string) (string, error)
	ListETags(ctx context.Context, calendarHref string) ([]Member, error)
	FindByUID(ctx context.Context, calendarHref, uid string) (*Object, error)
}

// HTTPError carries an unexpected HTTP status. errors.Is maps 401/403 to
// ErrAuth, 404/410 to ErrNotFound and 412 to ErrPreconditionFailed.
type HTTPError struct {
	Method, URL string
	Status      int
	Body        string // truncated
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("caldav: %s %s: HTTP %d", e.Method, e.URL, e.Status)
}

func (e *HTTPError) Is(target error) bool {
	switch target {
	case ErrAuth:
		return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
	case ErrNotFound:
		return e.Status == http.StatusNotFound || e.Status == http.StatusGone
	case ErrPreconditionFailed:
		return e.Status == http.StatusPreconditionFailed
	}
	return false
}

// Options configures a Client.
type Options struct {
	BaseURL      string // default DefaultBaseURL
	Username     string
	Password     string // app-specific password
	HTTPClient   *http.Client
	RateLimitRPS float64 // default 5
	Burst        int     // default 10
	UserAgent    string
}

// Client implements CalDAV.
type Client struct {
	base    *url.URL
	hc      *http.Client
	user    string
	pass    string
	ua      string
	limiter *rate.Limiter
	retry   retryPolicy
}

var _ CalDAV = (*Client)(nil)

// New builds a Client.
func New(o Options) (*Client, error) {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	base, err := url.Parse(o.BaseURL)
	if err != nil {
		return nil, err
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if o.RateLimitRPS <= 0 {
		o.RateLimitRPS = 5
	}
	if o.Burst <= 0 {
		o.Burst = 10
	}
	if o.UserAgent == "" {
		o.UserAgent = "calbridge"
	}
	return &Client{
		base:    base,
		hc:      o.HTTPClient,
		user:    o.Username,
		pass:    o.Password,
		ua:      o.UserAgent,
		limiter: rate.NewLimiter(rate.Limit(o.RateLimitRPS), o.Burst),
		retry:   defaultRetry,
	}, nil
}

// BaseURL returns the current base (the partition host after Discover).
func (c *Client) BaseURL() string { return c.base.String() }

// SetBaseURL switches to another host, e.g. a stored partition URL.
func (c *Client) SetBaseURL(u string) error {
	b, err := url.Parse(u)
	if err != nil {
		return err
	}
	c.base = b
	return nil
}

// SetSleep replaces the backoff sleep (tests).
func (c *Client) SetSleep(sleep func(context.Context, time.Duration) error) { c.retry.sleep = sleep }

// resolve turns an href (path or absolute URL) into an absolute URL.
func (c *Client) resolve(href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	return c.base.ResolveReference(u).String(), nil
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends one request with rate limiting and retry. body is replayable.
func (c *Client) do(ctx context.Context, method, href string, header http.Header, body []byte) (*response, error) {
	target, err := c.resolve(href)
	if err != nil {
		return nil, err
	}
	var resp *response
	err = c.retry.do(ctx, func() (time.Duration, error) {
		if err := c.limiter.Wait(ctx); err != nil {
			return 0, err
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rd)
		if err != nil {
			return 0, err
		}
		for k, vs := range header {
			req.Header[k] = vs
		}
		req.SetBasicAuth(c.user, c.pass)
		req.Header.Set("User-Agent", c.ua)
		hr, err := c.hc.Do(req)
		if err != nil {
			return 0, err
		}
		defer func() { _ = hr.Body.Close() }()
		b, err := io.ReadAll(io.LimitReader(hr.Body, 32<<20))
		if err != nil {
			return 0, err
		}
		resp = &response{status: hr.StatusCode, header: hr.Header, body: b}
		if retryableStatus(hr.StatusCode) {
			return retryAfter(hr.Header.Get("Retry-After")), &HTTPError{Method: method, URL: target, Status: hr.StatusCode, Body: truncate(b)}
		}
		return 0, nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func httpErr(method, target string, r *response) error {
	return &HTTPError{Method: method, URL: target, Status: r.status, Body: truncate(r.body)}
}

func truncate(b []byte) string {
	if len(b) > 2048 {
		b = b[:2048]
	}
	return string(b)
}

func retryableStatus(s int) bool {
	switch s {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryAfter parses a Retry-After header (seconds or HTTP date); 0 if absent.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// retryPolicy: honour Retry-After, else min(2^n s + jitter, max); 5 attempts.
type retryPolicy struct {
	attempts int
	max      time.Duration
	sleep    func(context.Context, time.Duration) error
}

var defaultRetry = retryPolicy{attempts: 5, max: 64 * time.Second, sleep: sleepCtx}

func (p retryPolicy) do(ctx context.Context, fn func() (time.Duration, error)) error {
	var err error
	for n := 0; n < p.attempts; n++ {
		var wait time.Duration
		wait, err = fn()
		var he *HTTPError
		if err == nil || !errors.As(err, &he) || !retryableStatus(he.Status) {
			return err
		}
		if n == p.attempts-1 {
			break
		}
		if wait == 0 {
			wait = time.Duration(1<<n)*time.Second + time.Duration(rand.Int64N(int64(time.Second))) // #nosec G404 -- jitter
		}
		if wait > p.max {
			wait = p.max
		}
		if serr := p.sleep(ctx, wait); serr != nil {
			return serr
		}
	}
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NormalizeETag returns e quoted (strong) or with its W/ prefix intact, so
// stored and compared ETags agree regardless of server quoting quirks
// (go-webdav issues #129/#132).
func NormalizeETag(e string) string {
	e = strings.TrimSpace(e)
	if e == "" {
		return ""
	}
	weak := ""
	if strings.HasPrefix(e, "W/") {
		weak, e = "W/", e[2:]
	}
	e = strings.Trim(e, `"`)
	return weak + `"` + e + `"`
}

// SameETag compares ETags after normalisation.
func SameETag(a, b string) bool { return a != "" && NormalizeETag(a) == NormalizeETag(b) }
