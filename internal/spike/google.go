package spike

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/googlecal"
)

const googleTokenStateFile = "google-sync-token" // #nosec G101 -- file name, not a credential

// googleList implements P0-1: full or incremental events.list, printing a
// summary and dumping the raw JSON.
func googleList(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "google-list")
	var c common
	c.register(fs)
	full := fs.Bool("full", false, "full sync (timeMin = now - lookback); stores the nextSyncToken")
	incremental := fs.Bool("sync", false, "incremental sync with the stored token; stores the new token")
	lookback := fs.Duration("lookback", 0, "override sync.lookback for --full")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *full == *incremental {
		_, _ = fmt.Fprintln(e.stderr, "exactly one of --full or --sync is required")
		return ErrUsage
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	g, err := googlecal.NewAuthorized(ctx, cfg.Google.ClientFile, cfg.Google.TokenFile, cfg.Google.Scope, c.insecurePerms)
	if err != nil {
		return err
	}
	req := googlecal.ListRequest{CalendarID: cfg.Google.CalendarID}
	tokenPath := filepath.Join(c.dumpDir, googleTokenStateFile)
	if *full {
		lb := cfg.Sync.Lookback.D()
		if *lookback > 0 {
			lb = *lookback
		}
		req.TimeMin = time.Now().Add(-lb)
	} else {
		b, err := os.ReadFile(tokenPath) // #nosec G304 -- spike state file in the dump dir
		if err != nil {
			return fmt.Errorf("no stored sync token (run --full first): %w", err)
		}
		req.SyncToken = strings.TrimSpace(string(b))
	}
	items, next, err := googlecal.ListAll(ctx, g, req)
	if errors.Is(err, googlecal.ErrSyncTokenGone) {
		_, _ = fmt.Fprintln(e.stderr, "sync token gone (410): run --full again")
	}
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	mode := "sync"
	if *full {
		mode = "full"
	}
	dump, err := writeDump(c.dumpDir, fmt.Sprintf("google-list-%s-%s.json", mode, stamp()), raw)
	if err != nil {
		return err
	}
	if _, err := writeDump(c.dumpDir, googleTokenStateFile, []byte(next+"\n")); err != nil {
		return err
	}
	printEvents(e, items)
	_, _ = fmt.Fprintf(e.stdout, "\n%d item(s). Raw JSON: %s\nnextSyncToken stored in %s\n", len(items), dump, tokenPath)
	return nil
}

func printEvents(e *env, items []*calendar.Event) {
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATUS\tSEQ\tSTART\tSELF\tORGANIZER\tRECURRING_ID\tICALUID\tSUMMARY")
	for _, ev := range items {
		self := "-"
		if a := googlecal.SelfAttendee(ev); a != nil {
			self = a.ResponseStatus
		}
		org := ""
		if ev.Organizer != nil {
			org = ev.Organizer.Email
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			ev.Id, ev.Status, ev.Sequence, startString(ev), self, org, ev.RecurringEventId, ev.ICalUID, ev.Summary)
	}
	_ = tw.Flush()
}

func startString(ev *calendar.Event) string {
	if ev.Start == nil {
		return ""
	}
	if ev.Start.Date != "" {
		return ev.Start.Date
	}
	if ev.Start.TimeZone != "" {
		return ev.Start.DateTime + " " + ev.Start.TimeZone
	}
	return ev.Start.DateTime
}

// googleRSVP implements P0-2. Because a non-"none" sendUpdates emails a real
// organizer (CLAUDE.md rule 3), it requires --send-updates explicitly and
// --confirm-organizer matching the event's organizer.
func googleRSVP(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "google-rsvp")
	var c common
	c.register(fs)
	eventID := fs.String("event", "", "Google event id (required)")
	status := fs.String("status", "", "accepted | tentative | declined | needsAction (required)")
	sendUpdates := fs.String("send-updates", "", "all | externalOnly | none (required; no default on purpose)")
	confirm := fs.String("confirm-organizer", "", "organizer email; required unless --send-updates none")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *eventID == "" || *status == "" || *sendUpdates == "" {
		_, _ = fmt.Fprintln(e.stderr, "--event, --status and --send-updates are required")
		return ErrUsage
	}
	if !googlecal.ValidResponseStatus(*status) {
		return fmt.Errorf("invalid --status %q", *status)
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	g, err := googlecal.NewAuthorized(ctx, cfg.Google.ClientFile, cfg.Google.TokenFile, cfg.Google.Scope, c.insecurePerms)
	if err != nil {
		return err
	}
	ev, err := g.Get(ctx, cfg.Google.CalendarID, *eventID)
	if err != nil {
		return err
	}
	if err := checkOrganizer(ev, *sendUpdates, *confirm); err != nil {
		return err
	}
	before := "-"
	if a := googlecal.SelfAttendee(ev); a != nil {
		before = a.ResponseStatus
	}
	out, err := g.SetSelfResponse(ctx, cfg.Google.CalendarID, *eventID, *status, *sendUpdates)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	dump, err := writeDump(c.dumpDir, fmt.Sprintf("google-rsvp-%s-%s.json", *status, stamp()), raw)
	if err != nil {
		return err
	}
	after := "-"
	if a := googlecal.SelfAttendee(out); a != nil {
		after = a.ResponseStatus
	}
	_, _ = fmt.Fprintf(e.stdout, "responseStatus %s -> %s (sendUpdates=%s). Response JSON: %s\n", before, after, *sendUpdates, dump)
	_, _ = fmt.Fprintln(e.stdout, "Now record what the organizer sees (guest list, inbox) in docs/phase0-results.md.")
	return nil
}

func checkOrganizer(ev *calendar.Event, sendUpdates, confirm string) error {
	if sendUpdates == "none" {
		return nil
	}
	org := ""
	if ev.Organizer != nil {
		org = ev.Organizer.Email
	}
	if confirm == "" || !strings.EqualFold(confirm, org) {
		return fmt.Errorf("--send-updates %s would notify the organizer %q; pass --confirm-organizer %s to proceed (test organizers only)", sendUpdates, org, org)
	}
	return nil
}
