package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth2 authorization-code flow with a local loopback redirect, implemented
// directly against Google's endpoints (no extra dependencies). Client
// credentials come from the settings table (google_client_id /
// google_client_secret); the token is stored AES-encrypted via the store.

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	calendarScope  = "https://www.googleapis.com/auth/calendar"
)

// Token is the persisted OAuth token (JSON, encrypted at rest).
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

func (t *Token) Valid() bool {
	return t != nil && t.AccessToken != "" && time.Now().Before(t.Expiry.Add(-30*time.Second))
}

// AuthError is a loud, actionable OAuth failure (hardening req 7): every
// caller surfaces it with re-auth instructions rather than swallowing it.
type AuthError struct {
	Step string
	Err  error
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("Google Calendar auth failed (%s): %v — reconnect in Settings → Scheduling → Connect Google Calendar", e.Step, e.Err)
}
func (e *AuthError) Unwrap() error { return e.Err }

// Flow is one in-progress authorization-code flow.
type Flow struct {
	ClientID     string
	ClientSecret string
	listener     net.Listener
	redirectURI  string
	state        string
	result       chan flowResult
}

type flowResult struct {
	tok *Token
	err error
}

// StartFlow binds a loopback port, returns the consent URL to open, and
// captures the callback in the background. Call Wait to get the token.
func StartFlow(clientID, clientSecret string) (*Flow, string, error) {
	if clientID == "" || clientSecret == "" {
		return nil, "", &AuthError{Step: "config", Err: fmt.Errorf("client ID/secret not configured")}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", &AuthError{Step: "listen", Err: err}
	}
	f := &Flow{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		listener:     ln,
		redirectURI:  fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", ln.Addr().(*net.TCPAddr).Port),
		state:        fmt.Sprintf("st%d", time.Now().UnixNano()),
		result:       make(chan flowResult, 1),
	}

	params := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {f.redirectURI},
		"response_type": {"code"},
		"scope":         {calendarScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {f.state},
	}
	authURL := googleAuthURL + "?" + params.Encode()

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != f.state {
			http.Error(w, "state mismatch", 400)
			f.finish(nil, fmt.Errorf("state mismatch"))
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "authorization denied: "+e, 400)
			f.finish(nil, fmt.Errorf("authorization denied: %s", e))
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "no code", 400)
			f.finish(nil, fmt.Errorf("no code in callback"))
			return
		}
		tok, err := exchangeCode(r.Context(), clientID, clientSecret, code, f.redirectURI)
		if err != nil {
			http.Error(w, "token exchange failed: "+err.Error(), 500)
			f.finish(nil, err)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body style='font-family:sans-serif;padding:40px'><h3>Google Calendar connected.</h3>You can close this tab and go back to Commit.</body></html>")
		f.finish(tok, nil)
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	go func() {
		// Whole flow times out after 5 minutes.
		time.Sleep(5 * time.Minute)
		f.finish(nil, fmt.Errorf("authorization timed out"))
		srv.Close()
	}()
	return f, authURL, nil
}

func (f *Flow) finish(tok *Token, err error) {
	select {
	case f.result <- flowResult{tok, err}:
		f.listener.Close()
	default:
	}
}

// Wait blocks until the callback arrives, the flow times out, or ctx ends.
func (f *Flow) Wait(ctx context.Context) (*Token, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-f.result:
		if r.err != nil {
			return nil, &AuthError{Step: "callback", Err: r.err}
		}
		return r.tok, nil
	}
}

func exchangeCode(ctx context.Context, clientID, clientSecret, code, redirectURI string) (*Token, error) {
	return tokenRequest(ctx, url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	})
}

// Refresh exchanges the refresh token for a fresh access token.
func Refresh(ctx context.Context, clientID, clientSecret string, tok *Token) (*Token, error) {
	if tok == nil || tok.RefreshToken == "" {
		return nil, &AuthError{Step: "refresh", Err: fmt.Errorf("no refresh token stored")}
	}
	nt, err := tokenRequest(ctx, url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {tok.RefreshToken},
		"grant_type":    {"refresh_token"},
	})
	if err != nil {
		return nil, &AuthError{Step: "refresh", Err: err}
	}
	if nt.RefreshToken == "" {
		nt.RefreshToken = tok.RefreshToken
	}
	return nt, nil
}

func tokenRequest(ctx context.Context, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token endpoint %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &Token{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(out.ExpiresIn) * time.Second),
	}, nil
}
