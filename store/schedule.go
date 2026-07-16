package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Schedule schema — logically "v9" (the tiered-autoclose branch owns v8).
//
// Deliberately NOT gated on the global schema_version counter: all DDL here is
// idempotent (IF NOT EXISTS) and touches only schedule-owned tables, so it
// applies cleanly whether or not the v8 migration shipped first, and it never
// bumps schema_version — meaning a later v8 block still fires on DBs that got
// these tables first. Bookkeeping lives in its own setting key.
const scheduleSchemaVersion = 9

func (db *DB) migrateSchedule() error {
	_, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS schedule_sessions (
			id            TEXT PRIMARY KEY,
			contact_jid   TEXT NOT NULL,
			contact_name  TEXT NOT NULL DEFAULT '',
			state         TEXT NOT NULL,
			intent        TEXT NOT NULL DEFAULT 'schedule',
			data          TEXT NOT NULL DEFAULT '{}',
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL,
			closed_at     INTEGER,
			close_reason  TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_schedule_sessions_contact
			ON schedule_sessions(contact_jid) WHERE state != 'closed';

		CREATE TABLE IF NOT EXISTS schedule_tz_overrides (
			contact_jid TEXT PRIMARY KEY,
			timezone    TEXT NOT NULL,
			updated_at  INTEGER NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	return db.SetSetting("schedule_schema_version", fmt.Sprintf("%d", scheduleSchemaVersion))
}

// ScheduleSessionRow is the persisted form of a scheduling session. The
// engine-level state lives in the Data JSON blob (owned by the schedule
// package); the columns here exist for querying.
type ScheduleSessionRow struct {
	ID          string
	ContactJID  string
	ContactName string
	State       string
	Intent      string
	Data        string // JSON blob
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    time.Time
	CloseReason string
}

func GenerateScheduleSessionID() string {
	b := make([]byte, 8)
	io.ReadFull(rand.Reader, b)
	return "ss_" + hex.EncodeToString(b)
}

func (db *DB) SaveScheduleSession(r *ScheduleSessionRow) error {
	if r.ID == "" {
		r.ID = GenerateScheduleSessionID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	r.UpdatedAt = time.Now()
	var closedAt interface{}
	if !r.ClosedAt.IsZero() {
		closedAt = r.ClosedAt.Unix()
	}
	_, err := db.conn.Exec(`
		INSERT INTO schedule_sessions (id, contact_jid, contact_name, state, intent, data, created_at, updated_at, closed_at, close_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			contact_jid = excluded.contact_jid,
			contact_name = excluded.contact_name,
			state = excluded.state,
			intent = excluded.intent,
			data = excluded.data,
			updated_at = excluded.updated_at,
			closed_at = excluded.closed_at,
			close_reason = excluded.close_reason`,
		r.ID, r.ContactJID, r.ContactName, r.State, r.Intent, r.Data,
		r.CreatedAt.Unix(), r.UpdatedAt.Unix(), closedAt, r.CloseReason,
	)
	return err
}

func (db *DB) scanScheduleSession(row *sql.Row) (*ScheduleSessionRow, error) {
	r := &ScheduleSessionRow{}
	var created, updated int64
	var closed sql.NullInt64
	err := row.Scan(&r.ID, &r.ContactJID, &r.ContactName, &r.State, &r.Intent, &r.Data,
		&created, &updated, &closed, &r.CloseReason)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = time.Unix(created, 0)
	r.UpdatedAt = time.Unix(updated, 0)
	if closed.Valid {
		r.ClosedAt = time.Unix(closed.Int64, 0)
	}
	return r, nil
}

const scheduleSessionCols = "id, contact_jid, contact_name, state, intent, data, created_at, updated_at, closed_at, close_reason"

// GetOpenScheduleSession returns the open (non-closed) session for a contact,
// or nil. One open session per contact at a time.
func (db *DB) GetOpenScheduleSession(contactJID string) (*ScheduleSessionRow, error) {
	row := db.conn.QueryRow(
		"SELECT "+scheduleSessionCols+" FROM schedule_sessions WHERE contact_jid = ? AND state != 'closed' ORDER BY created_at DESC LIMIT 1",
		contactJID)
	return db.scanScheduleSession(row)
}

func (db *DB) GetScheduleSession(id string) (*ScheduleSessionRow, error) {
	row := db.conn.QueryRow("SELECT "+scheduleSessionCols+" FROM schedule_sessions WHERE id = ?", id)
	return db.scanScheduleSession(row)
}

// GetOpenScheduleSessions returns all non-closed sessions (for the watcher and
// the expiry sweep).
func (db *DB) GetOpenScheduleSessions() ([]*ScheduleSessionRow, error) {
	rows, err := db.conn.Query("SELECT " + scheduleSessionCols + " FROM schedule_sessions WHERE state != 'closed'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ScheduleSessionRow
	for rows.Next() {
		r := &ScheduleSessionRow{}
		var created, updated int64
		var closed sql.NullInt64
		if err := rows.Scan(&r.ID, &r.ContactJID, &r.ContactName, &r.State, &r.Intent, &r.Data,
			&created, &updated, &closed, &r.CloseReason); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(created, 0)
		r.UpdatedAt = time.Unix(updated, 0)
		if closed.Valid {
			r.ClosedAt = time.Unix(closed.Int64, 0)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Timezone overrides — per-contact corrections beat phone-prefix inference.

func (db *DB) GetContactTZOverride(contactJID string) string {
	var tz string
	db.conn.QueryRow("SELECT timezone FROM schedule_tz_overrides WHERE contact_jid = ?", contactJID).Scan(&tz)
	return tz
}

func (db *DB) SetContactTZOverride(contactJID, tz string) error {
	_, err := db.conn.Exec(`
		INSERT INTO schedule_tz_overrides (contact_jid, timezone, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(contact_jid) DO UPDATE SET timezone = excluded.timezone, updated_at = excluded.updated_at`,
		contactJID, tz, time.Now().Unix())
	return err
}

// Google OAuth token — AES-encrypted at rest, same scheme as the API key.

func (db *DB) GetGoogleToken() string {
	stored := db.GetSetting("google_oauth_token")
	if stored == "" {
		return ""
	}
	decrypted, err := db.decrypt(stored)
	if err != nil {
		return ""
	}
	return decrypted
}

func (db *DB) SetGoogleToken(tokenJSON string) error {
	if tokenJSON == "" {
		return db.SetSetting("google_oauth_token", "")
	}
	encrypted, err := db.encrypt(tokenJSON)
	if err != nil {
		return err
	}
	return db.SetSetting("google_oauth_token", encrypted)
}

// GetGoogleClientSecret is encrypted like the API key; the client ID is not
// secret and is stored in plaintext under google_client_id.
func (db *DB) GetGoogleClientSecret() string {
	stored := db.GetSetting("google_client_secret")
	if stored == "" {
		return ""
	}
	decrypted, err := db.decrypt(stored)
	if err != nil {
		return ""
	}
	return decrypted
}

func (db *DB) SetGoogleClientSecret(secret string) error {
	if secret == "" {
		return db.SetSetting("google_client_secret", "")
	}
	encrypted, err := db.encrypt(secret)
	if err != nil {
		return err
	}
	return db.SetSetting("google_client_secret", encrypted)
}

// SchedulePrefs — everything the Settings UI edits, one JSON blob.

type SchedulePrefs struct {
	Calendars       []string `json:"calendars"`         // calendar IDs used for busy computation; primary used for event creation
	DayStartMin     int      `json:"day_start_min"`     // meeting hours, minutes from midnight
	DayEndMin       int      `json:"day_end_min"`
	Workdays        []int    `json:"workdays"`          // 0=Sunday..6=Saturday
	TravelBufferMin int      `json:"travel_buffer_min"` // in-person only
	DefaultDurMin   int      `json:"default_dur_min"`
	IgnoreTitles    []string `json:"ignore_titles"`   // events whose title contains one of these are never busy
	ProtectedRules  string   `json:"protected_rules"` // freeform, e.g. "no meetings before 10am Fridays"
	Timezone        string   `json:"timezone"`        // user's own TZ; empty = system
}

func DefaultSchedulePrefs() SchedulePrefs {
	return SchedulePrefs{
		DayStartMin:   9 * 60,
		DayEndMin:     18 * 60,
		Workdays:      []int{1, 2, 3, 4, 5},
		DefaultDurMin: 30,
		IgnoreTitles:  []string{"block", "hold"},
	}
}

func (db *DB) GetSchedulePrefs() SchedulePrefs {
	raw := db.GetSetting("schedule_prefs")
	prefs := DefaultSchedulePrefs()
	if raw == "" {
		return prefs
	}
	if err := json.Unmarshal([]byte(raw), &prefs); err != nil {
		return DefaultSchedulePrefs()
	}
	return prefs
}

func (db *DB) SetSchedulePrefs(p SchedulePrefs) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return db.SetSetting("schedule_prefs", string(data))
}

// Message helpers for the schedule watcher.

// GetLastNMessagesForChat returns the newest n messages of a chat in
// ascending time order.
func (db *DB) GetLastNMessagesForChat(chatJID string, n int) ([]*Message, error) {
	msgs, err := db.queryMessages(`
		SELECT id, chat_jid, sender_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group
		FROM messages WHERE chat_jid = ?
		ORDER BY timestamp DESC LIMIT ?`, chatJID, n)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// CountMessagesBetween counts messages in a chat with fromUnix <= ts <= toUnix,
// excluding the given message IDs. Used for consent-scoping ("is this the next
// self-chat message after Commit's prompt?").
func (db *DB) CountMessagesBetween(chatJID string, fromUnix, toUnix int64, excludeIDs []string) (int, error) {
	query := "SELECT COUNT(*) FROM messages WHERE chat_jid = ? AND timestamp >= ? AND timestamp <= ?"
	args := []interface{}{chatJID, fromUnix, toUnix}
	for _, id := range excludeIDs {
		query += " AND id != ?"
		args = append(args, id)
	}
	var n int
	err := db.conn.QueryRow(query, args...).Scan(&n)
	return n, err
}

// GetDirectChats lists 1:1 chats with a best-known display name, for fuzzy
// contact resolution.
func (db *DB) GetDirectChats() (map[string]string, error) {
	out := map[string]string{}
	rows, err := db.conn.Query(`
		SELECT chat_jid, chat_name FROM (
			SELECT chat_jid, chat_name, ROW_NUMBER() OVER (PARTITION BY chat_jid ORDER BY timestamp DESC) rn
			FROM messages WHERE is_group = 0 AND chat_name != ''
		) WHERE rn = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var jid, name string
		if err := rows.Scan(&jid, &name); err != nil {
			return nil, err
		}
		out[jid] = name
	}
	// Contact overrides win.
	for jid, name := range db.GetContactOverrides() {
		if name != "" {
			out[jid] = name
		}
	}
	return out, rows.Err()
}

// GetLastBookedScheduleSession returns the most recent session for a contact
// that resulted in a booking (its data blob has booked_event_id set).
func (db *DB) GetLastBookedScheduleSession(contactJID string) (*ScheduleSessionRow, error) {
	row := db.conn.QueryRow(
		"SELECT "+scheduleSessionCols+` FROM schedule_sessions
		 WHERE contact_jid = ? AND data LIKE '%"booked_event_id"%'
		 ORDER BY updated_at DESC LIMIT 1`, contactJID)
	return db.scanScheduleSession(row)
}
