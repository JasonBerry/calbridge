// Package caldavfake is an in-memory icloud.CalDAV for syncer tests. It
// models the iCloud behaviour Phase 0 recorded: ETags on PUT, If-Match /
// If-None-Match, UID uniqueness per collection (no-uid-conflict),
// sync-collection with removals, and objects Apple rewrites when the user
// acts (new ETag, extra X-APPLE-* props, changed PARTSTAT).
//
// DECISION (M7): an interface-level fake rather than the httptest CalDAV
// server sketched in PLAN.md §12. The real client's wire behaviour is covered
// by internal/icloud's fixture tests; the syncer only needs the interface
// semantics, which this fake reproduces with far less code.
package caldavfake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/emersion/go-ical"

	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/translate"
)

type object struct {
	raw  []byte
	etag string
	uid  string
}

type change struct {
	seq     int64
	href    string
	removed bool
}

// Fake is one calendar collection. Safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	cal     string
	objects map[string]*object
	changes []change
	seq     int64
	etagN   int

	// Counters of remote calls.
	Puts, Deletes, Gets int
	// DeleteOpts records the options of every DELETE.
	DeleteLog []icloud.DeleteOpts

	// FailNextPut makes the next Put fail with this error before storing.
	FailNextPut error
	// LoseNextPutResponse stores the next Put but returns an error, like a
	// connection lost after the server applied it (crash-after-PUT).
	LoseNextPutResponse bool
	// NoETagOnPut makes Put return an empty ETag (callers must GET).
	NoETagOnPut bool
	// FailNextDelete makes the next Delete fail with this error.
	FailNextDelete error
}

var _ icloud.CalDAV = (*Fake)(nil)

// ErrInjected is returned by injected failures.
var ErrInjected = errors.New("caldavfake: injected failure")

// New returns an empty collection at calendarHref (must end in "/").
func New(calendarHref string) *Fake {
	return &Fake{cal: calendarHref, objects: map[string]*object{}}
}

// CalendarHref returns the collection href.
func (f *Fake) CalendarHref() string { return f.cal }

func (f *Fake) nextETag() string {
	f.etagN++
	return fmt.Sprintf(`"e%d"`, f.etagN)
}

func (f *Fake) record(href string, removed bool) {
	f.seq++
	f.changes = append(f.changes, change{seq: f.seq, href: href, removed: removed})
}

func uidOf(cal *ical.Calendar) string {
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent {
			if p := c.Props.Get(ical.PropUID); p != nil {
				return p.Value
			}
		}
	}
	return ""
}

func parse(raw []byte) (*ical.Calendar, error) {
	return ical.NewDecoder(bytes.NewReader(raw)).Decode()
}

func (f *Fake) inCollection(href string) bool {
	return strings.HasPrefix(href, f.cal) && len(href) > len(f.cal)
}

// Discover implements icloud.CalDAV.
func (f *Fake) Discover(context.Context) (icloud.Discovery, error) {
	return icloud.Discovery{Calendars: []icloud.CalendarInfo{{
		Href: f.cal, DisplayName: "calbridge-test", Components: []string{"VEVENT"}, SupportsSync: true, IsCalendar: true,
	}}}, nil
}

// Get implements icloud.CalDAV.
func (f *Fake) Get(_ context.Context, href string) (*icloud.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Gets++
	o, ok := f.objects[href]
	if !ok {
		return nil, fmt.Errorf("GET %s: %w", href, icloud.ErrNotFound)
	}
	return toObject(href, o)
}

func toObject(href string, o *object) (*icloud.Object, error) {
	cal, err := parse(o.raw)
	if err != nil {
		return nil, err
	}
	return &icloud.Object{Href: href, ETag: o.etag, Cal: cal, Raw: append([]byte(nil), o.raw...)}, nil
}

