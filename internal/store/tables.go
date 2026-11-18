package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// --- sync_state --------------------------------------------------------------

// SyncState is a stored sync cursor ("google:<calendarID>", "icloud:<href>").
type SyncState struct {
	Key          string
	Token        string
	CTag         string
	LastFullSync time.Time
	LastSuccess  time.Time
	UpdatedAt    time.Time
}

// GetSyncState returns the cursor for key, or ErrNotFound.
func (q *Queries) GetSyncState(ctx context.Context, key string) (SyncState, error) {
	var (
		s                       SyncState
		tok, ctag, full, ok, up sql.NullString
	)
	err := q.x.QueryRowContext(ctx,
		"SELECT key, token, ctag, last_full_sync, last_success, updated_at FROM sync_state WHERE key=?", key).
		Scan(&s.Key, &tok, &ctag, &full, &ok, &up)
	if err != nil {
		return SyncState{}, wrapErr("get sync state", err)
	}
	s.Token, s.CTag = tok.String, ctag.String
	if s.LastFullSync, err = parseTime(full); err != nil {
		return s, err
	}
	if s.LastSuccess, err = parseTime(ok); err != nil {
		return s, err
	}
	s.UpdatedAt, err = parseTime(up)
	return s, err
}

// PutSyncState inserts or replaces the cursor for s.Key and sets s.UpdatedAt.
func (q *Queries) PutSyncState(ctx context.Context, s *SyncState) error {
	if s.Key == "" {
		return fmt.Errorf("store: sync state key is empty")
	}
	now := q.stamp()
	_, err := q.x.ExecContext(ctx, `INSERT INTO sync_state (key, token, ctag, last_full_sync, last_success, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET token=excluded.token, ctag=excluded.ctag,
		last_full_sync=excluded.last_full_sync, last_success=excluded.last_success, updated_at=excluded.updated_at`,
		s.Key, nullStr(s.Token), nullStr(s.CTag), fmtTime(s.LastFullSync), fmtTime(s.LastSuccess), now)
	if err != nil {
		return wrapErr("put sync state", err)
	}
	s.UpdatedAt, _ = parseTime(nullStr(now))
	return nil
}

// DeleteSyncState removes the cursor for key (forces a full resync, §8.3).
// Missing keys are not an error.
func (q *Queries) DeleteSyncState(ctx context.Context, key string) error {
	_, err := q.x.ExecContext(ctx, "DELETE FROM sync_state WHERE key=?", key)
	return wrapErr("delete sync state", err)
}

// --- google_events -----------------------------------------------------------

// GoogleEvent is the raw cache of one Google event item.
type GoogleEvent struct {
	CalendarID    string
	EventID       string
	SeriesID      string // recurringEventId, or EventID for masters/singles
	ICalUID       string
	Status        string
	ETag          string
	Updated       time.Time
	OriginalStart string // originalStartTime as received (date or dateTime)
	JSON          []byte
	SeenAt        time.Time
}

const googleEventCols = `google_calendar_id, google_event_id, series_id, ical_uid, status, etag, updated,
	original_start, json, seen_at`

func scanGoogleEvent(r scanner) (GoogleEvent, error) {
	var (
		e                          GoogleEvent
		uid, etag, upd, orig, seen sql.NullString
	)
	if err := r.Scan(&e.CalendarID, &e.EventID, &e.SeriesID, &uid, &e.Status, &etag, &upd, &orig, &e.JSON, &seen); err != nil {
		return e, err
	}
	e.ICalUID, e.ETag, e.OriginalStart = uid.String, etag.String, orig.String
	var err error
	if e.Updated, err = parseTime(upd); err != nil {
		return e, err
	}
	e.SeenAt, err = parseTime(seen)
	return e, err
}

