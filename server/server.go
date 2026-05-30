package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/msfoundry/commit/store"
	"github.com/msfoundry/commit/whatsapp"

	waTypes "go.mau.fi/whatsmeow/types"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	db       *store.DB
	wa       *whatsapp.Client
	port     int
	mux      *http.ServeMux
	sessions sync.Map // token -> bool
}

func New(db *store.DB, wa *whatsapp.Client, port int) *Server {
	s := &Server{db: db, wa: wa, port: port}
	s.mux = http.NewServeMux()
	s.registerRoutes()
	return s
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.mux}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	err := srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) registerRoutes() {
	staticSub, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("/", http.FileServer(http.FS(staticSub)))

	// Public (no auth required)
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/auth/check", s.handleAuthCheck)
	s.mux.HandleFunc("/api/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("/api/auth/setup", s.handleAuthSetup)

	// Protected (auth required)
	s.mux.HandleFunc("/api/setup", s.requireAuth(s.handleSetup))
	s.mux.HandleFunc("/api/login", s.requireAuth(s.handleLogin))
	s.mux.HandleFunc("/api/login/qr", s.requireAuth(s.handleLoginQR))
	s.mux.HandleFunc("/api/commitments", s.requireAuth(s.handleCommitments))
	s.mux.HandleFunc("/api/commitments/grouped", s.requireAuth(s.handleCommitmentsGrouped))
	s.mux.HandleFunc("/api/commitments/search", s.requireAuth(s.handleSearch))
	s.mux.HandleFunc("/api/commitments/stats", s.requireAuth(s.handleStats))
	s.mux.HandleFunc("/api/commitments/update", s.requireAuth(s.handleUpdateCommitment))
	s.mux.HandleFunc("/api/commitments/favorite", s.requireAuth(s.handleToggleFavorite))
	s.mux.HandleFunc("/api/commitments/reply", s.requireAuth(s.handleReply))
	s.mux.HandleFunc("/api/favorites", s.requireAuth(s.handleFavoritesView))
	s.mux.HandleFunc("/api/favorites/chat", s.requireAuth(s.handleToggleChatFavorite))
	s.mux.HandleFunc("/api/followups", s.requireAuth(s.handleFollowUps))
	s.mux.HandleFunc("/api/followups/nudge", s.requireAuth(s.handleNudge))
	s.mux.HandleFunc("/api/commitments/remind", s.requireAuth(s.handleSetReminder))
	s.mux.HandleFunc("/api/logout", s.requireAuth(s.handleLogout))
}

func (s *Server) generateSession() string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	s.sessions.Store(token, true)
	return token
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.db.HasPasscode() {
			next(w, r)
			return
		}
		cookie, err := r.Cookie("commit_session")
		if err != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		if _, ok := s.sessions.Load(cookie.Value); !ok {
			http.Error(w, "unauthorized", 401)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	hasPasscode := s.db.HasPasscode()
	authenticated := false
	if hasPasscode {
		if cookie, err := r.Cookie("commit_session"); err == nil {
			_, authenticated = s.sessions.Load(cookie.Value)
		}
	} else {
		authenticated = true
	}
	writeJSON(w, map[string]any{
		"has_passcode":  hasPasscode,
		"authenticated": authenticated,
	})
}

func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.db.HasPasscode() {
		http.Error(w, "passcode already set", 400)
		return
	}
	var body struct {
		Passcode string `json:"passcode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if len(body.Passcode) < 4 {
		http.Error(w, "passcode must be at least 4 characters", 400)
		return
	}
	if err := s.db.SetPasscode(body.Passcode); err != nil {
		http.Error(w, "failed to set passcode", 500)
		return
	}

	// Re-encrypt the API key if one exists
	apiKey := s.db.GetAPIKey()
	if apiKey != "" {
		s.db.SetAPIKey(apiKey)
	}

	token := s.generateSession()
	http.SetCookie(w, &http.Cookie{
		Name:     "commit_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Passcode string `json:"passcode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if !s.db.CheckPasscode(body.Passcode) {
		http.Error(w, "wrong passcode", 401)
		return
	}

	token := s.generateSession()
	http.SetCookie(w, &http.Cookie{
		Name:     "commit_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	hasKey := s.db.GetAPIKey() != ""
	hasSession := s.wa.HasSession()
	connected := s.wa.IsConnected()

	state := "needs_setup"
	if hasKey && !hasSession {
		state = "needs_login"
	} else if hasKey && hasSession && !connected {
		state = "connecting"
	} else if hasKey && connected {
		state = "ready"
	}

	writeJSON(w, map[string]any{
		"state":       state,
		"has_api_key": hasKey,
		"has_session": hasSession,
		"connected":   connected,
	})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if body.APIKey == "" {
		http.Error(w, "api_key required", 400)
		return
	}
	if err := s.db.SetAPIKey(body.APIKey); err != nil {
		http.Error(w, "failed to save key", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	ctx := r.Context()
	qrChan, err := s.wa.Login(ctx)
	if err != nil {
		log.Printf("login error: %v", err)
		http.Error(w, "login failed", 500)
		return
	}

	qr := <-qrChan
	writeJSON(w, map[string]any{"qr": qr})
}

func (s *Server) handleLoginQR(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}

	ctx := r.Context()
	qrChan, err := s.wa.Login(ctx)
	if err != nil {
		log.Printf("login error: %v", err)
		fmt.Fprintf(w, "data: {\"error\": \"login failed\"}\n\n")
		flusher.Flush()
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case qr, ok := <-qrChan:
			if !ok {
				if s.wa.IsConnected() {
					fmt.Fprintf(w, "data: {\"connected\": true}\n\n")
				} else {
					fmt.Fprintf(w, "data: {\"error\": \"QR expired\"}\n\n")
				}
				flusher.Flush()
				return
			}
			fmt.Fprintf(w, "data: {\"qr\": \"%s\"}\n\n", qr)
			flusher.Flush()
		}
	}
}

func (s *Server) handleCommitments(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "open"
	}
	commitments, err := s.db.GetCommitments(status)
	if err != nil {
		http.Error(w, "failed to get commitments", 500)
		return
	}
	if commitments == nil {
		commitments = []*store.Commitment{}
	}
	writeJSON(w, commitments)
}

func (s *Server) handleCommitmentsGrouped(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "open"
	}
	groups, err := s.db.GetCommitmentsGrouped(status)
	if err != nil {
		http.Error(w, "failed to get commitments", 500)
		return
	}
	if groups == nil {
		groups = []*store.CommitmentGroup{}
	}
	writeJSON(w, groups)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeJSON(w, []*store.Commitment{})
		return
	}
	results, err := s.db.SearchCommitments(query)
	if err != nil {
		http.Error(w, "search failed", 500)
		return
	}
	if results == nil {
		results = []*store.Commitment{}
	}
	writeJSON(w, results)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.GetCommitmentStats()
	if err != nil {
		http.Error(w, "failed to get stats", 500)
		return
	}
	totalMsgs, processedMsgs, _ := s.db.GetMessageStats()
	writeJSON(w, map[string]any{
		"open":               stats.Open,
		"you_owe":            stats.YouOwe,
		"they_owe":           stats.TheyOwe,
		"resolved":           stats.Resolved,
		"favorites":          stats.Favorites,
		"follow_ups":         stats.FollowUps,
		"total_messages":     totalMsgs,
		"processed_messages": processedMsgs,
	})
}

