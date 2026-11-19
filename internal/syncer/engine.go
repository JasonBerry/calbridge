// Package syncer runs calbridge's sync cycle (PLAN.md §8). It depends only on
// the googlecal.GoogleCalendar and icloud.CalDAV interfaces and the store.
//
// M7 implements Google → iCloud for non-recurring events: create, update,
// cancel/delete, 410 full resync, crash recovery (adopt), UID-conflict
// adoption, user deletions detected on GET, and dry-run. Recurring series
// are recorded as skipped until M9/M10; iCloud → Google RSVP is M8.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/JasonBerry/calbridge/internal/googlecal"
	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/store"
	"github.com/JasonBerry/calbridge/internal/translate"
)

// Clock supplies the current time.
type Clock interface{ Now() time.Time }

// SystemClock is the real clock.
type SystemClock struct{}

// Now returns time.Now().
func (SystemClock) Now() time.Time { return time.Now() }

// Delete modes (Phase 0 P0-9 chose plain).
const (
	DeleteModePlain          = "plain"
	DeleteModeScheduleReplyF = "schedule_reply_f"
)

// Options configure an Engine (from config.Config).
type Options struct {
	GoogleCalendarID     string
	CalendarHref         string // iCloud collection href
	SelfEmail            string // icloud.attendee_email: the user's attendee address in iCloud
	ScheduleAgent        string
	Privacy              translate.PrivacyMode
	Attendees            translate.AttendeeMode
	DefaultAlarms        []time.Duration
	EmitVTimezone        bool
	Lookback             time.Duration
	IncludeOrganizedByMe bool
	DeleteMode           string
	RecreateOnReschedule bool
	// SendUpdates is Google's sendUpdates for RSVP patches (P0-2/P0-5: all).
	SendUpdates string
	// NoGoogleRSVPToICloud disables writing Google-side response changes to
	// the mirror (sync.push_google_rsvp_to_icloud: false).
	NoGoogleRSVPToICloud bool
	// DeclinedDelete removes the mirror once the user has declined
	// (sync.declined: delete).
	DeclinedDelete bool
	// MaxFullResyncDeletes caps how many mirrors one full resync may delete
	// (CLAUDE.md rule 5: no bulk deletes). 0 means the default of 5.
	MaxFullResyncDeletes int
}

// Validate checks the options.
func (o Options) Validate() error {
	var bad []string
	if o.GoogleCalendarID == "" {
		bad = append(bad, "GoogleCalendarID is empty")
	}
	if !strings.HasSuffix(o.CalendarHref, "/") {
		bad = append(bad, "CalendarHref must end in /")
	}
	if !strings.Contains(o.SelfEmail, "@") {
		bad = append(bad, "SelfEmail is not an address")
	}
	switch o.SendUpdates {
	case "", "all", "externalOnly", "none":
	default:
		bad = append(bad, fmt.Sprintf("SendUpdates %q is not all|externalOnly|none", o.SendUpdates))
	}
	switch o.DeleteMode {
	case "", DeleteModePlain, DeleteModeScheduleReplyF:
	default:
		bad = append(bad, fmt.Sprintf("DeleteMode %q is not supported", o.DeleteMode))
	}
	if len(bad) > 0 {
		return fmt.Errorf("syncer: invalid options: %s", strings.Join(bad, "; "))
	}
	return nil
}

// Engine runs sync cycles.
type Engine struct {
	G      googlecal.GoogleCalendar
	C      icloud.CalDAV
	S      *store.Store
	Opts   Options
	Clock  Clock
	Log    *slog.Logger
	DryRun bool
}

// PlannedOp is one remote write a cycle made (or, in dry-run, would make).
// It carries ids only, never event content (CLAUDE.md rule 6).
type PlannedOp struct {
	Op       string // put_create | put_update | adopt | delete | patch_rsvp
	GoogleID string
	Href     string
}

// Report summarises a cycle.
type Report struct {
	Created, Updated, Adopted, Deleted, Unchanged, Skipped, UserDeleted, Failed int
	RSVPs                                                                       int // responses sent to Google
	FullResync                                                                  bool
	ErrorClass                                                                  string // of the cycle's error, if any
	Ops                                                                         []PlannedOp
	Errors                                                                      []error
}

// Writes is the number of remote writes in the report.
func (r Report) Writes() int { return len(r.Ops) }