// PutGoogleEvent inserts or replaces a cached Google item and sets e.SeenAt.
// A cancelled tombstone (only id/status/etag) replaces the cached item's
// status and JSON but keeps the series_id, iCalUID and original_start learned
// earlier, since tombstones do not carry them (P0-1).
func (q *Queries) PutGoogleEvent(ctx context.Context, e *GoogleEvent) error {
	if e.CalendarID == "" || e.EventID == "" || e.Status == "" {
		return fmt.Errorf("store: google event needs calendar id, event id and status")
	}
	if e.SeriesID == "" {
		e.SeriesID = e.EventID
	}
	if len(e.JSON) == 0 {
		return fmt.Errorf("store: google event %s has no JSON", e.EventID)
	}
	now := q.stamp()
	_, err := q.x.ExecContext(ctx, `INSERT INTO google_events (`+googleEventCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(google_calendar_id, google_event_id) DO UPDATE SET
		series_id = CASE WHEN excluded.series_id = excluded.google_event_id THEN google_events.series_id ELSE excluded.series_id END,
		ical_uid = COALESCE(excluded.ical_uid, google_events.ical_uid),
		status = excluded.status, etag = excluded.etag,
		updated = COALESCE(excluded.updated, google_events.updated),
		original_start = COALESCE(excluded.original_start, google_events.original_start),
		json = excluded.json, seen_at = excluded.seen_at`,
		e.CalendarID, e.EventID, e.SeriesID, nullStr(e.ICalUID), e.Status, nullStr(e.ETag), fmtTime(e.Updated),
		nullStr(e.OriginalStart), e.JSON, now)
	if err != nil {
		return wrapErr("put google event", err)
	}
	e.SeenAt, _ = parseTime(nullStr(now))
	return nil
}

// GetGoogleEvent returns one cached item.
func (q *Queries) GetGoogleEvent(ctx context.Context, calendarID, eventID string) (GoogleEvent, error) {
	e, err := scanGoogleEvent(q.x.QueryRowContext(ctx,
		"SELECT "+googleEventCols+" FROM google_events WHERE google_calendar_id=? AND google_event_id=?",
		calendarID, eventID))
	if err != nil {
		return GoogleEvent{}, wrapErr("get google event", err)
	}
	return e, nil
}

// ListSeries returns every cached item of a series (master first, then
// exceptions by original start).
func (q *Queries) ListSeries(ctx context.Context, calendarID, seriesID string) ([]GoogleEvent, error) {
	rows, err := q.x.QueryContext(ctx, "SELECT "+googleEventCols+` FROM google_events
		WHERE google_calendar_id=? AND series_id=?
		ORDER BY (google_event_id = series_id) DESC, original_start, google_event_id`, calendarID, seriesID)
	if err != nil {
		return nil, wrapErr("list series", err)
	}
	defer func() { _ = rows.Close() }()
	var out []GoogleEvent
	for rows.Next() {
		e, err := scanGoogleEvent(rows)
		if err != nil {
			return nil, wrapErr("list series", err)
		}
		out = append(out, e)
	}
	return out, wrapErr("list series", rows.Err())
}

// DeleteGoogleEvent drops one cached item (local cache only; never remote).
// Missing items are not an error.
func (q *Queries) DeleteGoogleEvent(ctx context.Context, calendarID, eventID string) error {
	_, err := q.x.ExecContext(ctx, "DELETE FROM google_events WHERE google_calendar_id=? AND google_event_id=?",
		calendarID, eventID)
	return wrapErr("delete google event", err)
}

// --- instance_state ----------------------------------------------------------

// InstanceState is per-occurrence RSVP state of a recurring series.
type InstanceState struct {
	MappingID      int64
	RecurrenceID   string // normalised RECURRENCE-ID (UTC date-time or DATE)
	GoogleEventID  string
	GoogleStatus   string
	ICloudPartStat string
	AgreedStatus   string
	RSVPPending    string
	UpdatedAt      time.Time
}

// PutInstanceState inserts or replaces one occurrence's state and sets UpdatedAt.
func (q *Queries) PutInstanceState(ctx context.Context, s *InstanceState) error {
	if s.MappingID == 0 || s.RecurrenceID == "" {
		return fmt.Errorf("store: instance state needs mapping id and recurrence id")
	}
	now := q.stamp()
	_, err := q.x.ExecContext(ctx, `INSERT INTO instance_state (mapping_id, recurrence_id, google_event_id,
		google_status, icloud_partstat, agreed_status, rsvp_pending, updated_at) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(mapping_id, recurrence_id) DO UPDATE SET google_event_id=excluded.google_event_id,
		google_status=excluded.google_status, icloud_partstat=excluded.icloud_partstat,
		agreed_status=excluded.agreed_status, rsvp_pending=excluded.rsvp_pending, updated_at=excluded.updated_at`,
		s.MappingID, s.RecurrenceID, nullStr(s.GoogleEventID), nullStr(s.GoogleStatus), nullStr(s.ICloudPartStat),
		nullStr(s.AgreedStatus), nullStr(s.RSVPPending), now)
	if err != nil {
		return wrapErr("put instance state", err)
	}
	s.UpdatedAt, _ = parseTime(nullStr(now))
	return nil
}

// ListInstanceStates returns a series' occurrence states ordered by recurrence id.
func (q *Queries) ListInstanceStates(ctx context.Context, mappingID int64) ([]InstanceState, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT mapping_id, recurrence_id, google_event_id, google_status,
		icloud_partstat, agreed_status, rsvp_pending, updated_at FROM instance_state
		WHERE mapping_id=? ORDER BY recurrence_id`, mappingID)
	if err != nil {
		return nil, wrapErr("list instance states", err)
	}
	defer func() { _ = rows.Close() }()
	var out []InstanceState
	for rows.Next() {
		var (
			s                             InstanceState
			gid, gs, ip, agreed, rsvp, up sql.NullString
		)
		if err := rows.Scan(&s.MappingID, &s.RecurrenceID, &gid, &gs, &ip, &agreed, &rsvp, &up); err != nil {
			return nil, wrapErr("list instance states", err)
		}
		s.GoogleEventID, s.GoogleStatus, s.ICloudPartStat = gid.String, gs.String, ip.String
		s.AgreedStatus, s.RSVPPending = agreed.String, rsvp.String
		if s.UpdatedAt, err = parseTime(up); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, wrapErr("list instance states", rows.Err())
}

// DeleteInstanceState drops one occurrence's state (e.g. the occurrence was
// cancelled). Missing rows are not an error.
func (q *Queries) DeleteInstanceState(ctx context.Context, mappingID int64, recurrenceID string) error {
	_, err := q.x.ExecContext(ctx, "DELETE FROM instance_state WHERE mapping_id=? AND recurrence_id=?",
		mappingID, recurrenceID)
	return wrapErr("delete instance state", err)
}

// --- ops_log -----------------------------------------------------------------

// Direction of an audited operation.
type Direction string

const (
	DirG2I       Direction = "g2i"
	DirI2G       Direction = "i2g"
	DirReconcile Direction = "reconcile"
)

// Op is one audit log entry. Detail must not contain event titles,
// descriptions or attendee addresses (CLAUDE.md rule 6): ids and codes only.
type Op struct {
	ID        int64
	At        time.Time
	Direction Direction
	Op        string // put_create|put_update|delete|patch_rsvp|adopt|skip
	MappingID int64  // 0 = none
	Detail    string
}

// AppendOp appends to the audit log and sets o.ID and o.At.
func (q *Queries) AppendOp(ctx context.Context, o *Op) error {
	if o.Op == "" {
		return fmt.Errorf("store: op is empty")
	}
	now := q.stamp()
	var mid sql.NullInt64
	if o.MappingID != 0 {
		mid = sql.NullInt64{Int64: o.MappingID, Valid: true}
	}
	res, err := q.x.ExecContext(ctx, "INSERT INTO ops_log (at, direction, op, mapping_id, detail) VALUES (?,?,?,?,?)",
		now, string(o.Direction), o.Op, mid, nullStr(o.Detail))
	if err != nil {
		return wrapErr("append op", err)
	}
	if o.ID, err = res.LastInsertId(); err != nil {
		return wrapErr("append op", err)
	}
	o.At, _ = parseTime(nullStr(now))
	return nil
}

// RecentOps returns up to limit newest audit entries, newest first.
func (q *Queries) RecentOps(ctx context.Context, limit int) ([]Op, error) {
	rows, err := q.x.QueryContext(ctx,
		"SELECT id, at, direction, op, mapping_id, detail FROM ops_log ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, wrapErr("recent ops", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Op
	for rows.Next() {
		var (
			o          Op
			at, detail sql.NullString
			dir        string
			mid        sql.NullInt64
		)
		if err := rows.Scan(&o.ID, &at, &dir, &o.Op, &mid, &detail); err != nil {
			return nil, wrapErr("recent ops", err)
		}
		o.Direction, o.MappingID, o.Detail = Direction(dir), mid.Int64, detail.String
		if o.At, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, wrapErr("recent ops", rows.Err())
}

// PruneOps deletes audit entries older than before (retention, §6) and
// returns how many were removed. Local log rows only.
func (q *Queries) PruneOps(ctx context.Context, before time.Time) (int64, error) {
	res, err := q.x.ExecContext(ctx, "DELETE FROM ops_log WHERE at < ?", fmtTime(before))
	if err != nil {
		return 0, wrapErr("prune ops", err)
	}
	n, err := res.RowsAffected()
	return n, wrapErr("prune ops", err)
}
