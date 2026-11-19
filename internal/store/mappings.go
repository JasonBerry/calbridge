package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Mapping is one mirrored iCloud object (= one Google series or single event).
// Empty strings and zero times are stored as NULL.
type Mapping struct {
	ID                int64
	GoogleCalendarID  string
	GoogleEventID     string
	ICalUID           string
	ICloudCalendar    string
	ICloudHref        string
	ICloudETag        string
	State             State
	SkipReason        string
	LastGoogleETag    string
	LastGoogleUpdated time.Time
	Sequence          int64
	ContentHash       string
	GoogleStatus      string
	ICloudPartStat    string
	AgreedStatus      string
	RSVPPending       string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         time.Time
	// Retry state (migration 0002): consecutive failures, earliest next
	// retry, and the last error class.
	Failures      int
	NextAttemptAt time.Time
	LastError     string
}

// MaxFailures is how many consecutive failures park a row (§8.7).
const MaxFailures = 10

// Parked reports whether the row has failed too often to be retried
// automatically; a new change to its Google event retries it.
func (m Mapping) Parked() bool { return m.Failures >= MaxFailures }

const mappingCols = `id, google_calendar_id, google_event_id, ical_uid, icloud_calendar, icloud_href,
	icloud_etag, state, skip_reason, last_google_etag, last_google_updated, sequence, content_hash,
	google_status, icloud_partstat, agreed_status, rsvp_pending, created_at, updated_at, deleted_at,
	failures, next_attempt_at, last_error`

type scanner interface{ Scan(dest ...any) error }

func scanMapping(r scanner) (Mapping, error) {
	var (
		m                                                   Mapping
		etag, skip, gEtag, gUpd, hash, gs, ip, agreed, rsvp sql.NullString
		created, updated, deleted, next, lastErr            sql.NullString
		state                                               string
	)
	err := r.Scan(&m.ID, &m.GoogleCalendarID, &m.GoogleEventID, &m.ICalUID, &m.ICloudCalendar, &m.ICloudHref,
		&etag, &state, &skip, &gEtag, &gUpd, &m.Sequence, &hash,
		&gs, &ip, &agreed, &rsvp, &created, &updated, &deleted, &m.Failures, &next, &lastErr)
	if err != nil {
		return m, err
	}
	m.State = State(state)
	m.ICloudETag, m.SkipReason, m.LastGoogleETag, m.ContentHash = etag.String, skip.String, gEtag.String, hash.String
	m.GoogleStatus, m.ICloudPartStat, m.AgreedStatus, m.RSVPPending = gs.String, ip.String, agreed.String, rsvp.String
	m.LastError = lastErr.String
	for _, p := range []struct {
		dst *time.Time
		src sql.NullString
	}{{&m.LastGoogleUpdated, gUpd}, {&m.CreatedAt, created}, {&m.UpdatedAt, updated}, {&m.DeletedAt, deleted}, {&m.NextAttemptAt, next}} {
		if *p.dst, err = parseTime(p.src); err != nil {
			return m, err
		}
	}
	return m, nil
}

func validateKeys(m *Mapping) error {
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"google_calendar_id", m.GoogleCalendarID}, {"google_event_id", m.GoogleEventID},
		{"ical_uid", m.ICalUID}, {"icloud_calendar", m.ICloudCalendar}, {"icloud_href", m.ICloudHref},
	} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("store: mapping missing %s", strings.Join(missing, ", "))
	}
	if !m.State.Valid() {
		return fmt.Errorf("store: mapping has invalid state %q", m.State)
	}
	return nil
}

