package icloud

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/emersion/go-ical"

	"github.com/JasonBerry/calbridge/internal/translate"
)

func xmlHeader(depth string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", `application/xml; charset="utf-8"`)
	if depth != "" {
		h.Set("Depth", depth)
	}
	return h
}

// multistatusReq sends PROPFIND/REPORT and parses a 207 response.
func (c *Client) multistatusReq(ctx context.Context, method, href, depth, body string) (*multistatus, error) {
	r, err := c.do(ctx, method, href, xmlHeader(depth), []byte(body))
	if err != nil {
		return nil, err
	}
	if r.status != http.StatusMultiStatus {
		target, _ := c.resolve(href)
		return nil, httpErr(method, target, r)
	}
	return parseMultistatus(r.body)
}

// Discover walks / → principal → calendar home → calendars, switching the
// client's base to the partition host the home lives on.
func (c *Client) Discover(ctx context.Context) (Discovery, error) {
	var d Discovery
	ms, err := c.multistatusReq(ctx, "PROPFIND", c.base.String(), "0", propfindPrincipal)
	if err != nil {
		return d, fmt.Errorf("discover principal: %w", err)
	}
	for _, r := range ms.Responses {
		p := r.okProp()
		if h := firstHref(p.CurrentUserPrincipal); h != "" {
			d.Principal = h
		}
	}
	if d.Principal == "" {
		return d, errors.New("caldav: no current-user-principal")
	}

	ms, err = c.multistatusReq(ctx, "PROPFIND", d.Principal, "0", propfindHome)
	if err != nil {
		return d, fmt.Errorf("discover home: %w", err)
	}
	for _, r := range ms.Responses {
		p := r.okProp()
		if h := firstHref(p.CalendarHomeSet); h != "" {
			d.CalendarHome = h
		}
		if p.AddressSet != nil {
			for _, a := range p.AddressSet.Hrefs {
				d.AddressSet = append(d.AddressSet, strings.TrimSpace(a))
			}
		}
		d.ScheduleInbox = firstHref(p.ScheduleInbox)
		d.ScheduleOutbox = firstHref(p.ScheduleOutbox)
	}
	if d.CalendarHome == "" {
		return d, errors.New("caldav: no calendar-home-set")
	}
	// The home is usually an absolute URL on pNN-caldav.icloud.com.
	homeURL, err := c.resolve(d.CalendarHome)
	if err != nil {
		return d, err
	}
	hu, _ := url.Parse(homeURL)
	c.base = &url.URL{Scheme: hu.Scheme, Host: hu.Host, Path: "/"}
	d.BaseURL = c.base.String()
	d.CalendarHome = hu.Path

	ms, err = c.multistatusReq(ctx, "PROPFIND", d.CalendarHome, "1", propfindCalendars)
	if err != nil {
		return d, fmt.Errorf("list calendars: %w", err)
	}
	for _, r := range ms.Responses {
		href := pathOf(r.href())
		if samePath(href, d.CalendarHome) {
			continue
		}
		p := r.okProp()
		ci := CalendarInfo{
			Href:        href,
			DisplayName: p.DisplayName,
			CTag:        p.CTag,
			SyncToken:   p.SyncToken,
			IsCalendar:  p.isCalendar(),
		}
		if p.SupportedComponents != nil {
			for _, comp := range p.SupportedComponents.Comps {
				ci.Components = append(ci.Components, comp.Name)
			}
		}
		if p.SupportedReportSet != nil {
			for _, rep := range p.SupportedReportSet.Reports {
				for _, n := range rep.Report.names() {
					if n == "sync-collection" {
						ci.SupportsSync = true
					}
				}
			}
		}
		if p.PrivilegeSet != nil {
			for _, pr := range p.PrivilegeSet.Privileges {
				ci.Privileges = append(ci.Privileges, pr.names()...)
			}
		}
		d.Calendars = append(d.Calendars, ci)
	}
	return d, nil
}

// FindCalendar returns the calendar with the given display name (exact match).
func (d Discovery) FindCalendar(name string) (CalendarInfo, bool) {
	for _, ci := range d.Calendars {
		if ci.IsCalendar && ci.DisplayName == name {
			return ci, true
		}
	}
	return CalendarInfo{}, false
}

// HasAddress reports whether the address set contains mailto:email (case-insensitive).
func (d Discovery) HasAddress(email string) bool {
	for _, a := range d.AddressSet {
		if strings.EqualFold(strings.TrimPrefix(strings.TrimPrefix(a, "mailto:"), "MAILTO:"), email) {
			return true
		}
	}
	return false
}

