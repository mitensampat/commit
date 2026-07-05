package store

import "time"

// SaveSession stores a web session by token hash, purging expired rows.
func (db *DB) SaveSession(tokenHash string, expiresAt time.Time) error {
	db.conn.Exec("DELETE FROM sessions WHERE expires_at < ?", time.Now().Unix())
	_, err := db.conn.Exec(
		"INSERT OR REPLACE INTO sessions (token_hash, expires_at) VALUES (?, ?)",
		tokenHash, expiresAt.Unix())
	return err
}

// SessionValid reports whether a session token hash exists and is unexpired.
func (db *DB) SessionValid(tokenHash string) bool {
	var expiresAt int64
	err := db.conn.QueryRow(
		"SELECT expires_at FROM sessions WHERE token_hash = ?", tokenHash).Scan(&expiresAt)
	if err != nil {
		return false
	}
	return time.Now().Unix() < expiresAt
}
