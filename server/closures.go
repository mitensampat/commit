package server

import (
	"encoding/json"
	"net/http"
)

// handlePendingClosures returns mid-confidence closure detections awaiting
// the user's confirmation, joined with the commitment title and person.
func (s *Server) handlePendingClosures(w http.ResponseWriter, r *http.Request) {
	items, err := s.db.GetPendingClosures()
	if err != nil {
		http.Error(w, "failed to get pending closures", 500)
		return
	}
	writeJSON(w, items)
}

func (s *Server) readClosureID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return "", false
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "id required", 400)
		return "", false
	}
	return body.ID, true
}

// handleClosureConfirm resolves the commitment with resolved_by='user' —
// the human made the final call — and clears the pending row.
func (s *Server) handleClosureConfirm(w http.ResponseWriter, r *http.Request) {
	id, ok := s.readClosureID(w, r)
	if !ok {
		return
	}
	if err := s.db.ConfirmPendingClosure(id); err != nil {
		http.Error(w, "confirm failed", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleClosureReject keeps the commitment open and records the rejected
// detection in closure_rejections as future training data.
func (s *Server) handleClosureReject(w http.ResponseWriter, r *http.Request) {
	id, ok := s.readClosureID(w, r)
	if !ok {
		return
	}
	if err := s.db.RejectPendingClosure(id); err != nil {
		http.Error(w, "reject failed", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
