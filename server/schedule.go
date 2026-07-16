package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/msfoundry/commit/calendar"
	"github.com/msfoundry/commit/schedule"
	"github.com/msfoundry/commit/store"
)

// Scheduling endpoints: settings, Google OAuth connect flow, calendar picker,
// and the open-sessions feed consumed by the Today-page strip.

func (s *Server) registerScheduleRoutes() {
	s.mux.HandleFunc("/api/schedule/sessions", s.requireAuth(s.handleScheduleSessions))
	s.mux.HandleFunc("/api/schedule/settings", s.requireAuth(s.handleScheduleSettings))
	s.mux.HandleFunc("/api/schedule/oauth/start", s.requireAuth(s.handleScheduleOAuthStart))
	s.mux.HandleFunc("/api/schedule/oauth/status", s.requireAuth(s.handleScheduleOAuthStatus))
	s.mux.HandleFunc("/api/schedule/disconnect", s.requireAuth(s.handleScheduleDisconnect))
	s.mux.HandleFunc("/api/schedule/calendars", s.requireAuth(s.handleScheduleCalendars))
}

// uiState maps engine states onto the three the Today strip understands.
func uiState(state string) string {
	switch schedule.State(state) {
	case schedule.StateAwaitingReply:
		return "awaiting_reply"
	case schedule.StateReplySurfaced, schedule.StateConfirmCancel:
		return "confirming"
	default: // resolving, slots_proposed
		return "drafting"
	}
}

// handleScheduleSessions returns OPEN sessions only. Contract (fixed — the
// home-schedule-ui branch consumes these exact field names):
//
//	{"sessions":[{"contact_name":"...","chat_jid":"...","state":"...",
//	  "topic":"...","proposed_slots":["Tue 8, 3:00 PM"],"updated_at":"RFC3339"}]}
func (s *Server) handleScheduleSessions(w http.ResponseWriter, r *http.Request) {
	type sessionOut struct {
		ContactName   string   `json:"contact_name"`
		ChatJID       string   `json:"chat_jid"`
		State         string   `json:"state"`
		Topic         string   `json:"topic"`
		ProposedSlots []string `json:"proposed_slots"`
		UpdatedAt     string   `json:"updated_at"`
	}
	out := struct {
		Sessions []sessionOut `json:"sessions"`
	}{Sessions: []sessionOut{}}

	rows, err := s.db.GetOpenScheduleSessions()
	if err != nil {
		// Missing tables or any read error → empty list, not a 500.
		writeJSON(w, out)
		return
	}
	prefs := s.db.GetSchedulePrefs()
	loc := time.Local
	if prefs.Timezone != "" {
		if l, lerr := time.LoadLocation(prefs.Timezone); lerr == nil {
			loc = l
		}
	}
	for _, row := range rows {
		var sess schedule.Session
		if err := json.Unmarshal([]byte(row.Data), &sess); err != nil {
			continue
		}
		so := sessionOut{
			ContactName:   row.ContactName,
			ChatJID:       row.ContactJID,
			State:         uiState(row.State),
			Topic:         sess.Topic,
			ProposedSlots: []string{},
			UpdatedAt:     row.UpdatedAt.Format(time.RFC3339),
		}
		for _, sl := range sess.Slots {
			so.ProposedSlots = append(so.ProposedSlots, schedule.FormatSlotShort(sl, loc))
		}
		out.Sessions = append(out.Sessions, so)
	}
	writeJSON(w, out)
}

// handleScheduleSettings reads/writes scheduling prefs + Google client creds.
// The client secret is write-only: reads report only whether one is set.
func (s *Server) handleScheduleSettings(w http.ResponseWriter, r *http.Request) {
	type settingsOut struct {
		store.SchedulePrefs
		GoogleClientID string `json:"google_client_id"`
		HasSecret      bool   `json:"has_secret"`
		Connected      bool   `json:"connected"`
	}
	if r.Method == "GET" {
		writeJSON(w, settingsOut{
			SchedulePrefs:  s.db.GetSchedulePrefs(),
			GoogleClientID: s.db.GetSetting("google_client_id"),
			HasSecret:      s.db.GetSetting("google_client_secret") != "",
			Connected:      s.db.GetGoogleToken() != "",
		})
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		store.SchedulePrefs
		GoogleClientID     *string `json:"google_client_id"`
		GoogleClientSecret *string `json:"google_client_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if err := s.db.SetSchedulePrefs(body.SchedulePrefs); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if body.GoogleClientID != nil {
		s.db.SetSetting("google_client_id", *body.GoogleClientID)
	}
	if body.GoogleClientSecret != nil && *body.GoogleClientSecret != "" {
		if err := s.db.SetGoogleClientSecret(*body.GoogleClientSecret); err != nil {
			http.Error(w, "could not store secret (unlock Commit first): "+err.Error(), 500)
			return
		}
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// The in-flight OAuth flow (one at a time).
var (
	oauthMu    sync.Mutex
	oauthErr   string
	oauthBusy  bool
)

// handleScheduleOAuthStart begins the authorization-code flow: it binds a
// loopback port, returns the consent URL for the browser to open, and stores
// the encrypted token when the callback lands.
func (s *Server) handleScheduleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	clientID := s.db.GetSetting("google_client_id")
	secret := s.db.GetGoogleClientSecret()
	if clientID == "" || secret == "" {
		http.Error(w, "Set the Google client ID and secret first.", 400)
		return
	}
	oauthMu.Lock()
	if oauthBusy {
		oauthMu.Unlock()
		http.Error(w, "an authorization is already in progress", 409)
		return
	}
	oauthBusy = true
	oauthErr = ""
	oauthMu.Unlock()

	flow, authURL, err := calendar.StartFlow(clientID, secret)
	if err != nil {
		oauthMu.Lock()
		oauthBusy = false
		oauthErr = err.Error()
		oauthMu.Unlock()
		http.Error(w, err.Error(), 500)
		return
	}
	go func() {
		defer func() {
			oauthMu.Lock()
			oauthBusy = false
			oauthMu.Unlock()
		}()
		// NOT r.Context(): the request context dies when this handler
		// returns, long before the user finishes Google's consent screen.
		tok, err := flow.Wait(context.Background())
		if err != nil {
			log.Printf("google oauth: %v", err)
			oauthMu.Lock()
			oauthErr = err.Error()
			oauthMu.Unlock()
			return
		}
		if err := schedule.NewTokenStore(s.db).SaveToken(tok); err != nil {
			log.Printf("google oauth: token received but could not be stored: %v", err)
			oauthMu.Lock()
			oauthErr = "token received but could not be stored: " + err.Error()
			oauthMu.Unlock()
			return
		}
		log.Printf("google oauth: calendar connected, token stored")
	}()
	writeJSON(w, map[string]string{"auth_url": authURL})
}

func (s *Server) handleScheduleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	oauthMu.Lock()
	busy, errMsg := oauthBusy, oauthErr
	oauthMu.Unlock()
	writeJSON(w, map[string]interface{}{
		"connected": s.db.GetGoogleToken() != "",
		"pending":   busy,
		"error":     errMsg,
	})
}

func (s *Server) handleScheduleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.db.SetGoogleToken("")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleScheduleCalendars(w http.ResponseWriter, r *http.Request) {
	client := calendar.NewClient(schedule.NewTokenStore(s.db))
	cals, err := client.ListCalendars(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	writeJSON(w, map[string]interface{}{"calendars": cals})
}