// MultiGet implements icloud.CalDAV (missing hrefs are left out).
func (f *Fake) MultiGet(_ context.Context, _ string, hrefs []string) ([]icloud.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []icloud.Object
	for _, h := range hrefs {
		f.Gets++
		if o, ok := f.objects[h]; ok {
			obj, err := toObject(h, o)
			if err != nil {
				return nil, err
			}
			out = append(out, *obj)
		}
	}
	return out, nil
}

// Put implements icloud.CalDAV.
func (f *Fake) Put(_ context.Context, href string, cal *ical.Calendar, cond icloud.PutCond) (string, error) {
	raw, err := translate.Encode(cal)
	if err != nil {
		return "", err
	}
	return f.putRaw(href, raw, cond, true)
}

func (f *Fake) putRaw(href string, raw []byte, cond icloud.PutCond, count bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if count {
		f.Puts++
		if err := f.FailNextPut; err != nil {
			f.FailNextPut = nil
			return "", err
		}
	}
	if !f.inCollection(href) {
		return "", fmt.Errorf("PUT %s outside %s: %w", href, f.cal, icloud.ErrNotFound)
	}
	cal, err := parse(raw)
	if err != nil {
		return "", fmt.Errorf("PUT %s: bad body: %w", href, err)
	}
	uid := uidOf(cal)
	cur, exists := f.objects[href]
	switch {
	case cond.IfNoneMatchAny && exists:
		return "", fmt.Errorf("PUT %s: %w", href, icloud.ErrPreconditionFailed)
	case cond.IfMatch != "" && (!exists || icloud.NormalizeETag(cond.IfMatch) != cur.etag):
		return "", fmt.Errorf("PUT %s: %w", href, icloud.ErrPreconditionFailed)
	}
	for h, o := range f.objects {
		if h != href && o.uid == uid {
			return "", fmt.Errorf("PUT %s: UID in use at %s: %w", href, h, icloud.ErrUIDConflict)
		}
	}
	etag := f.nextETag()
	f.objects[href] = &object{raw: raw, etag: etag, uid: uid}
	f.record(href, false)
	if count && f.LoseNextPutResponse {
		f.LoseNextPutResponse = false
		return "", fmt.Errorf("PUT %s: response lost: %w", href, ErrInjected)
	}
	if f.NoETagOnPut {
		return "", nil
	}
	return etag, nil
}

// Delete implements icloud.CalDAV.
func (f *Fake) Delete(_ context.Context, href string, opts icloud.DeleteOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Deletes++
	f.DeleteLog = append(f.DeleteLog, opts)
	if err := f.FailNextDelete; err != nil {
		f.FailNextDelete = nil
		return err
	}
	cur, ok := f.objects[href]
	if !ok {
		return fmt.Errorf("DELETE %s: %w", href, icloud.ErrNotFound)
	}
	if opts.IfMatch != "" && icloud.NormalizeETag(opts.IfMatch) != cur.etag {
		return fmt.Errorf("DELETE %s: %w", href, icloud.ErrPreconditionFailed)
	}
	delete(f.objects, href)
	f.record(href, true)
	return nil
}

// SyncCollection implements icloud.CalDAV. Tokens are "tok-<n>"; an
// unknown or future token returns ErrInvalidSyncToken.
func (f *Fake) SyncCollection(_ context.Context, _ string, token string) (icloud.SyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := icloud.SyncResult{NewToken: "tok-" + strconv.FormatInt(f.seq, 10)}
	if token == "" {
		for _, h := range f.sortedHrefs() {
			res.Changed = append(res.Changed, icloud.Member{Href: h, ETag: f.objects[h].etag})
		}
		return res, nil
	}
	since, err := strconv.ParseInt(strings.TrimPrefix(token, "tok-"), 10, 64)
	if err != nil || since > f.seq || !strings.HasPrefix(token, "tok-") {
		return icloud.SyncResult{}, icloud.ErrInvalidSyncToken
	}
	last := map[string]bool{} // href -> removed?
	var order []string
	for _, c := range f.changes {
		if c.seq <= since {
			continue
		}
		if _, ok := last[c.href]; !ok {
			order = append(order, c.href)
		}
		last[c.href] = c.removed
	}
	for _, h := range order {
		if last[h] {
			res.Removed = append(res.Removed, h)
		} else if o, ok := f.objects[h]; ok {
			res.Changed = append(res.Changed, icloud.Member{Href: h, ETag: o.etag})
		}
	}
	return res, nil
}