func (s *Server) handleUpdateCommitment(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := s.db.UpdateCommitmentStatus(body.ID, body.Status); err != nil {
		http.Error(w, "update failed", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSetReminder(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		ID        string `json:"id"`
		RemindAt  string `json:"remind_at"` // ISO 8601 or empty to clear
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if body.ID == "" {
		http.Error(w, "id required", 400)
		return
	}
	if body.RemindAt == "" {
		if err := s.db.ClearReminder(body.ID); err != nil {
			http.Error(w, "clear failed", 500)
			return
		}
	} else {
		t, err := time.Parse(time.RFC3339, body.RemindAt)
		if err != nil {
			t, err = time.Parse("2006-01-02T15:04", body.RemindAt)
			if err != nil {
				http.Error(w, "invalid remind_at format", 400)
				return
			}
		}
		if err := s.db.SetReminder(body.ID, t); err != nil {
			http.Error(w, "set failed", 500)
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		ChatJID string `json:"chat_jid"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if body.ChatJID == "" || body.Message == "" {
		http.Error(w, "chat_jid and message required", 400)
		return
	}

	jid, err := waTypes.ParseJID(body.ChatJID)
	if err != nil {
		http.Error(w, "invalid chat_jid", 400)
		return
	}

	if err := s.wa.SendMessage(r.Context(), jid, body.Message); err != nil {
		log.Printf("send message error: %v", err)
		http.Error(w, "failed to send", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleToggleFavorite(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	favorited, err := s.db.ToggleFavorite(body.ID)
	if err != nil {
		http.Error(w, "update failed", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "favorited": favorited})
}

func (s *Server) handleFavoritesView(w http.ResponseWriter, r *http.Request) {
	view, err := s.db.GetFavoritesView()
	if err != nil {
		http.Error(w, "failed to get favorites", 500)
		return
	}
	writeJSON(w, view)
}

func (s *Server) handleToggleChatFavorite(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		ChatJID  string `json:"chat_jid"`
		ChatName string `json:"chat_name"`
		IsGroup  bool   `json:"is_group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if body.ChatJID == "" {
		http.Error(w, "chat_jid required", 400)
		return
	}
	favorited, err := s.db.ToggleChatFavorite(body.ChatJID, body.ChatName, body.IsGroup)
	if err != nil {
		http.Error(w, "update failed", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "favorited": favorited})
}

func (s *Server) handleFollowUps(w http.ResponseWriter, r *http.Request) {
	followUps, err := s.db.GetFollowUps()
	if err != nil {
		http.Error(w, "failed to get follow-ups", 500)
		return
	}
	if followUps == nil {
		followUps = []*store.Commitment{}
	}
	writeJSON(w, followUps)
}

func (s *Server) handleNudge(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		ID     string `json:"id"`
		Action string `json:"action"` // "draft" or "record"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}

	if body.Action == "record" {
		s.db.RecordNudge(body.ID)
		writeJSON(w, map[string]any{"ok": true})
		return
	}

	commitments, err := s.db.GetCommitments("")
	if err != nil {
		http.Error(w, "failed to get commitments", 500)
		return
	}
	var target *store.Commitment
	for _, c := range commitments {
		if c.ID == body.ID {
			target = c
			break
		}
	}
	if target == nil {
		http.Error(w, "commitment not found", 404)
		return
	}

	apiKey := s.db.GetAPIKey()
	if apiKey == "" {
		http.Error(w, "no API key", 400)
		return
	}

	msg, err := generateNudgeMessage(r.Context(), apiKey, target)
	if err != nil {
		log.Printf("nudge draft error: %v", err)
		http.Error(w, "failed to generate nudge", 500)
		return
	}

	writeJSON(w, map[string]any{"message": msg})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if err := s.wa.Logout(); err != nil {
		http.Error(w, "logout failed", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
