# calbridge

Mirrors Google Calendar invitations sent to a hidden Google account (`user@YOUR_DOMAIN`) into an
existing iCloud calendar over CalDAV, and sends RSVP changes made in Apple Calendar back to the
organizer through the Google Calendar API.

Status: **Phase 0 complete; milestones M0–M11 done.** The sync engine, CLI and status server work;
reconcile (M12), the live trial (M13) and deployment files (M14) are next. Phase 0 results are in
[docs/phase0-results.md](docs/phase0-results.md).

- Design: [docs/PLAN.md](docs/PLAN.md)
- Progress: [docs/progress.md](docs/progress.md)
- Example config: [deploy/config.example.yaml](deploy/config.example.yaml)

## Build

```sh
make check   # build + vet + test + golangci-lint
make bin     # static binary in bin/calbridge
```

## Commands

```sh
calbridge calendars --config CONFIG          # list iCloud calendars; * marks the configured target
calbridge once --dry-run --config CONFIG     # plan one cycle: prints the writes, changes nothing
calbridge once --config CONFIG               # run one cycle (exit 1 on errors)
calbridge serve --config CONFIG              # loop every sync.interval; GET /healthz and /status on server.listen
calbridge status --config CONFIG             # last syncs, mirrors by state, rows retrying or parked
calbridge auth --config CONFIG               # (re)authorize Google
```

Only one `once`/`serve` runs per database (a second exits with code 3). Always run
`once --dry-run` before the first real run against a calendar.

## Scope

Only invitations from Google Calendar organizers are handled; invitations from Outlook, Apple,
Fastmail and others never reach the hidden Google account and are out of scope.

## Responding to and deleting mirrored invitations

- To accept, decline or answer "maybe", use the RSVP buttons in Apple Calendar. calbridge sends
  your answer to the organizer through Google. A declined invitation is hidden in Apple Calendar
  unless "Show Declined Events" is on.
- **Deleting a mirrored invitation does not notify the organizer**, even though Apple Calendar
  labels the button "Delete and Notify". Apple sends nothing, and calbridge does not treat a
  deletion as a decline: it just stops mirroring that event and never recreates it. To turn an
  invitation down, use **Decline** instead.
