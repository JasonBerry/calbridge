# calbridge

Mirrors Google Calendar invitations sent to a hidden Google account (`user@YOUR_DOMAIN`) into an
existing iCloud calendar over CalDAV, and sends RSVP changes made in Apple Calendar back to the
organizer through the Google Calendar API.

Status: **Phase 0 (de-risking).** Only the `auth` command and the hidden `spike …` experiment
commands work so far. The sync engine is built after Phase 0 results are recorded in
[docs/phase0-results.md](docs/phase0-results.md).

- Design: [docs/PLAN.md](docs/PLAN.md)
- Progress: [docs/progress.md](docs/progress.md)
- Example config: [deploy/config.example.yaml](deploy/config.example.yaml)

## Build

```sh
make check   # build + vet + test + golangci-lint
make bin     # static binary in bin/calbridge
```

## Scope

Only invitations from Google Calendar organizers are handled; invitations from Outlook, Apple,
Fastmail and others never reach the hidden Google account and are out of scope.
