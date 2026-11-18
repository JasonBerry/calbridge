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
| P0-1 | **Pass** (2026-11-18). Invitation from a Gmail organizer appeared via incremental sync: `confirmed`, self `needsAction`, iCalUID `<id>@google.com`, explicit `Australia/Sydney` start/end, Meet `conferenceData`+`hangoutLink` auto-added. Organizer reschedule: same id/iCalUID, `sequence` 0→1, self stayed `needsAction`. Deletions (own event, and organizer-deleted invite) arrive as bare tombstones: only `id`, `status: cancelled`, `etag`, `kind` — no iCalUID/start/organizer. A tombstone can arrive for an id never seen as confirmed (created+deleted between syncs). Account check: `calendarList.get` refused under `calendar.events`; the `events.list` fallback worked (primary calendar summary is a display name, not the address). | Map tombstones to mirrors by Google event id from the store; ignore tombstones for unknown ids (M7 test). Seen-then-deleted invite tombstone not separately observed (assumed same shape). | — | local `dumps/` only (not yet redacted) |
| P0-2 | **Pass** (2026-11-18). `events.patch` with the full attendees list: organizer attendee kept (`accepted`), only self changed; `sequence` unchanged by the reply; start and conferenceData untouched. `sendUpdates=all`: organizer guest list showed Yes and organizer got an "Accepted" email (Gmail filed it as spam). `sendUpdates=none` (tentative): organizer guest list showed Maybe, no email. | Keep `send_updates: all` (pending P0-5). `calendar.events.owned` not tested — `calendar.events` works. | `google.send_updates: all`, `google.scope: calendar.events` | local `dumps/` only |
| P0-3 | **Pass** (2026-11-18). Discovery: principal → home on a `pNN-caldav.icloud.com` partition. `calendar-user-address-set` contains the attendee address (7 mailto entries: custom-domain aliases plus me.com/icloud.com). `calbridge-test` found; every calendar advertises sync-collection, sync-token and getctag. Two calendars share the display name "Family" (one VEVENT, one VTODO). | `sync_method: auto` (sync-collection). Configure the real target by `calendar_href`, not display name. | `icloud.sync_method: auto`, `icloud.calendar_href` (target TBD) | local `dumps/caldav` only |
| P0-4 | **Pass** (2026-11-18). A/B/C all: PUT `If-None-Match:*` succeeded, ETag header returned and equal to the GET ETag. Stored VEVENT lines identical to what was sent (ORGANIZER/ATTENDEE incl. SCHEDULE-AGENT param, PARTSTAT, `X-CALBRIDGE-GID` kept; no SCHEDULE-STATUS added). iCloud replaces the sent VTIMEZONE with its own full-history one (+`X-LIC-LOCATION`). No organizer email from any PUT. RSVP buttons / inbox+notification — A: Mac yes, iPhone yes. B (`CLIENT`): Mac yes, iPhone **no** (inbox Mac only). C (`NONE`): Mac yes, iPhone yes. D/E not run. Spike UID has a doubled `p0-` prefix (cosmetic). | **C chosen** (user decision 2026-11-18). A wins by the §2.2 order rule and behaves identically today (P0-5: no iMIP from A or C), but C makes "no scheduling messages" explicit in each mirror, so a future iCloud change to external-organizer replies cannot cause duplicate notifications; `SCHEDULE-AGENT=NONE` survives Apple RSVP edits. Affects only calbridge-written objects. Change detection must hash what calbridge sent, never compare to the stored body. `x_prop_preserved: true`. | `icloud.schedule_agent: NONE` | local `dumps/caldav` only |
| P0-5 | **Pass — no iMIP sent by anyone** (2026-11-18). iPhone Accept on A and C; Mac Maybe then Decline on A. Organizer inbox and spam empty after every step. Each change visible via incremental sync-collection with a new ETag; `SEQUENCE` unchanged; no `SCHEDULE-STATUS`. Apple rewrites the whole object on RSVP: own `PRODID`; ATTENDEE gains `CUTYPE=UNKNOWN` and `EMAIL=`, loses `RSVP=TRUE`; ORGANIZER gains `EMAIL=`; adds `X-APPLE-NEEDS-REPLY:TRUE` (on A **and** C — not evidence of a send), `TRANSP:OPAQUE`, and a default `VALARM` (`X-APPLE-DEFAULT-ALARM`); updates `DTSTAMP`. Kept: `X-CALBRIDGE-GID`, `SCHEDULE-AGENT=NONE` (C). `LAST-MODIFIED` updated on iPhone edits and Mac Decline but **not** on Mac Maybe. | Decision-table row 1: calbridge patches Google with `sendUpdates=all`; organizer gets exactly one notification. Detect RSVP changes by ETag/sync token and read PARTSTAT by attendee address; never use `LAST-MODIFIED`. ContentHash must be computed from calbridge's own rendering, never from the stored object (Apple param/`TRANSP`/`PRODID` rewrites). Updates must preserve or deliberately drop Apple's VALARM (M8 design point). | `google.send_updates: all` | local `dumps/caldav` only |
| P0-6 | **Observation** (2026-11-18). After Decline on the Mac, the event is hidden on both Mac and iPhone unless Calendar's "Show Declined Events" is on. Sharee view not yet checked (deferred to first main-calendar run, per §2.1 step 7). | Keep declined mirrors; Apple already hides them for the user. Revisit if the sharee sees them. | `sync.declined: keep` | — |
| P0-7 | **Pass** (2026-11-18), variant C with UID = Google iCalUID. PUT `If-None-Match:*` succeeded (no UID conflict: nothing else held the UID). Google's tentative status rendered as Maybe on Mac and iPhone; no organizer email; no duplicate; the Siri Suggestion for the emailed invite disappeared once the mirror existed. **Apple → Google is not automatic:** iPhone Accept changed iCloud to ACCEPTED but Google stayed Maybe and no email was sent. Spike `google-rsvp` (as calbridge) then set Google accepted: organizer saw Yes, exactly one email (spam). **Google → iCloud is silent:** Google set declined (`sendUpdates=none`), then an `If-Match` PUT with PARTSTAT=DECLINED: event hidden on both devices, no email. The PUT overwrote Apple's additions (VALARM etc.). **Reschedule:** organizer moved the event; Google bumped `sequence` 1→2 and **reset self from declined to needsAction**. `If-Match` PUT with SEQUENCE 2 and NEEDS-ACTION: visible again at the new time with RSVP buttons, no email from the PUT. The organizer's update email produced a new Siri Suggestion (see Notes). Spike rendering is minimal (no description/location/Meet link) and hard-codes ATTENDEE CN=User. | Google → iCloud PARTSTAT is safe. §8.4 must treat "Google changed from agreed_status, iCloud did not" (e.g. reschedule reset to needsAction) as a Google change to push to iCloud — never as a user RSVP to send back (M10 test). M5: omit CN on the self ATTENDEE unless Google supplies displayName (Google supplied none for organizer or self); include description/location/conference per §7.2/§7.5. M8: decide whether updates preserve Apple's VALARM. | `sync.push_google_rsvp_to_icloud: true` | local `dumps/` only |
| P0-8 | **Pass** (2026-11-18). (1) TZID without VTIMEZONE: accepted; iCloud inserts its own full Australia/Sydney VTIMEZONE; Mac+iPhone show the correct local time. (2) With our VTIMEZONE: iCloud replaces it with the same full one; correct time. (3) All-day `VALUE=DATE` 2 days: stored byte-for-byte unchanged; shown all-day on both days on both devices. (4) Weekly RRULE COUNT=6 across the April DST end, one EXDATE, one moved RECURRENCE-ID override: stored with EXDATE and override intact; iCloud **reorders RRULE parts** (`BYDAY=MO;COUNT=6` → `COUNT=6;BYDAY=MO`); all occurrences correct on both devices, 10:00 stays 10:00 across DST. Mac "Accept only this event" on one occurrence: Apple **adds a new override VEVENT** (RECURRENCE-ID = original start with TZID) with PARTSTAT=ACCEPTED, `CREATED`, `X-APPLE-CREATOR-*`, and a copy of `X-CALBRIDGE-GID`; the master stays NEEDS-ACTION; the existing moved override is untouched apart from Apple's usual additions. Mac kept `RSVP=TRUE` (iPhone had dropped it). | `emit_vtimezone: false` — iCloud supplies the VTIMEZONE, so translate need not generate one (simplifies M6). §8.4 parsing: a per-occurrence RSVP is an override's PARTSTAT → patch the Google instance at that originalStart (`InstanceAt`); master PARTSTAT → the series. putMirror's 412 re-GET merge (§8.2) must carry Apple-created overrides' PARTSTAT, not only the master's. Change detection must not compare RRULE text from iCloud. | `icloud.emit_vtimezone: false` | local `dumps/caldav` only |
| P0-9 | **Pass** (2026-11-18). DELETE with no organizer email and no Google change in every case: plain on a variant C object, plain on a variant A object (no SCHEDULE-AGENT), `Schedule-Reply: F` on a variant B object (header accepted). Apple Calendar UI on the Mac, real-UID mirror (variant C): Apple offered only **"Delete and Notify"** or Cancel; chose Delete and Notify → no email, Google guest list still shows no response, Google incremental sync returned zero changes. All four deletions reported as `removed` by incremental sync-collection. | `delete_mode: plain`. A user deletion in Apple is **silent** despite the "Notify" wording: the organizer is not told. README must say so. calbridge sees it as a sync-collection removal → `user_deleted` per §8.5. **User decision (2026-11-19): keep §8.5** — a deletion is not a decline and Google is not patched; README tells users to use Decline instead (done). | `icloud.delete_mode: plain` | local `dumps/` only |
| P0-10 | **Pass** (2026-11-19). 50 PUT (201×50), 50 GET (200×50), 10 sync-collection REPORT (207×10), 50 cleanup DELETE (204×50), 3 PROPFIND (207×3), 200 ms spacing, sequential, 163 requests in 1 m 57 s. No 429/503, no `Retry-After`, no errors. Latency avg/max: PUT 596/752 ms, GET 378/613 ms, REPORT 442/506 ms, DELETE 587/835 ms, PROPFIND 667/1214 ms. Effective rate ≈1.4 req/s (latency-bound). All test objects removed. | Keep the default client-side limit at 5 req/s, burst 10, and the §8.7 retry policy (429/503 honouring `Retry-After`). The limit was not hit, because sequential requests at ~0.4–0.6 s latency stay well below it, so 5 req/s is untested as a ceiling. Allow ~0.6 s per write when sizing timeouts and the initial backfill. | `icloud.rate_limit_rps: 5` | console output only (no dump; counts only) |

