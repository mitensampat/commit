package store

import (
	"strings"
	"time"
)

// ContactNames holds every name WhatsApp knows for one identity. FullName and
// FirstName come from the user's address book (what they see in the WhatsApp
// UI); PushName is what the contact calls themselves (what message metadata
// carries). They routinely differ — "Allish Jain" vs "Allish".
type ContactNames struct {
	JID       string
	FullName  string
	FirstName string
	PushName  string
}

// Names returns every distinct non-empty name for this contact, best first.
func (c ContactNames) Names() []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range []string{c.FullName, c.PushName, c.FirstName} {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if k := strings.ToLower(n); !seen[k] {
			seen[k] = true
			out = append(out, n)
		}
	}
	return out
}

// Best returns the name the user most likely recognizes.
func (c ContactNames) Best() string {
	if names := c.Names(); len(names) > 0 {
		return names[0]
	}
	return ""
}

// SaveContactNames upserts a batch of synced contact names.
func (db *DB) SaveContactNames(batch []ContactNames) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`
		INSERT INTO contact_names (jid, full_name, first_name, push_name, synced_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			full_name  = CASE WHEN excluded.full_name  != '' THEN excluded.full_name  ELSE contact_names.full_name  END,
			first_name = CASE WHEN excluded.first_name != '' THEN excluded.first_name ELSE contact_names.first_name END,
			push_name  = CASE WHEN excluded.push_name  != '' THEN excluded.push_name  ELSE contact_names.push_name  END,
			synced_at  = excluded.synced_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, c := range batch {
		if c.JID == "" {
			continue
		}
		if _, err := stmt.Exec(c.JID, c.FullName, c.FirstName, c.PushName, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetContactNames returns synced names for one identity.
func (db *DB) GetContactNames(jid string) ContactNames {
	c := ContactNames{JID: jid}
	db.conn.QueryRow(
		"SELECT full_name, first_name, push_name FROM contact_names WHERE jid = ?", jid,
	).Scan(&c.FullName, &c.FirstName, &c.PushName)
	return c
}

// AllContactNames returns every synced contact keyed by JID.
func (db *DB) AllContactNames() (map[string]ContactNames, error) {
	rows, err := db.conn.Query("SELECT jid, full_name, first_name, push_name FROM contact_names")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ContactNames{}
	for rows.Next() {
		var c ContactNames
		if err := rows.Scan(&c.JID, &c.FullName, &c.FirstName, &c.PushName); err != nil {
			return nil, err
		}
		out[c.JID] = c
	}
	return out, rows.Err()
}

// ContactNameCount reports how many contacts have been synced.
func (db *DB) ContactNameCount() int {
	var n int
	db.conn.QueryRow("SELECT COUNT(*) FROM contact_names").Scan(&n)
	return n
}