// CTag implements icloud.CalDAV.
func (f *Fake) CTag(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "ctag-" + strconv.FormatInt(f.seq, 10), nil
}

// ListETags implements icloud.CalDAV.
func (f *Fake) ListETags(context.Context, string) ([]icloud.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []icloud.Member
	for _, h := range f.sortedHrefs() {
		out = append(out, icloud.Member{Href: h, ETag: f.objects[h].etag})
	}
	return out, nil
}

// FindByUID implements icloud.CalDAV.
func (f *Fake) FindByUID(_ context.Context, _ string, uid string) (*icloud.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.sortedHrefs() {
		if f.objects[h].uid == uid {
			return toObject(h, f.objects[h])
		}
	}
	return nil, icloud.ErrNotFound
}

func (f *Fake) sortedHrefs() []string {
	hs := make([]string, 0, len(f.objects))
	for h := range f.objects {
		hs = append(hs, h)
	}
	sort.Strings(hs)
	return hs
}

// --- test helpers: what the user / Apple does -------------------------------

// Writes is the number of remote PUTs and DELETEs so far.
func (f *Fake) Writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Puts + f.Deletes
}

// Hrefs lists stored object hrefs.
func (f *Fake) Hrefs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sortedHrefs()
}

// Object returns a stored object, or nil.
func (f *Fake) Object(href string) *icloud.Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[href]
	if !ok {
		return nil
	}
	obj, err := toObject(href, o)
	if err != nil {
		panic(err)
	}
	return obj
}

// AppleEdit rewrites an object the way Apple Calendar does when the user
// acts on it: fn mutates the parsed calendar, which is stored with a new
// ETag. It does not count as a calbridge write.
func (f *Fake) AppleEdit(href string, fn func(cal *ical.Calendar)) {
	obj := f.Object(href)
	if obj == nil {
		panic("caldavfake: no object " + href)
	}
	fn(obj.Cal)
	raw, err := translate.Encode(obj.Cal)
	if err != nil {
		panic(err)
	}
	if _, err := f.putRaw(href, raw, icloud.PutCond{}, false); err != nil {
		panic(err)
	}
}

// AppleRSVP sets the PARTSTAT of attendee email on the master VEVENT and
// adds the props Apple adds (P0-4/P0-5).
func (f *Fake) AppleRSVP(href, email, partstat string) {
	f.AppleEdit(href, func(cal *ical.Calendar) {
		for _, c := range cal.Children {
			if c.Name != ical.CompEvent || c.Props.Get(ical.PropRecurrenceID) != nil {
				continue
			}
			atts := c.Props[ical.PropAttendee]
			for i := range atts {
				if strings.EqualFold(strings.TrimPrefix(strings.ToLower(atts[i].Value), "mailto:"), strings.ToLower(email)) {
					atts[i].Params.Set(ical.ParamParticipationStatus, partstat)
					atts[i].Params.Set("CUTYPE", "UNKNOWN")
					atts[i].Params.Set("EMAIL", email)
					atts[i].Params.Del(ical.ParamRSVP)
				}
			}
			p := ical.NewProp("X-APPLE-NEEDS-REPLY")
			p.Value = "TRUE"
			c.Props.Set(p)
		}
	})
}

// UserDelete removes an object as the user deleting it in Apple Calendar would.
func (f *Fake) UserDelete(href string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[href]; !ok {
		panic("caldavfake: no object " + href)
	}
	delete(f.objects, href)
	f.record(href, true)
}

// Import stores raw at href as if the user imported an invite.ics there.
func (f *Fake) Import(href string, raw []byte) {
	if _, err := f.putRaw(href, raw, icloud.PutCond{}, false); err != nil {
		panic(err)
	}
}