// Get fetches one object. It ignores the response Content-Type, which iCloud
// has been seen to send as text/plain (go-webdav #131).
func (c *Client) Get(ctx context.Context, href string) (*Object, error) {
	r, err := c.do(ctx, http.MethodGet, href, nil, nil)
	if err != nil {
		return nil, err
	}
	if r.status != http.StatusOK {
		target, _ := c.resolve(href)
		return nil, httpErr(http.MethodGet, target, r)
	}
	o := &Object{Href: pathOf(href), ETag: NormalizeETag(r.header.Get("ETag")), Raw: r.body}
	o.Cal, _ = decode(r.body)
	return o, nil
}

func decode(b []byte) (*ical.Calendar, error) {
	return ical.NewDecoder(bytes.NewReader(b)).Decode()
}

// MultiGet fetches objects via calendar-multiget in batches of 50. Hrefs the
// server reports as missing are omitted.
func (c *Client) MultiGet(ctx context.Context, calendarHref string, hrefs []string) ([]Object, error) {
	var out []Object
	for start := 0; start < len(hrefs); start += 50 {
		end := min(start+50, len(hrefs))
		ms, err := c.multistatusReq(ctx, "REPORT", calendarHref, "1", multigetBody(hrefs[start:end]))
		if err != nil {
			return nil, err
		}
		out = append(out, objectsFrom(ms)...)
	}
	return out, nil
}

func objectsFrom(ms *multistatus) []Object {
	var out []Object
	for _, r := range ms.Responses {
		if c := statusCode(r.Status); c == http.StatusNotFound {
			continue
		}
		p := r.okProp()
		if p.CalendarData == "" {
			continue
		}
		o := Object{Href: pathOf(r.href()), ETag: NormalizeETag(p.GetETag), Raw: []byte(p.CalendarData)}
		o.Cal, _ = decode(o.Raw)
		out = append(out, o)
	}
	return out
}

// Put encodes cal and PUTs it.
func (c *Client) Put(ctx context.Context, href string, cal *ical.Calendar, cond PutCond) (string, error) {
	body, err := translate.Encode(cal) // folds long lines (go-ical does not)
	if err != nil {
		return "", fmt.Errorf("caldav: encode: %w", err)
	}
	return c.PutRaw(ctx, href, body, cond)
}

// PutRaw PUTs a serialized VCALENDAR. It returns the ETag header, which may
// be empty: callers then GET the object (UNVERIFIED: P0-4 whether iCloud
// returns ETag on PUT).
func (c *Client) PutRaw(ctx context.Context, href string, body []byte, cond PutCond) (string, error) {
	h := http.Header{}
	h.Set("Content-Type", "text/calendar; charset=utf-8")
	if cond.IfNoneMatchAny {
		h.Set("If-None-Match", "*")
	}
	if cond.IfMatch != "" {
		h.Set("If-Match", NormalizeETag(cond.IfMatch))
	}
	r, err := c.do(ctx, http.MethodPut, href, h, body)
	if err != nil {
		return "", err
	}
	switch r.status {
	case http.StatusCreated, http.StatusNoContent, http.StatusOK:
		return NormalizeETag(r.header.Get("ETag")), nil
	}
	target, _ := c.resolve(href)
	herr := httpErr(http.MethodPut, target, r)
	// RFC 4791 §5.3.2.1: CALDAV:no-uid-conflict precondition, 403 or 409.
	// UNVERIFIED: P0-4 which status iCloud uses.
	if (r.status == http.StatusForbidden || r.status == http.StatusConflict) && bytes.Contains(r.body, []byte("no-uid-conflict")) {
		return "", fmt.Errorf("%w: %v", ErrUIDConflict, herr)
	}
	return "", herr
}

// Delete removes an object. A missing object returns ErrNotFound; callers
// that only need it gone treat that as success.
func (c *Client) Delete(ctx context.Context, href string, opts DeleteOpts) error {
	h := http.Header{}
	if opts.IfMatch != "" {
		h.Set("If-Match", NormalizeETag(opts.IfMatch))
	}
	if opts.ScheduleReply != nil {
		// UNVERIFIED: P0-9 — iCloud's handling of Schedule-Reply.
		if *opts.ScheduleReply {
			h.Set("Schedule-Reply", "T")
		} else {
			h.Set("Schedule-Reply", "F")
		}
	}
	r, err := c.do(ctx, http.MethodDelete, href, h, nil)
	if err != nil {
		return err
	}
	switch r.status {
	case http.StatusOK, http.StatusNoContent, http.StatusAccepted:
		return nil
	}
	target, _ := c.resolve(href)
	return httpErr(http.MethodDelete, target, r)
}

