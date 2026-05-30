package store

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"time"
)

type Commitment struct {
	ID          string    `json:"id"`
	ChatJID     string    `json:"chat_jid"`
	ChatName    string    `json:"chat_name"`
	PersonName  string    `json:"person_name"`
	PersonJID   string    `json:"person_jid"`
	Title       string    `json:"title"`
	Context     string    `json:"context"`
	Direction   string    `json:"direction"` // "you_owe" or "they_owe"
	SourceQuote string    `json:"source_quote"`
	SourceTime  string    `json:"source_time"`
	MessageID   string    `json:"message_id"`
	Status      string    `json:"status"` // "open", "resolved", "dismissed", "snoozed"
	DueHint     string    `json:"due_hint"`
	CreatedAt   time.Time `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	IsGroup     bool      `json:"is_group"`
	Favorited   bool      `json:"favorited"`
	ResolvedBy  string     `json:"resolved_by"` // "user", "auto"
	ReminderAt  *time.Time `json:"reminder_at,omitempty"`
}

type CommitmentGroup struct {
	Name        string        `json:"name"`
	ChatJID     string        `json:"chat_jid"`
	IsGroup     bool          `json:"is_group"`
	Commitments []*Commitment `json:"commitments"`
	YouOwe      int           `json:"you_owe"`
	TheyOwe     int           `json:"they_owe"`
}

func GenerateCommitmentID(chatJID, title, direction string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s", chatJID, title, direction)))
	return fmt.Sprintf("%x", h[:16])
}

func (db *DB) SaveCommitment(c *Commitment) error {
	if c.ID == "" {
		c.ID = GenerateCommitmentID(c.ChatJID, c.Title, c.Direction)
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	if c.ResolvedBy == "" {
		c.ResolvedBy = "user"
	}
	_, err := db.conn.Exec(`
		INSERT OR IGNORE INTO commitments
			(id, chat_jid, chat_name, person_name, person_jid, title, context, direction,
			 source_quote, source_time, message_id, status, due_hint, created_at, is_group, favorited, resolved_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ChatJID, c.ChatName, c.PersonName, c.PersonJID,
		c.Title, c.Context, c.Direction, c.SourceQuote, c.SourceTime,
		c.MessageID, c.Status, c.DueHint, c.CreatedAt.Unix(), boolToInt(c.IsGroup), boolToInt(c.Favorited),
		c.ResolvedBy,
	)
	return err
}

func (db *DB) GetCommitments(status string) ([]*Commitment, error) {
	oneDayFromNow := time.Now().Add(24 * time.Hour).Unix()
	query := `SELECT id, chat_jid, chat_name, person_name, person_jid, title, context, direction,
		source_quote, source_time, message_id, status, due_hint, created_at, resolved_at, is_group, favorited, resolved_by, reminder_at
		FROM commitments`
	args := []any{}

	if status == "open" {
		query += " WHERE status = ? AND (reminder_at IS NULL OR reminder_at <= ?)"
		args = append(args, status, oneDayFromNow)
	} else if status != "" {
		query += " WHERE status = ?"
		args = append(args, status)
	}
	query += " ORDER BY favorited DESC, created_at DESC"

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanCommitments(rows)
}

func (db *DB) GetCommitmentsGrouped(status string) ([]*CommitmentGroup, error) {
	commitments, err := db.GetCommitments(status)
	if err != nil {
		return nil, err
	}
	return groupCommitments(commitments), nil
}

func (db *DB) UpdateCommitmentStatus(id, status string) error {
	return db.updateCommitmentStatus(id, status, "user")
}

func (db *DB) AutoResolveCommitment(id string) error {
	return db.updateCommitmentStatus(id, "resolved", "auto")
}

func (db *DB) updateCommitmentStatus(id, status, resolvedBy string) error {
	var resolvedAt *int64
	if status == "resolved" || status == "dismissed" {
		now := time.Now().Unix()
		resolvedAt = &now
	}
	_, err := db.conn.Exec(
		"UPDATE commitments SET status = ?, resolved_at = ?, resolved_by = ? WHERE id = ?",
		status, resolvedAt, resolvedBy, id,
	)
	return err
}