## Decisions

```
schedule_agent: NONE          # P0-4/P0-5; user chose C over A
sync_method: sync-collection  # P0-3
declined: keep                # P0-6 (Apple hides declined events)
delete_mode: plain            # P0-9 (nothing sent in any mode, incl. Apple UI)
send_updates: all             # P0-2/P0-5
emit_vtimezone: false         # P0-8 (iCloud inserts its own VTIMEZONE)
x_prop_preserved: true        # P0-4/P0-5 (also survives Apple RSVP edits)
```

## Notes

- Account type differs from PLAN.md §2.1: the domain is a Google Workspace domain, so the hidden
  account is the Workspace user itself (not a consumer account created with "use my current email
  address"). The OAuth consent screen is **Internal**, on a Cloud project whose parent is the
  domain's organization. That means no unverified-app screen, no "In production" publishing, no
  test users and no 7-day Testing token expiry. If `calbridge auth` reports access blocked, check
  Admin console → Security → API controls (mark the app Trusted) and that the Calendar service is on
  for the user. Invitations from non-Google organizers arrive by email (iMIP), so they reach Google
  Calendar only if the domain's MX records point at Google; Google-organizer invitations do not
  depend on MX.
- `calbridge auth` verifies the account with `calendarList.get("primary")` and falls back to the
  primary calendar's name from `events.list` if the `calendar.events` scope does not cover
  calendarList. Result: the fallback was needed (P0-1).
- Mail for the custom domain is delivered to iCloud, not Google: the "Updated invitation" email for
  the test event landed in iCloud Mail. Consequences: (a) invitations from **non-Google** organizers
  (Outlook, Apple, etc.) are emailed to iCloud and never reach the hidden Google account, so
  calbridge will not mirror them (confirms the PLAN.md §1 non-goal); Google-organizer invitations
  reach it directly. (b) Apple sees
  the iMIP email too: Siri Suggestions offers "add to calendar" for it (local suggestion only,
  nothing written to iCloud unless accepted). P0-7: the suggestion disappeared once the mirror
  existed, and the mirror PUT hit no UID conflict. After the organizer's reschedule email a new
  suggestion appeared (showing the Meet link and description). It did **not** clear on its own
  even though the mirror had the same UID, time and SEQUENCE; it went away only when the email was
  deleted in Apple Mail. Cosmetic and independent of calbridge (Google emails the guest anyway);
  local to the user's devices, not written to iCloud. Risk: tapping Add would create a duplicate.
  Mitigations (user's choice, not calbridge's): ignore/delete handled invitation emails, or turn
  off Siri/Calendar "events found in Mail".
- Google RSVP emails from the Workspace account landed in the organizer's Gmail spam folder (P0-2).
