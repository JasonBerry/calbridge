# calbridge live trial (M13)

A hands-on checklist for running calbridge against the real accounts. Run it on the
**`calbridge-test`** calendar only. The main (shared) calendar comes last and only as a
dry run (step 9).

- `ORGANIZER_TEST` is the personal Gmail used to send test invitations (PLAN.md "Project values").
- `user@YOUR_DOMAIN` is the hidden Google account, which is also the attendee address in iCloud.
- "One cycle" is `sync.interval`. For the trial, set it to 30 s so you aren't waiting (step 1).
- After each step, note **pass / fail** and anything odd in the results table at the end. If a step
  fails, stop and paste the output of `bin/calbridge status` and the last lines of the serve log.
- These tests send real RSVPs to Google with `send_updates: all`. The organizer is you (the test
  Gmail), so this is the user-supervised test that CLAUDE.md rule 3 allows.

Where to look for emails: the `ORGANIZER_TEST` inbox **and its Spam folder**, and iCloud Mail.
Google's own invitation and RSVP emails are expected. Anything sent by iCloud/Apple is not.

---

## 0. Before you start

1. Build the binary:
   ```sh
   make bin
   ```
2. Check `dev/config.yaml`. It should have: `icloud.calendar: calbridge-test`,
   `schedule_agent: NONE`, `privacy: full`, `attendees: all`, `send_updates: all`,
   `declined: keep`, `emit_vtimezone: false`, `delete_mode: plain`.
3. Clean up the Phase 0 leftovers:
   - In the `ORGANIZER_TEST` Google Calendar, **delete the old "Testing" event** so the trial
     starts clean.
   - See what is left in calbridge-test (this only prints a plan):
     ```sh
     bin/calbridge reconcile --config dev/config.yaml
     ```
     Expect `orphan 5`: the Phase 0 test events. Delete them:
     ```sh
     bin/calbridge reconcile --apply --delete-orphans --config dev/config.yaml
     ```
     **Expect:** the five Phase 0 test events disappear from calbridge-test on both devices. No emails.

## 1. Dry run, then start the service

1. ```sh
   bin/calbridge once --dry-run --config dev/config.yaml
   ```
   **Expect:** `nothing was written`, and a planned-writes table that is empty or lists only
   invitations you recognise.
2. Start the service in its own terminal tab. Use a 30 s interval for the trial (fish syntax):
   ```sh
   env CALBRIDGE_SYNC_INTERVAL=30s bin/calbridge serve --config dev/config.yaml
   ```
3. In another tab:
   ```sh
   curl -s 127.0.0.1:8089/healthz
   ```
   ```sh
   curl -s 127.0.0.1:8089/status
   ```
   **Expect:** `ok` (or `starting` before the first cycle finishes), and JSON with counts only (no titles).
4. Check that a second copy refuses to run:
   ```sh
   bin/calbridge once --config dev/config.yaml
   ```
   **Expect:** "another calbridge is running", exit code 3.

## 2. A single invitation

**T1. New invitation.** From `ORGANIZER_TEST`, create an event for later this week. Give it a
title, a location, a short description and a Google Meet link. Invite `user@YOUR_DOMAIN` plus one
extra guest (a plus-address of `ORGANIZER_TEST`, e.g. `name+guest1@…`).
- **Expect** within one cycle, on Mac and iPhone:
  - The event is in calbridge-test.
  - Title, time, location and description are right, and the Meet link and dial-in are at the top of the notes.
  - The organizer is shown. The invitees list shows you and the extra guest, with Google's statuses.
  - Accept / Maybe / Decline buttons are shown.
- **Expect:** no Siri Suggestion duplicate once the mirror exists. A suggestion from the invitation
  email can linger until that email is deleted (P0-7, cosmetic).

**T2. Accept on iPhone.**
- **Expect** within one cycle:
  - The organizer's Google Calendar shows you as **Accepted**.
  - **Exactly one** acceptance email from Google reaches the organizer (maybe in Spam).
  - The mirror stays Accepted, with no flicker back to "needs action".
  - No email from iCloud.

**T3. Organizer edits the description and location only.**
- **Expect:** the mirror shows the changes and still says **Accepted**. No new email to you beyond Google's own update email.

**T4. Organizer reschedules** (moves it by an hour).
- **Expect:**
  - The mirror moves.
  - Google resets your response, so Apple shows the RSVP buttons again ("needs action").
- Accept again on the **Mac**.
- **Expect:** the organizer sees Accepted again, with one email.

**T5. (Optional, needs a second Google account.)** The extra guest accepts.
- **Expect:** within a cycle the mirror's invitee list shows that guest as accepted.