func (db *DB) SearchCommitments(query string) ([]*Commitment, error) {
	pattern := "%" + query + "%"
	rows, err := db.conn.Query(`
		SELECT id, chat_jid, chat_name, person_name, person_jid, title, context, direction,
			source_quote, source_time, message_id, status, due_hint, created_at, resolved_at, is_group, favorited, resolved_by, reminder_at
		FROM commitments
		WHERE (title LIKE ? OR context LIKE ? OR person_name LIKE ? OR source_quote LIKE ? OR chat_name LIKE ?)
		ORDER BY CASE WHEN status = 'open' THEN 0 ELSE 1 END, favorited DESC, created_at DESC`,
		pattern, pattern, pattern, pattern, pattern,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanCommitments(rows)
}

func scanCommitments(rows *sql.Rows) ([]*Commitment, error) {
	var commitments []*Commitment
	for rows.Next() {
		c := &Commitment{}
		var createdAt int64
		var resolvedAt, reminderAt *int64
		var isGroup, favorited int
		if err := rows.Scan(&c.ID, &c.ChatJID, &c.ChatName, &c.PersonName, &c.PersonJID,
			&c.Title, &c.Context, &c.Direction, &c.SourceQuote, &c.SourceTime,
			&c.MessageID, &c.Status, &c.DueHint, &createdAt, &resolvedAt, &isGroup, &favorited, &c.ResolvedBy, &reminderAt); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(createdAt, 0)
		if resolvedAt != nil {
			t := time.Unix(*resolvedAt, 0)
			c.ResolvedAt = &t
		}
		if reminderAt != nil {
			t := time.Unix(*reminderAt, 0)
			c.ReminderAt = &t
		}
		c.IsGroup = isGroup == 1
		c.Favorited = favorited == 1
		if c.ResolvedBy == "" {
			c.ResolvedBy = "user"
		}
		commitments = append(commitments, c)
	}
	return commitments, rows.Err()
}

func (db *DB) ToggleFavorite(id string) (bool, error) {
	_, err := db.conn.Exec("UPDATE commitments SET favorited = CASE WHEN favorited = 0 THEN 1 ELSE 0 END WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	var fav int
	db.conn.QueryRow("SELECT favorited FROM commitments WHERE id = ?", id).Scan(&fav)
	return fav == 1, nil
}

func (db *DB) GetOpenCommitmentsForChat(chatJID string) ([]*Commitment, error) {
	rows, err := db.conn.Query(`
		SELECT id, chat_jid, chat_name, person_name, person_jid, title, context, direction,
			source_quote, source_time, message_id, status, due_hint, created_at, resolved_at, is_group, favorited, resolved_by, reminder_at
		FROM commitments
		WHERE chat_jid = ? AND status = 'open'
		ORDER BY created_at DESC`,
		chatJID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommitments(rows)
}

// Chat favorites

func (db *DB) ToggleChatFavorite(chatJID, chatName string, isGroup bool) (bool, error) {
	var exists int
	db.conn.QueryRow("SELECT COUNT(*) FROM favorite_chats WHERE chat_jid = ?", chatJID).Scan(&exists)
	if exists > 0 {
		_, err := db.conn.Exec("DELETE FROM favorite_chats WHERE chat_jid = ?", chatJID)
		return false, err
	}
	_, err := db.conn.Exec(
		"INSERT INTO favorite_chats (chat_jid, chat_name, is_group, created_at) VALUES (?, ?, ?, ?)",
		chatJID, chatName, boolToInt(isGroup), time.Now().Unix(),
	)
	return true, err
}

func (db *DB) IsChatFavorited(chatJID string) bool {
	var count int
	db.conn.QueryRow("SELECT COUNT(*) FROM favorite_chats WHERE chat_jid = ?", chatJID).Scan(&count)
	return count > 0
}

func (db *DB) GetFavoritedChatJIDs() (map[string]bool, error) {
	rows, err := db.conn.Query("SELECT chat_jid FROM favorite_chats")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]bool)
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, err
		}
		m[jid] = true
	}
	return m, rows.Err()
}

type FavoritesView struct {
	Open     []*CommitmentGroup `json:"open"`
	Resolved []*CommitmentGroup `json:"resolved"`
	FavChats map[string]bool    `json:"fav_chats"`
}

