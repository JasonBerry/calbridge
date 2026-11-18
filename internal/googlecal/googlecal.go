// Package googlecal wraps the Google Calendar API v3 client with the small
// surface calbridge needs (PLAN.md §11), sentinel errors and retry/backoff
// (PLAN.md §8.7).
package googlecal

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/JasonBerry/calbridge/internal/secrets"
)

// ListRequest selects one page of events.list. SyncToken and TimeMin are
// mutually exclusive: Google rejects most filters with a sync token (400).
type ListRequest struct {
	CalendarID string
	SyncToken  string
	TimeMin    time.Time
	PageToken  string
}

// ListPage is one page of events.list. NextSyncToken is set only on the last page.
type ListPage struct {
	Items         []*calendar.Event
	NextPageToken string
	NextSyncToken string
}

var (
	ErrSyncTokenGone = errors.New("google: sync token gone (410)")
	ErrAuth          = errors.New("google: authorization failed")
	ErrNotFound      = errors.New("google: not found")
	ErrNotAttendee   = errors.New("google: calendar owner is not an attendee of this event")
	ErrBadStatus     = errors.New("google: invalid responseStatus")
)

// GoogleCalendar is what the syncer depends on.
type GoogleCalendar interface {
	List(ctx context.Context, req ListRequest) (ListPage, error)
	Get(ctx context.Context, calendarID, eventID string) (*calendar.Event, error)
	InstanceAt(ctx context.Context, calendarID, masterID string, originalStart time.Time) (*calendar.Event, error)
	// SetSelfResponse GETs the event, sets responseStatus on attendees[self=true]
	// and PATCHes the full attendees array (arrays are replaced wholesale).
	SetSelfResponse(ctx context.Context, calendarID, eventID, status, sendUpdates string) (*calendar.Event, error)
}

// Client implements GoogleCalendar over the real API.
type Client struct {
	svc   *calendar.Service
	retry retryPolicy
}

var _ GoogleCalendar = (*Client)(nil)

// New builds a Client. Pass option.WithHTTPClient (an oauth2 client) in
// production, plus option.WithEndpoint in tests.
func New(ctx context.Context, opts ...option.ClientOption) (*Client, error) {
	svc, err := calendar.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc, retry: defaultRetry}, nil
}

// SetSleep replaces the backoff sleep (tests).
func (c *Client) SetSleep(sleep func(context.Context, time.Duration) error) { c.retry.sleep = sleep }

func (c *Client) List(ctx context.Context, req ListRequest) (ListPage, error) {
	call := c.svc.Events.List(req.CalendarID).MaxResults(250)
	if req.SyncToken != "" {
		call = call.SyncToken(req.SyncToken) // deleted items are always included
	} else {
		call = call.ShowDeleted(false)
		if !req.TimeMin.IsZero() {
			call = call.TimeMin(req.TimeMin.UTC().Format(time.RFC3339))
		}
	}
	if req.PageToken != "" {
		call = call.PageToken(req.PageToken)
	}
	var out *calendar.Events
	err := c.retry.do(ctx, func() error {
		var err error
		out, err = call.Context(ctx).Do()
		return err
	})
	if err != nil {
		if code(err) == http.StatusGone {
			return ListPage{}, fmt.Errorf("%w: %v", ErrSyncTokenGone, err)
		}
		return ListPage{}, classify(err)
	}
	return ListPage{Items: out.Items, NextPageToken: out.NextPageToken, NextSyncToken: out.NextSyncToken}, nil
}

// ListAll follows NextPageToken until a page carries NextSyncToken.
func ListAll(ctx context.Context, g GoogleCalendar, req ListRequest) ([]*calendar.Event, string, error) {
	var items []*calendar.Event
	for {
		page, err := g.List(ctx, req)
		if err != nil {
			return nil, "", err
		}
		items = append(items, page.Items...)
		if page.NextPageToken == "" {
			if page.NextSyncToken == "" {
				return nil, "", errors.New("google: last page has no nextSyncToken")
			}
			return items, page.NextSyncToken, nil
		}
		req.PageToken = page.NextPageToken
	}
}

