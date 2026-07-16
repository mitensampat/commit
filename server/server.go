package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/msfoundry/commit/extraction"
	"github.com/msfoundry/commit/store"
	"github.com/msfoundry/commit/whatsapp"

	qrcode "github.com/skip2/go-qrcode"
	waTypes "go.mau.fi/whatsmeow/types"
)

const AppVersion = "1.4.1"

//go:embed static
var staticFS embed.FS

type Server struct {
	db           *store.DB
	wa           *whatsapp.Client
	extractor    *extraction.Extractor
	port         int
	mux          *http.ServeMux
	startedAt    time.Time
	loginAttempts sync.Map // ip -> *loginThrottle
}

type loginThrottle struct {
	failures int
	lastFail time.Time
}

func New(db *store.DB, wa *whatsapp.Client, ext *extraction.Extractor, port int) *Server {
	s := &Server{db: db, wa: wa, extractor: ext, port: port, startedAt: time.Now()}
	s.mux = http.NewServeMux()
	s.registerRoutes()
	go s.cleanupThrottles()
	return s
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.securityHeaders(s.mux)}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	err := srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self' https://mitensampat.github.io;")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cleanupThrottles() {
	for {
		time.Sleep(15 * time.Minute)
		s.loginAttempts.Range(func(key, value any) bool {
			t := value.(*loginThrottle)
			if time.Since(t.lastFail) > 15*time.Minute {
				s.loginAttempts.Delete(key)
			}
			return true
		})
	}
}

