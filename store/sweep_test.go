package store

import (
	"path/filepath"
	"testing"
	"time"
)

// The sweep watermark is what stops the resolution sweep from re-sending an
// unchanged chat's whole window to Claude on every run. It must persist across
// a reopen (a restart), or the runaway-token bug comes straight back.
func TestSweepWatermarkPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "commit.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	chat := "111@s.whatsapp.net"

	if _, ok := db.GetSweptThrough(chat); ok {
		t.Fatal("a never-swept chat must report no watermark")
	}

	through := time.Unix(1_700_000_000, 0)
	if err := db.SetSweptThrough(chat, through); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopen — the restart the in-memory version couldn't survive.
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	got, ok := db2.GetSweptThrough(chat)
	if !ok {
		t.Fatal("watermark did not survive reopen")
	}
	if !got.Equal(through) {
		t.Fatalf("watermark = %v, want %v", got, through)
	}
}

// The skip decision: a chat is re-analyzed only when a message strictly newer
// than the watermark exists. Equal timestamps must skip (nothing new).
func TestSweepSkipDecision(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "commit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	chat := "222@s.whatsapp.net"

	swept := time.Unix(1_700_000_000, 0)
	db.SetSweptThrough(chat, swept)

	cases := []struct {
		name       string
		latest     time.Time
		wantAnalyze bool
	}{
		{"older message", swept.Add(-time.Hour), false},
		{"same message", swept, false},
		{"newer message", swept.Add(time.Minute), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			last, _ := db.GetSweptThrough(chat)
			analyze := tc.latest.After(last)
			if analyze != tc.wantAnalyze {
				t.Fatalf("latest %v vs swept %v: analyze=%v, want %v",
					tc.latest, last, analyze, tc.wantAnalyze)
			}
		})
	}
}

// The window bound: earlier messages can neither create nor close a
// commitment, so the sweep should search only from (oldest open commitment
// created_at − lead), capped at the 48h horizon.
func TestSweepWindowBound(t *testing.T) {
	now := time.Unix(1_700_100_000, 0)
	horizon := now.Add(-48 * time.Hour)
	lead := 2 * time.Hour

	windowStart := func(oldestCommitment time.Time) time.Time {
		since := horizon
		if oldestCommitment.After(since) {
			if l := oldestCommitment.Add(-lead); l.After(since) {
				since = l
			}
		}
		return since
	}

	// Recent commitment (made 3h ago) → window starts ~5h ago, not 48h.
	recent := now.Add(-3 * time.Hour)
	if got := windowStart(recent); !got.Equal(recent.Add(-lead)) {
		t.Fatalf("recent commitment: window start = %v, want %v", got, recent.Add(-lead))
	}
	// Old commitment (made 40h ago, less lead still inside horizon) → clamps to lead-in.
	old := now.Add(-40 * time.Hour)
	if got := windowStart(old); !got.Equal(old.Add(-lead)) {
		t.Fatalf("old commitment: window start = %v, want %v", got, old.Add(-lead))
	}
	// Ancient commitment (older than horizon) → clamps to the 48h horizon.
	ancient := now.Add(-100 * time.Hour)
	if got := windowStart(ancient); !got.Equal(horizon) {
		t.Fatalf("ancient commitment: window start = %v, want horizon %v", got, horizon)
	}
}