// CreateMapping inserts m and sets m.ID, CreatedAt and UpdatedAt. A new
// mapping starts in pending_put (write-ahead intent, §8.6) unless it is
// created directly as skipped. Duplicate keys return ErrConflict.
func (q *Queries) CreateMapping(ctx context.Context, m *Mapping) error {
	if m.State == "" {
		m.State = StatePendingPut
	}
	if m.State != StatePendingPut && m.State != StateSkipped {
		return fmt.Errorf("%w: new mapping must start as %s or %s, not %s",
			ErrInvalidTransition, StatePendingPut, StateSkipped, m.State)
	}
	if err := validateKeys(m); err != nil {
		return err
	}
	now := q.stamp()
	res, err := q.x.ExecContext(ctx, `INSERT INTO mappings (
		google_calendar_id, google_event_id, ical_uid, icloud_calendar, icloud_href, icloud_etag, state,
		skip_reason, last_google_etag, last_google_updated, sequence, content_hash, google_status,
		icloud_partstat, agreed_status, rsvp_pending, created_at, updated_at, deleted_at,
		failures, next_attempt_at, last_error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.GoogleCalendarID, m.GoogleEventID, m.ICalUID, m.ICloudCalendar, m.ICloudHref, nullStr(m.ICloudETag),
		string(m.State), nullStr(m.SkipReason), nullStr(m.LastGoogleETag), fmtTime(m.LastGoogleUpdated),
		m.Sequence, nullStr(m.ContentHash), nullStr(m.GoogleStatus), nullStr(m.ICloudPartStat),
		nullStr(m.AgreedStatus), nullStr(m.RSVPPending), now, now, fmtTime(m.DeletedAt),
		m.Failures, fmtTime(m.NextAttemptAt), nullStr(m.LastError))
	if err != nil {
		return wrapErr("create mapping", err)
	}
	if m.ID, err = res.LastInsertId(); err != nil {
		return wrapErr("create mapping", err)
	}
	m.CreatedAt, _ = parseTime(nullStr(now))
	m.UpdatedAt = m.CreatedAt
	return nil
}

// UpdateMapping writes every mutable column of the row with m.ID
// (google_calendar_id, google_event_id and created_at never change). The state
// change from the stored row to m.State must be allowed by CanTransition;
// the check and the write are a single statement, so it is race-free without
// a transaction. Sets m.UpdatedAt.
func (q *Queries) UpdateMapping(ctx context.Context, m *Mapping) error {
	if err := validateKeys(m); err != nil {
		return err
	}
	sources := sourcesOf(m.State)
	ph := strings.TrimSuffix(strings.Repeat("?,", len(sources)), ",")
	args := []any{
		m.ICalUID, m.ICloudCalendar, m.ICloudHref, nullStr(m.ICloudETag), string(m.State), nullStr(m.SkipReason),
		nullStr(m.LastGoogleETag), fmtTime(m.LastGoogleUpdated), m.Sequence, nullStr(m.ContentHash),
		nullStr(m.GoogleStatus), nullStr(m.ICloudPartStat), nullStr(m.AgreedStatus), nullStr(m.RSVPPending),
	}
	now := q.stamp()
	args = append(args, now, fmtTime(m.DeletedAt), m.Failures, fmtTime(m.NextAttemptAt), nullStr(m.LastError), m.ID)
	for _, s := range sources {
		args = append(args, string(s))
	}
	res, err := q.x.ExecContext(ctx, `UPDATE mappings SET
		ical_uid=?, icloud_calendar=?, icloud_href=?, icloud_etag=?, state=?, skip_reason=?,
		last_google_etag=?, last_google_updated=?, sequence=?, content_hash=?,
		google_status=?, icloud_partstat=?, agreed_status=?, rsvp_pending=?,
		updated_at=?, deleted_at=?, failures=?, next_attempt_at=?, last_error=?
		WHERE id=? AND state IN (`+ph+`)`, args...)
	if err != nil {
		return wrapErr("update mapping", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrapErr("update mapping", err)
	}
	if n == 0 {
		cur, err := q.GetMapping(ctx, m.ID)
		if err != nil {
			return err
		}
		return checkTransition(cur.State, m.State)
	}
	m.UpdatedAt, _ = parseTime(nullStr(now))
	return nil
}

// GetMapping returns the mapping with the given id.
func (q *Queries) GetMapping(ctx context.Context, id int64) (Mapping, error) {
	return q.getMapping(ctx, "get mapping", "id=?", id)
}

// GetMappingByGoogleID looks a mapping up by its Google master/single id.
func (q *Queries) GetMappingByGoogleID(ctx context.Context, calendarID, eventID string) (Mapping, error) {
	return q.getMapping(ctx, "get mapping by google id", "google_calendar_id=? AND google_event_id=?", calendarID, eventID)
}

// GetMappingByHref looks a mapping up by its iCloud object href.
func (q *Queries) GetMappingByHref(ctx context.Context, calendarHref, href string) (Mapping, error) {
	return q.getMapping(ctx, "get mapping by href", "icloud_calendar=? AND icloud_href=?", calendarHref, href)
}

// GetMappingByUID looks a mapping up by the UID written to iCloud.
func (q *Queries) GetMappingByUID(ctx context.Context, calendarHref, uid string) (Mapping, error) {
	return q.getMapping(ctx, "get mapping by uid", "icloud_calendar=? AND ical_uid=?", calendarHref, uid)
}

func (q *Queries) getMapping(ctx context.Context, op, where string, args ...any) (Mapping, error) {
	m, err := scanMapping(q.x.QueryRowContext(ctx, "SELECT "+mappingCols+" FROM mappings WHERE "+where, args...))
	if err != nil {
		return Mapping{}, wrapErr(op, err)
	}
	return m, nil
}

// ListMappings returns mappings in any of the given states (all mappings
// when none are given), ordered by id.
func (q *Queries) ListMappings(ctx context.Context, states ...State) ([]Mapping, error) {
	query := "SELECT " + mappingCols + " FROM mappings"
	args := make([]any, 0, len(states))
	if len(states) > 0 {
		for _, s := range states {
			if !s.Valid() {
				return nil, fmt.Errorf("store: invalid state %q", s)
			}
			args = append(args, string(s))
		}
		query += " WHERE state IN (" + strings.TrimSuffix(strings.Repeat("?,", len(states)), ",") + ")"
	}
	rows, err := q.x.QueryContext(ctx, query+" ORDER BY id", args...)
	if err != nil {
		return nil, wrapErr("list mappings", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Mapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, wrapErr("list mappings", err)
		}
		out = append(out, m)
	}
	return out, wrapErr("list mappings", rows.Err())
}

// CountMappingsByState returns the number of mappings per state (for status).
func (q *Queries) CountMappingsByState(ctx context.Context) (map[State]int, error) {
	rows, err := q.x.QueryContext(ctx, "SELECT state, COUNT(*) FROM mappings GROUP BY state")
	if err != nil {
		return nil, wrapErr("count mappings", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[State]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, wrapErr("count mappings", err)
		}
		out[State(s)] = n
	}
	return out, wrapErr("count mappings", rows.Err())
}

// DeleteMappingRow removes one mapping row (and, by cascade, its
// instance_state). It never touches iCloud: it is for retention pruning of
// rows already in deleted or skipped, and refuses any other state so that a
// live mirror cannot lose its mapping (CLAUDE.md rule 5).
func (q *Queries) DeleteMappingRow(ctx context.Context, id int64) error {
	res, err := q.x.ExecContext(ctx, "DELETE FROM mappings WHERE id=? AND state IN (?,?)",
		id, string(StateDeleted), string(StateSkipped))
	if err != nil {
		return wrapErr("delete mapping row", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrapErr("delete mapping row", err)
	}
	if n == 0 {
		cur, err := q.GetMapping(ctx, id)
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: cannot remove a %s mapping row", ErrInvalidTransition, cur.State)
	}
	return nil
}

// IsNotFound reports whether err is ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
