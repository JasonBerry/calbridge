# Phase 0 results

Fill this in while running the experiments in PLAN.md §2.2. Findings here override PLAN.md.
Gate to M4 (PLAN.md §2.3): P0-1 pass; P0-2 pass; P0-3 found the target calendar href; P0-4
produced a winning variant **or** the §2.5 fallback is approved; P0-5/P0-7 decisions recorded;
P0-9 delete mode recorded.

## Setup

1. Manual setup in PLAN.md §2.1 (Google account setting "From everyone", OAuth client published
   "In production", app-specific password, unshared `calbridge-test` calendar in iCloud).
2. Local config (git-ignored): `mkdir -p dev && cp deploy/config.example.yaml dev/config.yaml`,
   then set `google.client_file`, `google.token_file`, `self_email`, `icloud.username`,
   `icloud.attendee_email`, `icloud.app_password_file` (all files mode 0600), and
   `icloud.calendar: calbridge-test`.
3. `export CALBRIDGE_CONFIG=$PWD/dev/config.yaml`
4. `go run ./cmd/calbridge auth`

All spike commands write raw captures to `./dumps/` (override with `--dump DIR`, disable with
`--dump ""`). CalDAV write commands refuse any calendar not named `calbridge-test*`.

## Commands per experiment

| ID | Command(s) |
|----|-----------|
| P0-1 | `calbridge spike google-list --full`; after the organizer reschedules: `spike google-list --sync`; after the organizer deletes: `spike google-list --sync` |
| P0-2 | `spike google-rsvp --event ID --status accepted --send-updates all --confirm-organizer ORGANIZER_TEST`; repeat with `tentative`, `declined`, and `--send-updates none`. For `calendar.events.owned`: set `google.scope`, re-run `calbridge auth`, repeat `google-list` and `google-rsvp`. |
| P0-3 | `spike caldav-discover` |
| P0-4 | `spike caldav-put --variant A --organizer ORGANIZER_TEST` (then B, C; `--vtimezone=false` for the no-VTIMEZONE run; D: `--variant D --attendee ADDRESS --schedule-agent X`; E: `--variant E --schedule-agent X`) |
| P0-5 | RSVP in Apple Calendar, then `spike caldav-sync` (shows PARTSTAT, SCHEDULE-STATUS, ETag) and `spike caldav-get --uid UID` |
| P0-6 | Observation only (Mac, iPhone, sharee) |
| P0-7 | `spike caldav-put --from-google EVENT_ID --schedule-agent X`; Google→iCloud PARTSTAT: re-run with `--partstat DECLINED`; reschedule: re-run after the organizer moves the event (updates with If-Match) |
| P0-8 | `spike caldav-put --case tz-novtz --organizer ORGANIZER_TEST` (then `tz-vtz`, `allday`, `recurring`) |
| P0-9 | `spike caldav-delete --href HREF --mode plain` (then `schedule_reply_f`, `neutralize_then_delete`); also delete one in the Apple Calendar UI |
| P0-10 | `spike caldav-volume` (50 PUT + 50 GET + 10 REPORT at 200 ms, plain events, cleans up after itself) |

## Results

| ID | Result (pass/fail/observation) | Decision taken | Config value | Evidence file |
|----|-------------------------------|----------------|--------------|---------------|
| P0-1 | | | | docs/phase0/p0-1-*.json |
| P0-2 | | | `google.send_updates`, `google.scope` | docs/phase0/p0-2-*.json |
| P0-3 | | | `icloud.sync_method`, `icloud.calendar_href` | docs/phase0/p0-3-*.txt |
| P0-4 | | | `icloud.schedule_agent` | docs/phase0/p0-4-*.ics |
| P0-5 | | | `google.send_updates` | |
| P0-6 | | | `sync.declined` | |
| P0-7 | | | `sync.push_google_rsvp_to_icloud` | |
| P0-8 | | | `icloud.emit_vtimezone` | |
| P0-9 | | | `icloud.delete_mode` | |
| P0-10 | | | `icloud.rate_limit_rps` | |

## Decisions

```
schedule_agent: ""|CLIENT|NONE
sync_method: sync-collection|ctag
declined: keep|delete
delete_mode: plain|schedule_reply_f|neutralize_then_delete
send_updates: all|none
emit_vtimezone: true|false
x_prop_preserved: true|false
```

## Notes

- `calbridge auth` verifies the account with `calendarList.get("primary")` and falls back to the
  primary calendar's name from `events.list` if the `calendar.events` scope does not cover
  calendarList. Record which one worked (P0-1).
