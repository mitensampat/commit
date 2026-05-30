package store

import "time"

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