func (s *Server) requireJSON(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" {
			ct := r.Header.Get("Content-Type")
			if len(ct) < 16 || ct[:16] != "application/json" {
				http.Error(w, "Content-Type must be application/json", 415)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) registerRoutes() {
	staticSub, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("/", http.FileServer(http.FS(staticSub)))

	// Public (no auth required)
	s.mux.HandleFunc("/api/qr", s.handleQR)
	s.mux.HandleFunc("/api/version", s.handleVersion)
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/auth/check", s.handleAuthCheck)
	s.mux.HandleFunc("/api/auth/login", s.requireJSON(s.handleAuthLogin))
	s.mux.HandleFunc("/api/auth/setup", s.requireJSON(s.handleAuthSetup))

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
	s.mux.HandleFunc("/api/commitments/auto-resolved", s.requireAuth(s.handleAutoResolved))
	s.mux.HandleFunc("/api/chats/mute", s.requireAuth(s.handleToggleChatMute))
	s.mux.HandleFunc("/api/chats/muted", s.requireAuth(s.handleMutedChats))
	s.mux.HandleFunc("/api/commitments/remind", s.requireAuth(s.handleSetReminder))
	s.mux.HandleFunc("/api/local-ip", s.requireAuth(s.handleLocalIP))
	s.mux.HandleFunc("/api/user-name", s.requireAuth(s.handleUserName))
	s.mux.HandleFunc("/api/setup/validate", s.requireAuth(s.handleValidateKey))
	s.mux.HandleFunc("/api/model", s.requireAuth(s.handleModel))
	s.mux.HandleFunc("/api/my-style", s.requireAuth(s.handleMyStyle))
	s.mux.HandleFunc("/api/commitments/detailed-stats", s.requireAuth(s.handleDetailedStats))
	s.mux.HandleFunc("/api/commitments/stale", s.requireAuth(s.handleStaleCommitments))
	s.mux.HandleFunc("/api/contacts", s.requireAuth(s.handleContacts))
	s.mux.HandleFunc("/api/setup/update-key", s.requireAuth(s.handleUpdateKey))
	s.mux.HandleFunc("/api/backfill", s.requireAuth(s.handleBackfill))
	s.mux.HandleFunc("/api/debug", s.requireAuth(s.handleDebug))
	s.mux.HandleFunc("/api/find", s.requireAuth(s.handleFind))
	s.mux.HandleFunc("/api/commitments/context", s.requireAuth(s.handleCommitmentContext))
	s.mux.HandleFunc("/api/today", s.requireAuth(s.handleToday))
	s.mux.HandleFunc("/api/commitments/snooze", s.requireAuth(s.handleSnooze))
	s.mux.HandleFunc("/api/closures/pending", s.requireAuth(s.handlePendingClosures))
	s.mux.HandleFunc("/api/closures/confirm", s.requireAuth(s.handleClosureConfirm))
	s.mux.HandleFunc("/api/closures/reject", s.requireAuth(s.handleClosureReject))
	s.mux.HandleFunc("/api/resolution-sweep", s.handleResolutionSweep)
	s.mux.HandleFunc("/api/logout", s.requireAuth(s.handleLogout))
}

func (s *Server) generateSession() string {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	token := hex.EncodeToString(b)
	if err := s.db.SaveSession(hashToken(token), time.Now().Add(30*24*time.Hour)); err != nil {
		log.Printf("save session error: %v", err)
	}
	return token
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Server) sessionCookie(token string, r *http.Request) *http.Cookie {
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	return &http.Cookie{
		Name:     "commit_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	}
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.db.HasPasscode() {
			writeJSON(w, map[string]any{"error": "setup_required", "message": "set a passcode first"})
			return
		}
		cookie, err := r.Cookie("commit_session")
		if err != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		if !s.db.SessionValid(hashToken(cookie.Value)) {
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
			authenticated = s.db.SessionValid(hashToken(cookie.Value))
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
	if len(body.Passcode) < 6 {
		http.Error(w, "passcode must be at least 6 characters", 400)
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
	http.SetCookie(w, s.sessionCookie(token, r))
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
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if val, ok := s.loginAttempts.Load(ip); ok {
		t := val.(*loginThrottle)
		if t.failures >= 5 && time.Since(t.lastFail) < time.Duration(t.failures)*10*time.Second {
			http.Error(w, "too many attempts, try again later", 429)
			return
		}
	}

	if !s.db.CheckPasscode(body.Passcode) {
		val, _ := s.loginAttempts.LoadOrStore(ip, &loginThrottle{})
		t := val.(*loginThrottle)
		t.failures++
		t.lastFail = time.Now()
		http.Error(w, "wrong passcode", 401)
		return
	}
	s.loginAttempts.Delete(ip)

	token := s.generateSession()
	http.SetCookie(w, s.sessionCookie(token, r))
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"version": AppVersion})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	hasKey := s.db.GetAPIKey() != ""
	hasSession := s.wa.HasSession()
	connected := s.wa.IsConnected()
	hasPasscode := s.db.HasPasscode()

	state := "needs_setup"
	if !hasPasscode {
		state = "needs_passcode"
	} else if hasKey && !hasSession {
		state = "needs_login"
	} else if hasKey && hasSession && !connected {
		state = "connecting"
	} else if hasKey && connected {
		state = "ready"
	}

	writeJSON(w, map[string]any{
		"state": state,
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

func (s *Server) handleValidateKey(w http.ResponseWriter, r *http.Request) {
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

	model := s.db.GetModel()
	valid, detectedModel := s.validateKeyWithModel(r.Context(), body.APIKey, model)
	if !valid {
		writeJSON(w, map[string]any{"valid": false, "error": "invalid API key"})
		return
	}
	if detectedModel != model {
		s.db.SetModel(detectedModel)
	}
	writeJSON(w, map[string]any{"valid": true, "model": detectedModel})
}

func (s *Server) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) validateKeyWithModel(ctx context.Context, apiKey, model string) (bool, string) {
	models := []string{model}
	if model != store.FallbackModel {
		models = append(models, store.FallbackModel)
	}
	for _, m := range models {
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages",
			bytes.NewReader([]byte(fmt.Sprintf(`{"model":"%s","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, m))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// 401 = bad key, definitely invalid
		if resp.StatusCode == 401 {
			return false, ""
		}
		// 404 with not_found_error = model doesn't exist, try next
		if resp.StatusCode == 404 && strings.Contains(string(respBody), "not_found_error") {
			continue
		}
		// Anything else (200, 400, 429, 529) = key is valid, model works
		return true, m
	}
	// All models returned 404 — key might still be valid, accept with fallback
	return true, store.FallbackModel
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		writeJSON(w, map[string]string{"model": s.db.GetModel()})
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
		http.Error(w, "model required", 400)
		return
	}
	if err := s.db.SetModel(body.Model); err != nil {
		http.Error(w, "failed to save", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "model": body.Model})
}

func (s *Server) handleMyStyle(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		writeJSON(w, map[string]string{"style": s.db.GetMyStyle()})
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Style string `json:"style"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if len(body.Style) > 500 {
		body.Style = body.Style[:500]
	}
	if err := s.db.SetMyStyle(body.Style); err != nil {
		http.Error(w, "failed to save", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleContacts(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		contacts, err := s.db.GetChatContacts()
		if err != nil {
			http.Error(w, "failed to get contacts", 500)
			return
		}
		if contacts == nil {
			contacts = []store.ChatContact{}
		}
		writeJSON(w, contacts)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		ChatJID string `json:"chat_jid"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	overrides := s.db.GetContactOverrides()
	if body.Name == "" {
		delete(overrides, body.ChatJID)
	} else {
		overrides[body.ChatJID] = body.Name
	}
	if err := s.db.SetContactOverrides(overrides); err != nil {
		http.Error(w, "failed to save", 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDetailedStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.GetDetailedStats()
	if err != nil {
		http.Error(w, "failed to get stats", 500)
		return
	}
	totalMsgs, processedMsgs, _ := s.db.GetMessageStats()
	writeJSON(w, map[string]any{
		"stats":              stats,
		"total_messages":     totalMsgs,
		"processed_messages": processedMsgs,
	})
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
					flusher.Flush()

					s.wa.SendWelcomeMessages(ctx, func(stage string) {
						fmt.Fprintf(w, "data: {\"stage\": \"%s\"}\n\n", stage)
						flusher.Flush()
					})

					fmt.Fprintf(w, "data: {\"done\": true}\n\n")
					flusher.Flush()
				} else {
					fmt.Fprintf(w, "data: {\"error\": \"QR expired\"}\n\n")
					flusher.Flush()
				}
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

func (s *Server) handleAutoResolved(w http.ResponseWriter, r *http.Request) {
	items, err := s.db.GetRecentlyAutoResolved()
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	if items == nil {
		items = []*store.Commitment{}
	}
	writeJSON(w, items)
}

func (s *Server) handleStaleCommitments(w http.ResponseWriter, r *http.Request) {
	items, err := s.db.GetStaleCommitments(14)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	if items == nil {
		items = []*store.Commitment{}
	}
	writeJSON(w, items)
}

func (s *Server) handleToggleChatMute(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		ChatJID  string `json:"chat_jid"`
		ChatName string `json:"chat_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	muted, err := s.db.ToggleChatMute(body.ChatJID, body.ChatName)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	writeJSON(w, map[string]any{"muted": muted})
}

func (s *Server) handleMutedChats(w http.ResponseWriter, r *http.Request) {
	muted, err := s.db.GetMutedChatJIDs()
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	writeJSON(w, muted)
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

func (s *Server) handleLocalIP(w http.ResponseWriter, r *http.Request) {
	ip := "127.0.0.1"
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					ip = ipNet.IP.String()
					writeJSON(w, map[string]string{"ip": ip})
					return
				}
			}
		}
	}
	writeJSON(w, map[string]string{"ip": ip})
}

func (s *Server) handleUserName(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		name := s.db.GetSetting("user_name")
		writeJSON(w, map[string]string{"name": name})
		return
	}
	if r.Method == "POST" {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if err := s.db.SetSetting("user_name", body.Name); err != nil {
			http.Error(w, "failed", 500)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	http.Error(w, "method not allowed", 405)
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

	if body.Action == "draft-tiers" {
		tiers, err := s.callNudgeTiersWithFallback(r.Context(), apiKey, target)
		if err != nil {
			log.Printf("nudge tiers error: %v", err)
			http.Error(w, "failed to generate nudge tiers", 500)
			return
		}
		writeJSON(w, map[string]any{"tiers": tiers})
		return
	}

	msg, err := s.callNudgeWithFallback(r.Context(), apiKey, target)
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

func (s *Server) handleBackfill(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Since string `json:"since"` // YYYY-MM-DD
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"error": "invalid request"})
		return
	}
	since, err := time.Parse("2006-01-02", req.Since)
	if err != nil {
		writeJSON(w, map[string]any{"error": "invalid date format, use YYYY-MM-DD"})
		return
	}
	count, err := s.db.RequeueMessagesSince(since)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("backfill: requeued %d messages since %s", count, req.Since)
	writeJSON(w, map[string]any{"ok": true, "requeued": count})
}

func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request) {
	totalMsgs, processedMsgs, _ := s.db.GetMessageStats()
	extractionStatus := s.extractor.GetDebugStatus()

	recentMsgs, _ := s.db.GetRecentMessages(10)
	var recentList []map[string]any
	for _, m := range recentMsgs {
		recentList = append(recentList, map[string]any{
			"chat_name":   m.ChatName,
			"sender_name": m.SenderName,
			"timestamp":   m.Timestamp.Format(time.RFC3339),
			"is_from_me":  m.IsFromMe,
			"is_group":    m.IsGroup,
		})
	}

	writeJSON(w, map[string]any{
		"uptime":     time.Since(s.startedAt).String(),
		"started_at": s.startedAt.Format(time.RFC3339),
		"whatsapp": map[string]any{
			"connected":   s.wa.IsConnected(),
			"has_session": s.wa.HasSession(),
		},
		"messages": map[string]any{
			"total":       totalMsgs,
			"processed":   processedMsgs,
			"unprocessed": totalMsgs - processedMsgs,
		},
		"extraction":      extractionStatus,
		"recent_messages": recentList,
	})
}

func (s *Server) handleFind(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, "q parameter required", 400)
		return
	}
	result, err := s.extractor.Find(r.Context(), query)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, result)
}

func (s *Server) handleCommitmentContext(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id parameter required", 400)
		return
	}

	commitment, err := s.db.GetCommitmentByID(id)
	if err != nil || commitment == nil {
		http.Error(w, "commitment not found", 404)
		return
	}

	type contextMsg struct {
		Sender    string `json:"sender"`
		Content   string `json:"content"`
		Timestamp string `json:"timestamp"`
		IsFromMe  bool   `json:"is_from_me"`
		IsSource  bool   `json:"is_source"`
	}

	var messages []contextMsg

	// Try to find the source message and get surrounding context
	if commitment.MessageID != "" {
		msg, _ := s.db.GetMessageByID(commitment.MessageID)
		if msg != nil {
			surrounding, _ := s.db.GetMessagesAround(msg.ChatJID, msg.Timestamp.Unix(), 5)
			for _, m := range surrounding {
				sender := m.SenderName
				if m.IsFromMe {
					sender = "You"
				}
				messages = append(messages, contextMsg{
					Sender:    sender,
					Content:   m.Content,
					Timestamp: m.Timestamp.Format("Jan 2, 3:04 PM"),
					IsFromMe:  m.IsFromMe,
					IsSource:  m.ID == commitment.MessageID,
				})
			}
		}
	}

	// Fallback: search by source_quote in the chat
	if len(messages) == 0 && commitment.SourceQuote != "" && commitment.ChatJID != "" {
		keywords := []string{commitment.SourceQuote}
		if len(commitment.SourceQuote) > 60 {
			keywords = []string{commitment.SourceQuote[:60]}
		}
		found, _ := s.db.SearchMessagesInChats([]string{commitment.ChatJID}, keywords, 1)
		if len(found) > 0 {
			surrounding, _ := s.db.GetMessagesAround(found[0].ChatJID, found[0].Timestamp.Unix(), 5)
			for _, m := range surrounding {
				sender := m.SenderName
				if m.IsFromMe {
					sender = "You"
				}
				messages = append(messages, contextMsg{
					Sender:    sender,
					Content:   m.Content,
					Timestamp: m.Timestamp.Format("Jan 2, 3:04 PM"),
					IsFromMe:  m.IsFromMe,
					IsSource:  false,
				})
			}
		}
	}

	writeJSON(w, map[string]any{"messages": messages})
}

func (s *Server) handleResolutionSweep(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST required", 405)
		return
	}
	s.extractor.RunStalenessCheck()
	err := s.extractor.RunResolutionSweep(context.Background())
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "resolution sweep complete"})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	data := r.URL.Query().Get("data")
	if data == "" {
		http.Error(w, "data required", 400)
		return
	}
	png, err := qrcode.Encode(data, qrcode.Medium, 200)
	if err != nil {
		http.Error(w, "qr generation failed", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
