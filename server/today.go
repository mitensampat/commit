package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/msfoundry/commit/store"
)

// handleToday returns the "Act on today" list: at most 5 open you_owe
// commitments ranked by deadline, promise age, going-cold and favorites
// signals, plus the counts for the trust line under the list.
func (s *Server) handleToday(w http.ResponseWriter, r *http.Request) {
	cands, err := s.db.GetTodayCandidates()
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	items := store.RankToday(cands, time.Now(), 5)
	if items == nil {
		items = []*store.TodayItem{}
	}

	weekAgo := time.Now().Add(-7 * 24 * time.Hour)
	snoozed, _ := s.db.GetSnoozedCount()
	autoClosed, _ := s.db.GetAutoResolvedCountSince(weekAgo)
	resolvedWeek, _ := s.db.GetResolvedCountSince(weekAgo)
	pendingClosures, _ := s.db.GetPendingClosures()
	if pendingClosures == nil {
		pendingClosures = []*store.PendingClosure{}
	}

	writeJSON(w, map[string]any{
		"items":            items,
		"snoozed":          snoozed,
		"auto_closed_week": autoClosed,
		"resolved_week":    resolvedWeek,
		"pending_closures": pendingClosures,
	})
}

// handleSnooze hides a commitment for a week by setting its reminder,
// matching the nudge tenet: if ignored, wait a week before resurfacing.
func (s *Server) handleSnooze(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		ID   string `json:"id"`
		Days int    `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if body.ID == "" {
		http.Error(w, "id required", 400)
		return
	}
	if body.Days <= 0 {
		body.Days = 7
	}
	until := time.Now().Add(time.Duration(body.Days) * 24 * time.Hour)
	if err := s.db.SetSnooze(body.ID, until); err != nil {
		http.Error(w, "snooze failed", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "until": until.Format(time.RFC3339)})
}