func (db *DB) GetFavoritesView() (*FavoritesView, error) {
	favChats, err := db.GetFavoritedChatJIDs()
	if err != nil {
		return nil, err
	}

	thirtyDaysAgo := time.Now().Add(-30 * 24 * time.Hour).Unix()

	rows, err := db.conn.Query(`
		SELECT id, chat_jid, chat_name, person_name, person_jid, title, context, direction,
			source_quote, source_time, message_id, status, due_hint, created_at, resolved_at, is_group, favorited, resolved_by, reminder_at
		FROM commitments
		WHERE (favorited = 1 OR chat_jid IN (SELECT chat_jid FROM favorite_chats))
			AND status = 'open'
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	openItems, err := scanCommitments(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}

	rows, err = db.conn.Query(`
		SELECT id, chat_jid, chat_name, person_name, person_jid, title, context, direction,
			source_quote, source_time, message_id, status, due_hint, created_at, resolved_at, is_group, favorited, resolved_by, reminder_at
		FROM commitments
		WHERE (favorited = 1 OR chat_jid IN (SELECT chat_jid FROM favorite_chats))
			AND status IN ('resolved', 'dismissed')
			AND resolved_at > ?
		ORDER BY resolved_at DESC
		LIMIT 20`, thirtyDaysAgo)
	if err != nil {
		return nil, err
	}
	resolvedItems, err := scanCommitments(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}

	return &FavoritesView{
		Open:     groupCommitments(openItems),
		Resolved: groupCommitments(resolvedItems),
		FavChats: favChats,
	}, nil
}

func groupCommitments(commitments []*Commitment) []*CommitmentGroup {
	groupMap := make(map[string]*CommitmentGroup)
	var order []string
	for _, c := range commitments {
		key := c.ChatJID
		if key == "" {
			key = c.PersonName
		}
		g, ok := groupMap[key]
		if !ok {
			g = &CommitmentGroup{
				Name:    c.ChatName,
				ChatJID: c.ChatJID,
				IsGroup: c.IsGroup,
			}
			if g.Name == "" {
				g.Name = c.PersonName
			}
			groupMap[key] = g
			order = append(order, key)
		}
		g.Commitments = append(g.Commitments, c)
		if c.Direction == "you_owe" {
			g.YouOwe++
		} else {
			g.TheyOwe++
		}
	}
	groups := make([]*CommitmentGroup, 0, len(order))
	for _, key := range order {
		groups = append(groups, groupMap[key])
	}
	return groups
}

func (db *DB) GetFavoritesCount() int {
	var count int
	db.conn.QueryRow(`
		SELECT COUNT(*) FROM commitments
		WHERE (favorited = 1 OR chat_jid IN (SELECT chat_jid FROM favorite_chats))
			AND status = 'open'`).Scan(&count)
	return count
}

type Stats struct {
	Open      int `json:"open"`
	YouOwe    int `json:"you_owe"`
	TheyOwe   int `json:"they_owe"`
	Resolved  int `json:"resolved"`
	Favorites int `json:"favorites"`
	FollowUps int `json:"follow_ups"`
}

func (db *DB) GetCommitmentStats() (*Stats, error) {
	s := &Stats{}
	var err error
	oneDayFromNow := time.Now().Add(24 * time.Hour).Unix()
	snoozeFilter := " AND (reminder_at IS NULL OR reminder_at <= ?)"
	if err = db.conn.QueryRow("SELECT COUNT(*) FROM commitments WHERE status = 'open'"+snoozeFilter, oneDayFromNow).Scan(&s.Open); err != nil {
		return nil, err
	}
	if err = db.conn.QueryRow("SELECT COUNT(*) FROM commitments WHERE status = 'open' AND direction = 'you_owe'"+snoozeFilter, oneDayFromNow).Scan(&s.YouOwe); err != nil {
		return nil, err
	}
	if err = db.conn.QueryRow("SELECT COUNT(*) FROM commitments WHERE status = 'open' AND direction = 'they_owe'"+snoozeFilter, oneDayFromNow).Scan(&s.TheyOwe); err != nil {
		return nil, err
	}
	if err = db.conn.QueryRow("SELECT COUNT(*) FROM commitments WHERE status = 'resolved'").Scan(&s.Resolved); err != nil {
		return nil, err
	}
	db.conn.QueryRow(`
		SELECT COUNT(*) FROM commitments
		WHERE (favorited = 1 OR chat_jid IN (SELECT chat_jid FROM favorite_chats))
			AND status = 'open'`).Scan(&s.Favorites)
	s.FollowUps, _ = db.CountFollowUps()
	return s, nil
}

func (db *DB) GetFollowUps() ([]*Commitment, error) {
	oneDayAgo := time.Now().Add(-24 * time.Hour).Unix()
	fortyEightHoursAgo := time.Now().Add(-48 * time.Hour).Unix()
	oneDayFromNow := time.Now().Add(24 * time.Hour).Unix()

	rows, err := db.conn.Query(`
		SELECT id, chat_jid, chat_name, person_name, person_jid, title, context, direction,
			source_quote, source_time, message_id, status, due_hint, created_at, resolved_at, is_group, favorited, resolved_by, reminder_at
		FROM commitments
		WHERE status = 'open'
			AND (
				(direction = 'they_owe'
					AND created_at < ?
					AND (last_nudged_at IS NULL OR last_nudged_at < ?)
					AND chat_jid NOT IN (
						SELECT DISTINCT chat_jid FROM messages
						WHERE is_from_me = 0 AND timestamp > commitments.created_at
					)
				)
				OR (reminder_at IS NOT NULL AND reminder_at > ?)
			)
		ORDER BY created_at ASC`, oneDayAgo, fortyEightHoursAgo, oneDayFromNow)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommitments(rows)
}

func (db *DB) CountFollowUps() (int, error) {
	oneDayAgo := time.Now().Add(-24 * time.Hour).Unix()
	fortyEightHoursAgo := time.Now().Add(-48 * time.Hour).Unix()
	oneDayFromNow := time.Now().Add(24 * time.Hour).Unix()

	var count int
	err := db.conn.QueryRow(`
		SELECT COUNT(*) FROM commitments
		WHERE status = 'open'
			AND (
				(direction = 'they_owe'
					AND created_at < ?
					AND (last_nudged_at IS NULL OR last_nudged_at < ?)
					AND chat_jid NOT IN (
						SELECT DISTINCT chat_jid FROM messages
						WHERE is_from_me = 0 AND timestamp > commitments.created_at
					)
				)
				OR (reminder_at IS NOT NULL AND reminder_at > ?)
			)`, oneDayAgo, fortyEightHoursAgo, oneDayFromNow).Scan(&count)
	return count, err
}

func (db *DB) GetRecentlyAutoResolved() ([]*Commitment, error) {
	oneDayAgo := time.Now().Add(-24 * time.Hour).Unix()
	rows, err := db.conn.Query(`
		SELECT id, chat_jid, chat_name, person_name, person_jid, title, context, direction,
			source_quote, source_time, message_id, status, due_hint, created_at, resolved_at, is_group, favorited, resolved_by, reminder_at
		FROM commitments
		WHERE status = 'resolved' AND resolved_by = 'auto' AND resolved_at > ?
		ORDER BY resolved_at DESC
		LIMIT 10`, oneDayAgo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommitments(rows)
}

func (db *DB) RecordNudge(id string) error {
	_, err := db.conn.Exec("UPDATE commitments SET last_nudged_at = ? WHERE id = ?", time.Now().Unix(), id)
	return err
}

func (db *DB) SetReminder(id string, at time.Time) error {
	_, err := db.conn.Exec("UPDATE commitments SET reminder_at = ? WHERE id = ?", at.Unix(), id)
	return err
}

func (db *DB) ClearReminder(id string) error {
	_, err := db.conn.Exec("UPDATE commitments SET reminder_at = NULL WHERE id = ?", id)
	return err
}

// Chat muting

func (db *DB) ToggleChatMute(chatJID, chatName string) (bool, error) {
	var exists int
	db.conn.QueryRow("SELECT COUNT(*) FROM muted_chats WHERE chat_jid = ?", chatJID).Scan(&exists)
	if exists > 0 {
		_, err := db.conn.Exec("DELETE FROM muted_chats WHERE chat_jid = ?", chatJID)
		return false, err
	}
	_, err := db.conn.Exec(
		"INSERT INTO muted_chats (chat_jid, chat_name, created_at) VALUES (?, ?, ?)",
		chatJID, chatName, time.Now().Unix(),
	)
	return true, err
}

func (db *DB) IsChatMuted(chatJID string) bool {
	var count int
	db.conn.QueryRow("SELECT COUNT(*) FROM muted_chats WHERE chat_jid = ?", chatJID).Scan(&count)
	return count > 0
}

func (db *DB) GetMutedChatJIDs() (map[string]bool, error) {
	rows, err := db.conn.Query("SELECT chat_jid FROM muted_chats")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]bool)
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, err
		}
		m[jid] = true
	}
	return m, rows.Err()
}

func (db *DB) GetDueReminders() ([]*Commitment, error) {
	now := time.Now().Unix()
	rows, err := db.conn.Query(`
		SELECT id, chat_jid, chat_name, person_name, person_jid, title, context, direction,
			source_quote, source_time, message_id, status, due_hint, created_at, resolved_at, is_group, favorited, resolved_by, reminder_at
		FROM commitments
		WHERE status = 'open' AND reminder_at IS NOT NULL AND reminder_at <= ?
		ORDER BY reminder_at ASC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommitments(rows)
}
