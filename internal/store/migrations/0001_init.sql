-- 0001_init.sql: initial schema (PLAN.md §6).
-- foreign_keys is enabled per connection by the DSN (a PRAGMA inside the
-- migration transaction would be a no-op). All timestamps are UTC RFC 3339
-- text with fixed-width nanoseconds so they sort lexically.

CREATE TABLE sync_state (
  key            TEXT PRIMARY KEY,          -- e.g. 'google:primary', 'icloud:<calendar_href>'
  token          TEXT,                      -- Google nextSyncToken, or CalDAV sync-token
  ctag           TEXT,                      -- CalDAV getctag (fallback path)
  last_full_sync TEXT,
  last_success   TEXT,
  updated_at     TEXT NOT NULL
);

-- Raw cache of every Google event item we have seen (masters, singles, exceptions).
CREATE TABLE google_events (
  google_calendar_id TEXT NOT NULL,
  google_event_id    TEXT NOT NULL,         -- item id (instance ids for exceptions)
  series_id          TEXT NOT NULL,         -- = recurringEventId, or own id for masters/singles
  ical_uid           TEXT,
  status             TEXT NOT NULL,         -- confirmed|tentative|cancelled
  etag               TEXT,
  updated            TEXT,
  original_start     TEXT,                  -- for exceptions: originalStartTime (date or dateTime)
  json               BLOB NOT NULL,         -- full events#event JSON as received
  seen_at            TEXT NOT NULL,
  PRIMARY KEY (google_calendar_id, google_event_id)
);
CREATE INDEX google_events_series ON google_events(google_calendar_id, series_id);

-- One row per mirrored iCloud object (= one Google series or single event).
CREATE TABLE mappings (
  id                 INTEGER PRIMARY KEY,
  google_calendar_id TEXT NOT NULL,
  google_event_id    TEXT NOT NULL,         -- master/single id
  ical_uid           TEXT NOT NULL,         -- UID written to iCloud (normally Google iCalUID)
  icloud_calendar    TEXT NOT NULL,         -- calendar collection href
  icloud_href        TEXT NOT NULL,         -- object href (deterministic, see §7.1)
  icloud_etag        TEXT,                  -- last ETag we wrote or read
  state              TEXT NOT NULL
                     CHECK (state IN ('pending_put','synced','pending_delete','deleted','user_deleted','skipped')),
  skip_reason        TEXT,
  last_google_etag   TEXT,
  last_google_updated TEXT,
  sequence           INTEGER NOT NULL DEFAULT 0,  -- SEQUENCE we last wrote
  content_hash       TEXT,                  -- sha256 of rendered VEVENT(s) minus volatile props
  google_status      TEXT,                  -- last seen Google responseStatus for self
  icloud_partstat    TEXT,                  -- last seen iCloud PARTSTAT for self
  agreed_status      TEXT,                  -- last value both sides agreed on (Google vocabulary)
  rsvp_pending       TEXT,                  -- status we intend to push to Google but haven't confirmed
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL,
  deleted_at         TEXT,
  UNIQUE (google_calendar_id, google_event_id),
  UNIQUE (icloud_calendar, icloud_href),
  UNIQUE (icloud_calendar, ical_uid)
);
CREATE INDEX mappings_state ON mappings(state);

-- Per-instance RSVP/override state for recurring series.
CREATE TABLE instance_state (
  mapping_id        INTEGER NOT NULL REFERENCES mappings(id) ON DELETE CASCADE,
  recurrence_id     TEXT NOT NULL,          -- RECURRENCE-ID normalised (UTC or DATE)
  google_event_id   TEXT,                   -- Google instance id if known
  google_status     TEXT,
  icloud_partstat   TEXT,
  agreed_status     TEXT,
  rsvp_pending      TEXT,
  updated_at        TEXT NOT NULL,
  PRIMARY KEY (mapping_id, recurrence_id)
);

-- Append-only audit log of writes. No event bodies.
CREATE TABLE ops_log (
  id          INTEGER PRIMARY KEY,
  at          TEXT NOT NULL,
  direction   TEXT NOT NULL CHECK (direction IN ('g2i','i2g','reconcile')),
  op          TEXT NOT NULL,               -- put_create|put_update|delete|patch_rsvp|adopt|skip
  mapping_id  INTEGER,
  detail      TEXT                         -- short, no PII beyond ids
);
CREATE INDEX ops_log_at ON ops_log(at);