var (
	// ErrTooManyDeletes stops a full resync that would delete more mirrors
	// than Options.MaxFullResyncDeletes.
	ErrTooManyDeletes = errors.New("syncer: full resync would delete too many mirrors; refusing")
	errRollback       = errors.New("syncer: dry-run rollback")
)

// Cycle runs one sync cycle (§8.1). With DryRun, store changes are rolled
// back and remote writes are recorded in Report.Ops instead of sent.
func (e *Engine) Cycle(ctx context.Context) (Report, error) {
	if err := e.Opts.Validate(); err != nil {
		return Report{}, err
	}
	if !e.DryRun {
		r := e.newRun(&e.S.Queries)
		err := r.cycle(ctx)
		return *r.rep, err
	}
	var (
		rep    Report
		cycErr error
	)
	err := e.S.Tx(ctx, func(tx *store.Tx) error {
		r := e.newRun(&tx.Queries)
		r.c = &dryCalDAV{CalDAV: e.C}
		r.g = &dryGoogle{GoogleCalendar: e.G}
		cycErr = r.cycle(ctx)
		rep = *r.rep
		return errRollback
	})
	if errors.Is(err, errRollback) {
		err = cycErr
	}
	return rep, err
}

// run is the state of one cycle.
type run struct {
	e   *Engine
	q   *store.Queries
	c   icloud.CalDAV
	g   googlecal.GoogleCalendar
	rep *Report
	log *slog.Logger
	now time.Time
	// soft collects failures whose work is durably queued: they are
	// reported but do not hold back the sync tokens.
	soft []error
	// touched lists series whose Google side changed because of a push in
	// this cycle (a per-occurrence RSVP creates an exception).
	touched map[string]bool
}

func (e *Engine) newRun(q *store.Queries) *run {
	clock := e.Clock
	if clock == nil {
		clock = SystemClock{}
	}
	log := e.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if e.DryRun {
		log = log.With("dry_run", true)
	}
	return &run{e: e, q: q, c: e.C, g: e.G, rep: &Report{}, log: log, now: clock.Now().UTC(), touched: map[string]bool{}}
}

func (r *run) cycle(ctx context.Context) error {
	// §8.1: Google first, so the iCloud pass sees the ETags of our own PUTs
	// and skips them (loop prevention, §7.10).
	err := r.retryPending(ctx)
	if !IsAuth(err) && ctx.Err() == nil {
		err = errors.Join(err, r.googleToICloud(ctx))
	}
	if !IsAuth(err) && ctx.Err() == nil {
		err = errors.Join(err, r.iCloudToGoogle(ctx))
	}
	err = errors.Join(append([]error{err}, r.soft...)...)
	if err != nil {
		r.rep.Errors = append(r.rep.Errors, err)
		r.rep.ErrorClass = ErrorClass(err)
	}
	return err
}

func (r *run) opts() Options { return r.e.Opts }

func (r *run) logOp(ctx context.Context, op string, m *store.Mapping) {
	r.rep.Ops = append(r.rep.Ops, PlannedOp{Op: op, GoogleID: m.GoogleEventID, Href: m.ICloudHref})
	r.log.DebugContext(ctx, "remote write", "op", op, "google_id", m.GoogleEventID, "href", m.ICloudHref)
	if err := r.q.AppendOp(ctx, &store.Op{Direction: store.DirG2I, Op: op, MappingID: m.ID}); err != nil {
		r.log.WarnContext(ctx, "ops_log append failed", "err", err)
	}
}

// dryCalDAV records writes instead of sending them; reads pass through.
type dryCalDAV struct {
	icloud.CalDAV
	n       int
	written map[string]bool // hrefs "written" in this dry run
}

func (d *dryCalDAV) Put(_ context.Context, href string, _ *ical.Calendar, _ icloud.PutCond) (string, error) {
	d.n++
	if d.written == nil {
		d.written = map[string]bool{}
	}
	d.written[href] = true
	return fmt.Sprintf(`"dry-run-%d"`, d.n), nil
}

func (d *dryCalDAV) Delete(_ context.Context, href string, _ icloud.DeleteOpts) error {
	if d.written == nil {
		d.written = map[string]bool{}
	}
	d.written[href] = true
	return nil
}

// dryWritten reports whether href was only pretend-written in this dry run:
// the real iCloud does not reflect it, so the iCloud pass must ignore it.
func (r *run) dryWritten(href string) bool {
	d, ok := r.c.(*dryCalDAV)
	return ok && d.written[href]
}