**T6. Decline on the Mac.**
- **Expect:**
  - The organizer sees **Declined**, with one email.
  - The event disappears from Apple Calendar unless "Show Declined Events" is on (P0-6, `declined: keep`).

**T7. Organizer deletes the event.**
- **Expect:** the mirror disappears from calbridge-test on both devices. No email from iCloud.

## 3. A recurring invitation

**T8. New weekly series.** From `ORGANIZER_TEST`, invite `user@YOUR_DOMAIN` to a weekly event with
4 occurrences, starting this week.
- **Expect:** one recurring event in Apple with the right 4 dates and RSVP buttons.

**T9. Accept the whole series on iPhone** ("All events" if asked).
- **Expect:**
  - The organizer sees Accepted for the series, with **one** email (not four).
  - All occurrences in Apple show Accepted.

**T10. Answer one occurrence differently.** On the Mac, open the **third** occurrence and choose
Decline, applying it to **this event only**.
- **Expect** within one or two cycles:
  - In the organizer's Google Calendar only that occurrence shows you declined, and the others stay accepted.
  - Apple keeps your answer for that occurrence, and the other occurrences are unchanged.
- *Unverified before this trial:* finding the Google occurrence for a per-occurrence RSVP. A failure
  here is exactly what this step is for.

**T11. Organizer moves one occurrence** (the second) by two hours, "this event" only.
- **Expect:** that occurrence moves in Apple, and the others don't. Your answer for it may reset to needs action (Google's choice).

**T12. Organizer cancels one occurrence** (the fourth).
- **Expect:** that occurrence disappears in Apple. The rest of the series stays.

**T13. Organizer changes "this and following"** from the third occurrence (for example, a new time).
- **Expect:**
  - Apple shows the first part of the series ending before the change, and a second series from the change on.
  - Both have the right times, and nothing is duplicated.
- Send `bin/calbridge status` output if anything looks off.

## 4. Deleting in Apple

**T14. Delete a mirror in Apple.** Create one more single invitation (as in T1) and wait for it to
appear. Then delete it in Apple Calendar ("Delete and Notify").
- **Expect:**
  - The organizer is **not** notified, and Google still shows no response (P0-9; the README explains this).
  - `bin/calbridge status` shows a `user_deleted` mirror.
- Now have the organizer change that event's description.
- **Expect:** it is **not** recreated in Apple.

## 5. Private events

**T15.** Have the organizer mark an invitation **Private** (visibility) and save.
- **Expect:** with `privacy: full`, the mirror still shows the real title and details (your choice;
  `honor_private` would show "Busy").

## 6. Restart and recovery

**T16.** Stop `serve` with Ctrl-C while it is waiting between cycles.
- **Expect:** it exits promptly with no error. Start it again (step 1.2).
- **Expect:** the next cycles write nothing for unchanged events (`/status` → `last_cycle` shows zero created/updated).

**T17. Reconcile.**
```sh
bin/calbridge reconcile --config dev/config.yaml
```
- **Expect:** ignore the `ok` rows; the only others should be the `user_deleted` mirror from T14 and
  nothing unexpected. Run it while `serve` is stopped, or expect the lock refusal (exit 3).

## 7. Results

| Step | Pass / fail | Notes (no real addresses needed) |
|---|---|---|
| 0 cleanup | | |
| 1 dry run / serve / health / lock | | |
| T1 new invite | | |
| T2 accept (iPhone) | | |
| T3 organizer edit | | |
| T4 reschedule + re-accept | | |
| T5 guest accepts (optional) | | |
| T6 decline (Mac) | | |
| T7 organizer deletes | | |
| T8 weekly series | | |
| T9 accept all | | |
| T10 one occurrence declined | | |
| T11 occurrence moved | | |
| T12 occurrence cancelled | | |
| T13 this and following | | |
| T14 delete in Apple | | |
| T15 private event | | |
| T16 restart | | |
| T17 reconcile | | |

Emails received (count and sender) per step:

## 8. Afterwards

- Leave `serve` running for a day on calbridge-test and check that `/healthz` stays `ok` and the logs show no errors.
- Bugs found here are fixed with a test (rule 8) before moving on.

## 9. The main calendar (only when you decide to)

Nothing in this checklist touches the main, shared calendar. When you are ready, and only then:

1. Copy the config, change `icloud.calendar` (or `calendar_href`, since there are two calendars
   named "Family") to the main calendar, and use a **separate database path** so the test mappings
   don't carry over.
2. Run **only** the dry run, and share its output before anything else:
   ```sh
   bin/calbridge once --dry-run --config dev/config-main.yaml
   ```
3. Switching `serve` to the main calendar is a separate, explicit decision.
