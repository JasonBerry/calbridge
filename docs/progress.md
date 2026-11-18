# Progress

One short note per milestone (PLAN.md §15).
- M0 (2026-11-18): scaffold — go.mod (Go 1.27.4 via asdf), CLI dispatch with `version` + stubs, Makefile (`make check`), golangci-lint v2 config, CLAUDE.md, README, example config, .gitignore for secrets/state, docs/PLAN.md.
- M1 (2026-11-18): config (YAML + CALBRIDGE_* env overrides, strict keys, inline-secret rejection, validation), secrets (0600/owner checks, env → systemd credential → file lookup, atomic token file, persisting token source), `calbridge auth` (loopback + PKCE + state, account check via calendarList.get with events.list summary fallback — UNVERIFIED which works under calendar.events). Manual step pending: user runs `calbridge auth` once.
- M2 (2026-11-18): `internal/googlecal` (List/ListAll with 410 → ErrSyncTokenGone, Get, InstanceAt via originalStart, SetSelfResponse sending the full attendees array, sentinel errors, §8.7 backoff), `spike google-list --full|--sync` and `spike google-rsvp` (requires explicit --send-updates and --confirm-organizer unless none). Raw captures go to ./dumps (git-ignored). Manual step pending: user runs P0-1, P0-2.
