package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const apiBase = "https://www.googleapis.com/calendar/v3"

// TokenStore abstracts encrypted token persistence (implemented by store.DB
// wrappers in the wiring layer).
type TokenStore interface {
	LoadToken() (*Token, error)
	SaveToken(*Token) error
	ClientCreds() (id, secret string)
}

// Client is a Google Calendar REST client with automatic token refresh.
// Every auth failure comes back as *AuthError so callers can be loud about
// it (hardening req 7).
type Client struct {
	Store TokenStore
	HTTP  *http.Client

	mu  sync.Mutex
	tok *Token
}

func NewClient(store TokenStore) *Client {
	return &Client{Store: store, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok == nil {
		tok, err := c.Store.LoadToken()
		if err != nil || tok == nil {
			return "", &AuthError{Step: "load", Err: fmt.Errorf("not connected: %v", err)}
		}
		c.tok = tok
	}
	if c.tok.Valid() {
		return c.tok.AccessToken, nil
	}
	id, secret := c.Store.ClientCreds()
	nt, err := Refresh(ctx, id, secret, c.tok)
	if err != nil {
		return "", err
	}
	c.tok = nt
	if err := c.Store.SaveToken(nt); err != nil {
		return "", &AuthError{Step: "save", Err: err}
	}
	return nt.AccessToken, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body interface{}, out interface{}) error {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	u := apiBase + path
	if query != nil {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("calendar api: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return &AuthError{Step: "api", Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200))}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("calendar api %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}
	if out != nil {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// CalendarInfo is one entry from the user's calendar list.
type CalendarInfo struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	Primary bool   `json:"primary"`
}

func (c *Client) ListCalendars(ctx context.Context) ([]CalendarInfo, error) {
	var out struct {
		Items []CalendarInfo `json:"items"`
	}
	if err := c.do(ctx, "GET", "/users/me/calendarList", url.Values{"minAccessRole": {"writer"}}, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

type apiEvent struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
	Start   struct {
		DateTime string `json:"dateTime"`
		Date     string `json:"date"`
	} `json:"start"`
	End struct {
		DateTime string `json:"dateTime"`
		Date     string `json:"date"`
	} `json:"end"`
	Transparency string `json:"transparency"`
	EventType    string `json:"eventType"`
}

// ListEvents fetches expanded (singleEvents) events in [timeMin, timeMax) for
// one calendar, converted to the local Event type. We use events.list rather
// than freeBusy so titles, transparency, and eventType are visible — required
// for the all-day / block-title rules (hardening req 4).
func (c *Client) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time, loc *time.Location) ([]Event, error) {
	if loc == nil {
		loc = time.Local
	}
	q := url.Values{
		"singleEvents": {"true"},
		"orderBy":      {"startTime"},
		"timeMin":      {timeMin.Format(time.RFC3339)},
		"timeMax":      {timeMax.Format(time.RFC3339)},
		"maxResults":   {"250"},
	}
	var out struct {
		Items []apiEvent `json:"items"`
	}
	if err := c.do(ctx, "GET", "/calendars/"+url.PathEscape(calendarID)+"/events", q, nil, &out); err != nil {
		return nil, err
	}
	var events []Event
	for _, it := range out.Items {
		e := Event{
			ID:           it.ID,
			Summary:      it.Summary,
			Status:       it.Status,
			Transparency: it.Transparency,
			EventType:    it.EventType,
		}
		if it.Start.Date != "" { // all-day
			e.AllDay = true
			s, err1 := time.ParseInLocation("2006-01-02", it.Start.Date, loc)
			en, err2 := time.ParseInLocation("2006-01-02", it.End.Date, loc)
			if err1 != nil || err2 != nil {
				continue
			}
			e.Start, e.End = s, en
			// Google marks regular all-day events transparent by default,
			// but older events may omit the field; default all-day to
			// transparent unless explicitly opaque or OOO.
			if e.Transparency == "" && e.EventType != "outOfOffice" {
				e.Transparency = "transparent"
			}
		} else {
			s, err1 := time.Parse(time.RFC3339, it.Start.DateTime)
			en, err2 := time.Parse(time.RFC3339, it.End.DateTime)
			if err1 != nil || err2 != nil {
				continue
			}
			e.Start, e.End = s, en
		}
		events = append(events, e)
	}
	return events, nil
}

// BusyAcross aggregates busy intervals across several calendars.
func (c *Client) BusyAcross(ctx context.Context, calendarIDs []string, timeMin, timeMax time.Time, loc *time.Location, ignoreTitles []string) ([]Interval, error) {
	var all []Event
	for _, id := range calendarIDs {
		evs, err := c.ListEvents(ctx, id, timeMin, timeMax, loc)
		if err != nil {
			return nil, err
		}
		all = append(all, evs...)
	}
	return BusyIntervals(all, ignoreTitles), nil
}

// VerifySlotFree re-checks a specific window right before booking
// (hardening reqs 2 and 10).
func (c *Client) VerifySlotFree(ctx context.Context, calendarIDs []string, start, end time.Time, loc *time.Location, ignoreTitles []string) (bool, error) {
	busy, err := c.BusyAcross(ctx, calendarIDs, start.Add(-time.Hour), end.Add(time.Hour), loc, ignoreTitles)
	if err != nil {
		return false, err
	}
	return IsFree(busy, start, end.Sub(start)), nil
}

// CreatedEvent is the result of CreateEvent.
type CreatedEvent struct {
	ID       string
	HTMLLink string
	MeetLink string
}

// CreateEvent inserts an event with a Google Meet conference when withMeet is
// set, and invites the attendee if an email is known.
func (c *Client) CreateEvent(ctx context.Context, calendarID, summary, description string, start, end time.Time, attendeeEmail string, withMeet bool) (*CreatedEvent, error) {
	body := map[string]interface{}{
		"summary":     summary,
		"description": description,
		"start":       map[string]string{"dateTime": start.Format(time.RFC3339)},
		"end":         map[string]string{"dateTime": end.Format(time.RFC3339)},
	}
	if attendeeEmail != "" {
		body["attendees"] = []map[string]string{{"email": attendeeEmail}}
	}
	q := url.Values{}
	if withMeet {
		body["conferenceData"] = map[string]interface{}{
			"createRequest": map[string]interface{}{
				"requestId":             fmt.Sprintf("commit-%d", time.Now().UnixNano()),
				"conferenceSolutionKey": map[string]string{"type": "hangoutsMeet"},
			},
		}
		q.Set("conferenceDataVersion", "1")
	}
	var out struct {
		ID         string `json:"id"`
		HTMLLink   string `json:"htmlLink"`
		HangoutLnk string `json:"hangoutLink"`
	}
	if err := c.do(ctx, "POST", "/calendars/"+url.PathEscape(calendarID)+"/events", q, body, &out); err != nil {
		return nil, err
	}
	return &CreatedEvent{ID: out.ID, HTMLLink: out.HTMLLink, MeetLink: out.HangoutLnk}, nil
}

// DeleteEvent removes a booked event (used by @schedule cancel).
func (c *Client) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	return c.do(ctx, "DELETE", "/calendars/"+url.PathEscape(calendarID)+"/events/"+url.PathEscape(eventID), nil, nil, nil)
}