// SyncCollection runs an RFC 6578 sync-collection REPORT. An empty token is
// an initial sync (all members). It follows 507 truncation.
// UNVERIFIED: P0-3 — iCloud support for sync-collection.
func (c *Client) SyncCollection(ctx context.Context, calendarHref, token string) (SyncResult, error) {
	var res SyncResult
	cal := pathOf(calendarHref)
	for round := 0; round < 20; round++ {
		r, err := c.do(ctx, "REPORT", calendarHref, xmlHeader("0"), []byte(syncCollectionBody(token)))
		if err != nil {
			return res, err
		}
		if r.status != http.StatusMultiStatus {
			target, _ := c.resolve(calendarHref)
			herr := httpErr("REPORT", target, r)
			switch {
			case bytes.Contains(r.body, []byte("valid-sync-token")):
				return res, fmt.Errorf("%w: %v", ErrInvalidSyncToken, herr)
			case r.status == http.StatusNotImplemented || r.status == http.StatusBadRequest ||
				bytes.Contains(r.body, []byte("supported-report")):
				return res, fmt.Errorf("%w: %v", ErrSyncUnsupported, herr)
			}
			return res, herr
		}
		ms, err := parseMultistatus(r.body)
		if err != nil {
			return res, err
		}
		truncated := false
		for _, resp := range ms.Responses {
			href := pathOf(resp.href())
			st := statusCode(resp.Status)
			if samePath(href, cal) {
				if st == http.StatusInsufficientStorage {
					truncated = true
				}
				continue
			}
			if st == http.StatusNotFound {
				res.Removed = append(res.Removed, href)
				continue
			}
			p := resp.okProp()
			res.Changed = append(res.Changed, Member{Href: href, ETag: NormalizeETag(p.GetETag)})
		}
		res.NewToken = strings.TrimSpace(ms.SyncToken)
		if !truncated {
			return res, nil
		}
		token = res.NewToken
	}
	return res, errors.New("caldav: sync-collection still truncated after 20 rounds")
}

// CTag returns the CalendarServer getctag of a calendar.
func (c *Client) CTag(ctx context.Context, calendarHref string) (string, error) {
	ms, err := c.multistatusReq(ctx, "PROPFIND", calendarHref, "0", propfindCTag)
	if err != nil {
		return "", err
	}
	for _, r := range ms.Responses {
		if p := r.okProp(); p.CTag != "" {
			return p.CTag, nil
		}
	}
	return "", errors.New("caldav: no getctag")
}

// ListETags lists all members of a calendar with their ETags (ctag fallback path).
func (c *Client) ListETags(ctx context.Context, calendarHref string) ([]Member, error) {
	ms, err := c.multistatusReq(ctx, "PROPFIND", calendarHref, "1", propfindETags)
	if err != nil {
		return nil, err
	}
	cal := pathOf(calendarHref)
	var out []Member
	for _, r := range ms.Responses {
		href := pathOf(r.href())
		p := r.okProp()
		if samePath(href, cal) || p.isCollection() {
			continue
		}
		out = append(out, Member{Href: href, ETag: NormalizeETag(p.GetETag)})
	}
	return out, nil
}

// FindByUID locates the object with the given UID: calendar-query with a UID
// text-match first, then a ListETags + MultiGet scan.
// UNVERIFIED: P0-4 — iCloud support for UID prop-filter.
func (c *Client) FindByUID(ctx context.Context, calendarHref, uid string) (*Object, error) {
	ms, err := c.multistatusReq(ctx, "REPORT", calendarHref, "1", uidQueryBody(uid))
	if err == nil {
		for _, o := range objectsFrom(ms) {
			if objectUID(o.Cal) == uid {
				return &o, nil
			}
		}
	} else if errors.Is(err, ErrAuth) {
		return nil, err
	}
	members, err := c.ListETags(ctx, calendarHref)
	if err != nil {
		return nil, err
	}
	hrefs := make([]string, len(members))
	for i, m := range members {
		hrefs[i] = m.Href
	}
	objs, err := c.MultiGet(ctx, calendarHref, hrefs)
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		if objectUID(o.Cal) == uid {
			return &o, nil
		}
	}
	return nil, ErrNotFound
}

func objectUID(cal *ical.Calendar) string {
	if cal == nil {
		return ""
	}
	for _, child := range cal.Children {
		if child.Name == ical.CompEvent {
			if p := child.Props.Get(ical.PropUID); p != nil {
				return p.Value
			}
		}
	}
	return ""
}

// pathOf returns the unescaped path of an href or absolute URL.
func pathOf(href string) string {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return href
	}
	return u.Path
}

func samePath(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}
