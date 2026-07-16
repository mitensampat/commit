package store

import (
	"time"
)

// PendingClosure is a mid-confidence closure detection awaiting the user's
// confirmation. High-confidence detections auto-close; low ones are ignored.
type PendingClosure struct {
	CommitmentID string    `json:"commitment_id"`
	Confidence   float64   `json:"-"` // never shown in the UI
	Evidence     string    `json:"evidence"`
	ClosureType  string    `json:"closure_type"`
	DetectedAt   time.Time `json:"detected_at"`

	// Joined from the commitment for display.
	Title      string `json:"title"`
	PersonName string `json:"person_name"`
	ChatName   string `json:"chat_name"`
	Direction  string `json:"direction"`
}

// SavePendingClosure records a detection for user confirmation. It is a
// no-op if the commitment already has a pending closure or is not open.
func (db *DB) SavePendingClosure(commitmentID string, confidence float64, evidence, closureType string) error {
	_, err := db.conn.Exec(`
		INSERT OR IGNORE INTO pending_closures (commitment_id, confidence, evidence, closure_type, detected_at)
		SELECT id, ?, ?, ?, ? FROM commitments WHERE id = ? AND status = 'open'`,
		confidence, evidence, closureType, time.Now().Unix(), commitmentID,
	)
	return err
}

// suggestionTTL is how long a suggestion waits before expiring silently —
// the commitment stays open, and the expiry is recorded as training data.
const suggestionTTL = 7 * 24 * time.Hour

// cleanupPendingClosures lazily maintains the suggestion queue on read:
// rows whose commitment was resolved or dismissed through another path
// (Done button, extraction, staleness) are dropped, and rows older than
// suggestionTTL expire into closure_rejections with reason 'expired'.
func (db *DB) cleanupPendingClosures() {
	db.conn.Exec(`DELETE FROM pending_closures WHERE commitment_id NOT IN
		(SELECT id FROM commitments WHERE status = 'open')`)

	cutoff := time.Now().Add(-suggestionTTL).Unix()
	db.conn.Exec(`
		INSERT INTO closure_rejections (commitment_id, confidence, evidence, closure_type, detected_at, rejected_at, reason)
		SELECT commitment_id, confidence, evidence, closure_type, detected_at, ?, 'expired'
		FROM pending_closures WHERE detected_at < ?`,
		time.Now().Unix(), cutoff,
	)
	db.conn.Exec("DELETE FROM pending_closures WHERE detected_at < ?", cutoff)
}

// GetPendingClosures returns suggestions awaiting review, highest confidence
// first, joined with the commitment for display.
func (db *DB) GetPendingClosures() ([]*PendingClosure, error) {
	db.cleanupPendingClosures()
	rows, err := db.conn.Query(`
		SELECT p.commitment_id, p.confidence, p.evidence, p.closure_type, p.detected_at,
			c.title, c.person_name, c.chat_name, c.direction
		FROM pending_closures p
		JOIN commitments c ON c.id = p.commitment_id
		ORDER BY p.confidence DESC, p.detected_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*PendingClosure
	for rows.Next() {
		p := &PendingClosure{}
		var detectedAt int64
		if err := rows.Scan(&p.CommitmentID, &p.Confidence, &p.Evidence, &p.ClosureType,
			&detectedAt, &p.Title, &p.PersonName, &p.ChatName, &p.Direction); err != nil {
			return nil, err
		}
		p.DetectedAt = time.Unix(detectedAt, 0)
		out = append(out, p)
	}
	if out == nil {
		out = []*PendingClosure{}
	}
	return out, rows.Err()
}

// HasPendingClosure reports whether a detection is already waiting for this
// commitment.
func (db *DB) HasPendingClosure(commitmentID string) bool {
	var n int
	db.conn.QueryRow("SELECT COUNT(*) FROM pending_closures WHERE commitment_id = ?", commitmentID).Scan(&n)
	return n > 0
}

// DeletePendingClosure removes a pending row (used when the commitment gets
// auto-closed or confirmed).
func (db *DB) DeletePendingClosure(commitmentID string) error {
	_, err := db.conn.Exec("DELETE FROM pending_closures WHERE commitment_id = ?", commitmentID)
	return err
}

// ConfirmPendingClosure resolves the commitment as user-confirmed (the human
// made the final call) and clears the pending row.
func (db *DB) ConfirmPendingClosure(commitmentID string) error {
	if err := db.updateCommitmentStatus(commitmentID, "resolved", "user"); err != nil {
		return err
	}
	return db.DeletePendingClosure(commitmentID)
}

// RejectPendingClosure keeps the commitment open, records the explicit user
// rejection as future training data, and clears the pending row.
func (db *DB) RejectPendingClosure(commitmentID string) error {
	_, err := db.conn.Exec(`
		INSERT INTO closure_rejections (commitment_id, confidence, evidence, closure_type, detected_at, rejected_at, reason)
		SELECT commitment_id, confidence, evidence, closure_type, detected_at, ?, 'rejected'
		FROM pending_closures WHERE commitment_id = ?`,
		time.Now().Unix(), commitmentID,
	)
	if err != nil {
		return err
	}
	return db.DeletePendingClosure(commitmentID)
}
