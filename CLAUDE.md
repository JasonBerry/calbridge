# calbridge — CLAUDE.md

Mirrors Google Calendar invitations (hidden Google account user@YOUR_DOMAIN) into an existing
iCloud calendar via CalDAV, and propagates RSVP (PARTSTAT) changes made in Apple Calendar back to
Google via events.patch. Full design: docs/PLAN.md. Phase 0 findings: docs/phase0-results.md
(these override PLAN.md where they differ).

## Commands
Go is pinned in .tool-versions (asdf); if `go` is not found, prepend ~/.asdf/shims to PATH.
- Build:  go build ./...
- Check:  make check        # go vet ./... && go test ./... && golangci-lint run
- Race:   go test -race ./...
- Golden: go test ./internal/translate -update   # only when an intended rendering change is reviewed
- Run:    go run ./cmd/calbridge once --dry-run --config ./dev/config.yaml

## Layout
cmd/calbridge (CLI only) · internal/{config,secrets,googlecal,icloud,translate,store,syncer,server,lock,fakes,spike}
translate is pure (no I/O). syncer depends only on the GoogleCalendar and CalDAV interfaces.

## Rules
1. Never commit secrets: no OAuth client JSON, tokens, app-specific passwords, real email addresses,
   or captured event data with real people's details. Fixtures must be redacted (use YOUR_DOMAIN
   and ORGANIZER_TEST@example.com placeholders, never the real addresses from the table in PLAN.md
   "Project values"). Real addresses belong only in the git-ignored local config. Check
   `git diff --cached` before every commit.
2. Never write to a real calendar without running the same command with --dry-run first and showing
   the plan to the user. Never run anything against the user's main (shared) iCloud calendar unless
   the user explicitly says so in this session. Spike commands only target calendars named calbridge-test*.
3. Never call Google events.patch with sendUpdates=all on a real event except in a user-requested,
   user-supervised manual test.
4. Ask before changing: the RSVP conflict policy (agreed_status / last-change-wins), the UID/href
   strategy, deletion semantics (user_deleted never recreated), privacy defaults, or OAuth scopes.
5. No bulk deletes. Deletion code paths must go through syncer.deleteMirror and be covered by tests.
6. Do not log event titles, descriptions, attendee emails, tokens or passwords at info level or above.
7. Keep CGO_ENABLED=0 buildable (modernc.org/sqlite only; no mattn/go-sqlite3).
8. Every behavioral change needs a test (fake Google + fake CalDAV); idempotency test must stay green
   (second Cycle performs zero remote writes).
9. Mark anything that depends on unverified iCloud/Google behavior with a `// UNVERIFIED:` comment
   referencing the Phase 0 item.
10. Work milestone by milestone (PLAN.md §15); finish each with `make check` green and a short note in
    docs/progress.md.

## Style
Go stdlib first; log/slog; errors wrapped with %w and sentinel errors from googlecal/icloud;
context on every I/O call; table-driven tests; no global state except in main.