func (c *Client) Get(ctx context.Context, calendarID, eventID string) (*calendar.Event, error) {
	var ev *calendar.Event
	err := c.retry.do(ctx, func() error {
		var err error
		ev, err = c.svc.Events.Get(calendarID, eventID).Context(ctx).Do()
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	return ev, nil
}

// InstanceAt finds the instance of a recurring event by its original start.
func (c *Client) InstanceAt(ctx context.Context, calendarID, masterID string, originalStart time.Time) (*calendar.Event, error) {
	var out *calendar.Events
	err := c.retry.do(ctx, func() error {
		var err error
		out, err = c.svc.Events.Instances(calendarID, masterID).
			OriginalStart(originalStart.UTC().Format(time.RFC3339)).
			ShowDeleted(true).Context(ctx).Do()
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	if len(out.Items) == 0 {
		return nil, ErrNotFound
	}
	return out.Items[0], nil
}

// ValidResponseStatus reports whether s is a Google attendee responseStatus.
func ValidResponseStatus(s string) bool {
	switch s {
	case "accepted", "tentative", "declined", "needsAction":
		return true
	}
	return false
}

func (c *Client) SetSelfResponse(ctx context.Context, calendarID, eventID, status, sendUpdates string) (*calendar.Event, error) {
	if !ValidResponseStatus(status) {
		return nil, fmt.Errorf("%w: %q", ErrBadStatus, status)
	}
	switch sendUpdates {
	case "all", "externalOnly", "none":
	default:
		return nil, fmt.Errorf("google: invalid sendUpdates %q", sendUpdates)
	}
	ev, err := c.Get(ctx, calendarID, eventID)
	if err != nil {
		return nil, err
	}
	attendees, err := WithSelfResponse(ev.Attendees, status)
	if err != nil {
		return nil, err
	}
	patch := &calendar.Event{Attendees: attendees}
	var out *calendar.Event
	err = c.retry.do(ctx, func() error {
		var err error
		out, err = c.svc.Events.Patch(calendarID, eventID, patch).SendUpdates(sendUpdates).Context(ctx).Do()
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// WithSelfResponse returns a copy of attendees with responseStatus changed on
// the self entry only. events.patch replaces arrays, so callers must send the
// whole result.
func WithSelfResponse(attendees []*calendar.EventAttendee, status string) ([]*calendar.EventAttendee, error) {
	out := make([]*calendar.EventAttendee, len(attendees))
	found := false
	for i, a := range attendees {
		cp := *a
		if a.Self {
			cp.ResponseStatus = status
			found = true
		}
		out[i] = &cp
	}
	if !found {
		return nil, ErrNotAttendee
	}
	return out, nil
}

// SelfAttendee returns the attendees[self=true] entry, or nil.
func SelfAttendee(ev *calendar.Event) *calendar.EventAttendee {
	for _, a := range ev.Attendees {
		if a.Self {
			return a
		}
	}
	return nil
}

func code(err error) int {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code
	}
	return 0
}

func reasons(err error) []string {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return nil
	}
	var rs []string
	for _, e := range gerr.Errors {
		rs = append(rs, e.Reason)
	}
	return rs
}

// classify maps API errors onto sentinel errors, keeping the original wrapped.
func classify(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return fmt.Errorf("%w: %v", ErrAuth, err)
	}
	switch code(err) {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %v", ErrAuth, err)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case http.StatusForbidden:
		if !isRateLimit(err) {
			return fmt.Errorf("%w: %v", ErrAuth, err)
		}
	}
	return err
}

func isRateLimit(err error) bool {
	for _, r := range reasons(err) {
		switch r {
		case "usageLimits", "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded":
			return true
		}
	}
	return false
}

func retryable(err error) bool {
	switch c := code(err); {
	case c == http.StatusTooManyRequests, c >= 500 && c <= 504:
		return true
	case c == http.StatusForbidden:
		return isRateLimit(err)
	}
	return false
}

// retryPolicy is truncated exponential backoff: min(2^n s + rand(0..1s), max).
type retryPolicy struct {
	attempts int
	max      time.Duration
	sleep    func(context.Context, time.Duration) error
}

var defaultRetry = retryPolicy{attempts: 5, max: 64 * time.Second, sleep: sleepCtx}

func (p retryPolicy) do(ctx context.Context, fn func() error) error {
	var err error
	for n := 0; n < p.attempts; n++ {
		if err = fn(); err == nil || !retryable(err) {
			return err
		}
		if n == p.attempts-1 {
			break
		}
		d := time.Duration(1<<n)*time.Second + time.Duration(rand.Int64N(int64(time.Second))) // #nosec G404 -- jitter
		if d > p.max {
			d = p.max
		}
		if serr := p.sleep(ctx, d); serr != nil {
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

// NewAuthorized builds a Client from the OAuth client JSON and the token file
// written by `calbridge auth`; refreshed tokens are persisted back to tokenFile.
func NewAuthorized(ctx context.Context, clientFile, tokenFile, scope string, insecurePerms bool) (*Client, error) {
	oc, err := secrets.LoadOAuthConfig(clientFile, insecurePerms, scope)
	if err != nil {
		return nil, err
	}
	tok, err := secrets.LoadToken(tokenFile, insecurePerms)
	if err != nil {
		return nil, err
	}
	ts := oauth2.ReuseTokenSource(nil, secrets.NewPersistingTokenSource(ctx, oc, tok, tokenFile))
	return New(ctx, option.WithHTTPClient(oauth2.NewClient(ctx, ts)))
}
