package store

import (
	"strings"
	"time"
)

type Message struct {
	ID         string
	ChatJID    string
	SenderJID  string
	SenderName string
	ChatName   string
	Content    string
	Timestamp  time.Time
	IsFromMe   bool
	IsGroup    bool
	Processed  bool
}

func (db *DB) SaveMessage(m *Message) error {
	_, err := db.conn.Exec(`
		INSERT OR IGNORE INTO messages (id, chat_jid, sender_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group, processed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		m.ID, m.ChatJID, m.SenderJID, m.SenderName, m.ChatName, m.Content,
		m.Timestamp.Unix(), boolToInt(m.IsFromMe), boolToInt(m.IsGroup),
	)
	return err
}

func (db *DB) GetUnprocessedMessages(limit int) ([]*Message, error) {
	rows, err := db.conn.Query(`
		SELECT id, chat_jid, sender_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group
		FROM messages WHERE processed = 0
		ORDER BY timestamp ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		m := &Message{}
		var ts int64
		var fromMe, group int
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderJID, &m.SenderName, &m.ChatName,
			&m.Content, &ts, &fromMe, &group); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		m.IsFromMe = fromMe == 1
		m.IsGroup = group == 1
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (db *DB) MarkMessagesProcessed(ids []string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("UPDATE messages SET processed = 1 WHERE id = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) GetMessageStats() (total int, processed int, err error) {
	err = db.conn.QueryRow("SELECT COUNT(*) FROM messages").Scan(&total)
	if err != nil {
		return
	}
	err = db.conn.QueryRow("SELECT COUNT(*) FROM messages WHERE processed = 1").Scan(&processed)
	return
}

func (db *DB) GetRecentMessages(limit int) ([]*Message, error) {
	rows, err := db.conn.Query(`
		SELECT id, chat_jid, sender_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group
		FROM messages ORDER BY timestamp DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		m := &Message{}
		var ts int64
		var fromMe, group int
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderJID, &m.SenderName, &m.ChatName,
			&m.Content, &ts, &fromMe, &group); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		m.IsFromMe = fromMe == 1
		m.IsGroup = group == 1
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (db *DB) RequeueMessagesSince(since time.Time) (int64, error) {
	result, err := db.conn.Exec(
		"UPDATE messages SET processed = 0 WHERE timestamp >= ? AND processed = 1",
		since.Unix(),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (db *DB) GetChatsWithRecentOutbound(since time.Time) ([]string, error) {
	rows, err := db.conn.Query(`
		SELECT DISTINCT m.chat_jid FROM messages m
		JOIN commitments c ON m.chat_jid = c.chat_jid AND c.status = 'open'
		WHERE m.is_from_me = 1 AND m.timestamp >= ?`,
		since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jids []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, err
		}
		jids = append(jids, jid)
	}
	return jids, rows.Err()
}

func (db *DB) GetRecentMessagesForChat(chatJID string, since time.Time) ([]*Message, error) {
	rows, err := db.conn.Query(`
		SELECT id, chat_jid, sender_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group
		FROM messages WHERE chat_jid = ? AND timestamp >= ?
		ORDER BY timestamp ASC`, chatJID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		m := &Message{}
		var ts int64
		var fromMe, group int
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderJID, &m.SenderName, &m.ChatName,
			&m.Content, &ts, &fromMe, &group); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		m.IsFromMe = fromMe == 1
		m.IsGroup = group == 1
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (db *DB) SearchMessages(keywords []string, limit int) ([]*Message, error) {
	if len(keywords) == 0 || limit == 0 {
		return nil, nil
	}

	var conditions []string
	var args []interface{}
	for _, kw := range keywords {
		conditions = append(conditions, "(content LIKE ? OR sender_name LIKE ? OR chat_name LIKE ?)")
		pattern := "%" + kw + "%"
		args = append(args, pattern, pattern, pattern)
	}

	query := `SELECT id, chat_jid, sender_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group
		FROM messages WHERE ` + strings.Join(conditions, " OR ") + `
		ORDER BY timestamp DESC LIMIT ?`
	args = append(args, limit)

	return db.queryMessages(query, args...)
}

// SearchMessagesAND returns messages matching ALL keywords (content/sender/chat).
func (db *DB) SearchMessagesAND(keywords []string, limit int) ([]*Message, error) {
	if len(keywords) == 0 || limit == 0 {
		return nil, nil
	}

	var conditions []string
	var args []interface{}
	for _, kw := range keywords {
		conditions = append(conditions, "(content LIKE ? OR sender_name LIKE ? OR chat_name LIKE ?)")
		pattern := "%" + kw + "%"
		args = append(args, pattern, pattern, pattern)
	}

	query := `SELECT id, chat_jid, sender_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group
		FROM messages WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY timestamp DESC LIMIT ?`
	args = append(args, limit)

	return db.queryMessages(query, args...)
}

func (db *DB) queryMessages(query string, args ...interface{}) ([]*Message, error) {

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		m := &Message{}
		var ts int64
		var fromMe, group int
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderJID, &m.SenderName, &m.ChatName,
			&m.Content, &ts, &fromMe, &group); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		m.IsFromMe = fromMe == 1
		m.IsGroup = group == 1
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ResolvePerson finds chat JIDs where a person name fuzzy-matches.
// Searches both message sender names and commitment person/chat names.
func (db *DB) ResolvePerson(name string) []string {
	pattern := "%" + name + "%"
	var jids []string
	seen := map[string]bool{}

	// Direct chats from commitments (most reliable — person_name is curated)
	rows, err := db.conn.Query(`
		SELECT DISTINCT chat_jid FROM commitments
		WHERE person_name LIKE ? OR chat_name LIKE ?`, pattern, pattern)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var jid string
			rows.Scan(&jid)
			if jid != "" && !seen[jid] {
				seen[jid] = true
				jids = append(jids, jid)
			}
		}
	}

	// Also check message sender names
	rows2, err := db.conn.Query(`
		SELECT DISTINCT chat_jid FROM messages
		WHERE (sender_name LIKE ? OR chat_name LIKE ?) AND is_group = 0
		LIMIT 10`, pattern, pattern)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var jid string
			rows2.Scan(&jid)
			if jid != "" && !seen[jid] {
				seen[jid] = true
				jids = append(jids, jid)
			}
		}
	}

	return jids
}

// GetMessagesAround fetches N messages before and after a given timestamp in a chat.
func (db *DB) GetMessagesAround(chatJID string, ts int64, window int) ([]*Message, error) {
	query := `SELECT id, chat_jid, sender_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group
		FROM messages WHERE chat_jid = ? AND timestamp BETWEEN ? AND ?
		ORDER BY timestamp ASC`
	before := ts - int64(window*300)
	after := ts + int64(window*300)
	msgs, err := db.queryMessages(query, chatJID, before, after)
	if err != nil {
		return nil, err
	}
	// Trim to window messages on each side of the target timestamp
	var result []*Message
	pivot := -1
	for i, m := range msgs {
		if m.Timestamp.Unix() <= ts {
			pivot = i
		}
	}
	if pivot < 0 {
		pivot = 0
	}
	start := pivot - window
	if start < 0 {
		start = 0
	}
	end := pivot + window + 1
	if end > len(msgs) {
		end = len(msgs)
	}
	result = msgs[start:end]
	return result, nil
}

// SearchMessagesInChats searches for keywords only within specific chat JIDs.
func (db *DB) SearchMessagesInChats(chatJIDs []string, keywords []string, limit int) ([]*Message, error) {
	if len(chatJIDs) == 0 || len(keywords) == 0 || limit == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(chatJIDs))
	var args []interface{}
	for i, jid := range chatJIDs {
		placeholders[i] = "?"
		args = append(args, jid)
	}
	chatClause := "chat_jid IN (" + strings.Join(placeholders, ",") + ")"

	var kwConditions []string
	for _, kw := range keywords {
		kwConditions = append(kwConditions, "content LIKE ?")
		args = append(args, "%"+kw+"%")
	}

	query := `SELECT id, chat_jid, sender_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group
		FROM messages WHERE ` + chatClause + ` AND (` + strings.Join(kwConditions, " OR ") + `)
		ORDER BY timestamp DESC LIMIT ?`
	args = append(args, limit)

	return db.queryMessages(query, args...)
}

// GetMessageByID returns a single message by its ID.
func (db *DB) GetMessageByID(id string) (*Message, error) {
	msgs, err := db.queryMessages(
		`SELECT id, chat_jid, sender_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group
		 FROM messages WHERE id = ? LIMIT 1`, id)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	return msgs[0], nil
}

func (db *DB) GetChatDisplayName(chatJID string) string {
	var name string
	db.conn.QueryRow(
		"SELECT sender_name FROM messages WHERE chat_jid = ? AND is_from_me = 0 AND sender_name != '' ORDER BY timestamp DESC LIMIT 1",
		chatJID,
	).Scan(&name)
	return name
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
