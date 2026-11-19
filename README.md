# calbridge

Mirrors Google Calendar invitations sent to a hidden Google account (`user@YOUR_DOMAIN`) into an
existing iCloud calendar over CalDAV, and sends RSVP changes made in Apple Calendar back to the
organizer through the Google Calendar API.

Status: **milestones M0–M14 done.** The live trial on a test calendar passed
([docs/e2e-results-2026-11-19.md](docs/e2e-results-2026-11-19.md)). Phase 0 results are in
[docs/phase0-results.md](docs/phase0-results.md).

- Design: [docs/PLAN.md](docs/PLAN.md)
- Progress: [docs/progress.md](docs/progress.md)
- Example config: [deploy/config.example.yaml](deploy/config.example.yaml)

## Build

Prebuilt static binaries for Linux (amd64, arm64) and macOS (arm64, amd64), with SHA-256 checksums,
are on the [Releases](https://github.com/JasonBerry/calbridge/releases) page. To build from source:

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
calbridge reconcile --config CONFIG          # compare Google, iCloud and the database; prints a plan
calbridge reconcile --apply --config CONFIG  # repair: rebuild mappings, rewrite stale mirrors, create missing ones
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

## Running it

Pick one always-on machine: a Linux server with systemd, or Docker. It needs outbound HTTPS only.

### One-time setup

1. **Google:** a Desktop OAuth client JSON for the Google account that receives the invitations
   (PLAN.md §2.1). Keep the Workspace OAuth consent screen *Internal*, so tokens don't expire after 7 days.
2. **Apple:** an app-specific password for the Apple Account (appleid.apple.com ▸ Sign-In and
   Security ▸ App-Specific Passwords).
3. **Config:** copy `deploy/config.example.yaml` and fill in the addresses and the target calendar.
   Run `calbridge calendars` to see the calendars. If two share a name, set `icloud.calendar_href`.
   Secrets never go in the config file, only paths to them.
4. **Authorize Google** once, as the user the service runs as. The browser step needs to reach the
   machine's loopback, so on a headless server tunnel a fixed port:
   ```sh
   ssh -L 8085:127.0.0.1:8085 server     # from your laptop
   sudo -u calbridge calbridge auth --config /etc/calbridge/config.yaml --port 8085 --no-browser
   ```
   Open the printed URL on the laptop. The token is saved (0600) to `google.token_file`.
5. **Dry run first**, always, and read the plan:
   ```sh
   sudo -u calbridge calbridge once --dry-run --config /etc/calbridge/config.yaml
   ```

### Google Cloud (e2-micro VM)

A step-by-step guide for a locked-down, free-tier VM (about US$3.65/month for its IPv4 address) is in
[deploy/gce.md](deploy/gce.md).

### systemd

`deploy/calbridge.service` contains the install commands at the top. In short: create a `calbridge`
system user, install the binary to `/usr/local/bin`, put the config in `/etc/calbridge/` and the
app password in `/etc/calbridge/icloud-app-password` (root-only; systemd hands it over as a
credential). Then:

```sh
sudo systemctl enable --now calbridge
journalctl -u calbridge -f
```

The state (database, Google token, backups) lives in `/var/lib/calbridge`. Point `google.token_file`,
`store.path` and `store.backup_dir` there.

### Docker

```sh
docker build -f deploy/Dockerfile -t calbridge --build-arg VERSION=$(git describe --always) .
docker run -d --name calbridge --restart unless-stopped -v /srv/calbridge:/data -p 127.0.0.1:8089:8089 calbridge
```

Everything lives in the `/data` volume, so all paths in `/data/config.yaml` must point inside `/data`,
and `server.listen` must be `0.0.0.0:8089`. Run `auth` and the dry run with the same image:
`docker run --rm -it -v /srv/calbridge:/data --network host calbridge auth --config /data/config.yaml --port 8085 --no-browser`.
`.dockerignore` keeps `dev/`, tokens and passwords out of the build.

### Monitoring

- `GET /healthz` returns 200 while the last successful sync is younger than three intervals, and 503
  otherwise (with the error class). `GET /status` returns counts only. Both listen on `server.listen`,
  which defaults to localhost.
- `monitoring.ping_url` (e.g. a healthchecks.io check) is pinged after every successful cycle, and
  `<url>/fail` on an authorization failure, so you hear when syncing stops.
- `calbridge status` shows the last syncs, mirrors by state, and events being retried or parked.
  An event that fails 10 times is parked until it changes again in Google.

### When something breaks

| Symptom | Cause | Fix |
|---|---|---|
| `/healthz` 503, class `google_auth`, logs say `invalid_grant` | Google token revoked or expired | Run `calbridge auth` again (step 4), then restart the service. |
| `/healthz` 503, class `icloud_auth` | Apple app-specific password revoked (changing the Apple Account password revokes them all) | Create a new app-specific password, replace the file, restart. |
| Exit code 3, "another calbridge is running" | A second `serve` or `once` on the same database | Stop the other one. `status` and `calendars` are safe to run alongside. |
| Organizers get your RSVP emails in Spam | Your domain fails DMARC for mail Google sends as you | Turn on Google Workspace DKIM for the domain (Admin console ▸ Gmail ▸ Authenticate email) and add `include:_spf.google.com` to the existing SPF record (edit it; never add a second one). |
| The Mac shows an event the iPhone and icloud.com don't | Mac Calendar's local cache | Check icloud.com first. Quit Calendar and run `killall dataaccessd calaccessd`. Don't delete it on the Mac: that can upload the stale copy back. |
| Something looks out of step | Anything | `calbridge reconcile` prints a plan; `--apply` repairs it. |

### Backups and restore

`serve` snapshots the database daily into `store.backup_dir` (`calbridge-YYYYMMDD.db`, 0600) and
keeps 14. To restore: stop the service, copy a backup over `store.path` (and delete any
`calbridge.db-wal` / `-shm` next to it), start it again, then run `calbridge reconcile` and
`--apply` if it suggests changes. Losing the database entirely is not data loss:
`calbridge reconcile --apply` on an empty database rebuilds the mappings from Google and iCloud
without rewriting mirrors that already match. Mirrors you deleted in Apple can only be told
apart from lost ones with the old database; after a rebuild they come back (or use `--restore`
deliberately). Back up `/etc/calbridge` separately, encrypted: it holds the secrets.

## License

MIT. See [LICENSE](LICENSE).
