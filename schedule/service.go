package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/msfoundry/commit/calendar"
	"github.com/msfoundry/commit/store"
)

// CalendarService is what the manager needs from a calendar. The production
// implementation wraps the Google client; the dry-run uses a fake.
type CalendarService interface {
	Connected() bool
	// ComputeSlots proposes options between from and to.
	ComputeSlots(ctx context.Context, from, to time.Time, dur time.Duration, inPerson bool) ([]Slot, error)
	// VerifyFree re-checks one window right before proposing/booking it.
	VerifyFree(ctx context.Context, start, end time.Time) (bool, error)
	// Book creates the event (with a Meet link for non-in-person meetings).
	Book(ctx context.Context, summary, description string, start, end time.Time, withMeet bool) (eventID, htmlLink, meetLink string, err error)
	// CancelEvent deletes a previously booked event.
	CancelEvent(ctx context.Context, eventID string) error
}

// Sender is the outbound message path. SendSelf/SendTo return the WhatsApp
// message ID of the sent message (used for consent-scoping adjacency).
type Sender interface {
	SendSelf(ctx context.Context, text string) (msgID string, err error)
	SendTo(ctx context.Context, jid, text string) (msgID string, err error)
}

// GoogleCalendarService adapts calendar.Client + store prefs to
// CalendarService.
type GoogleCalendarService struct {
	DB     *store.DB
	Client *calendar.Client
}

// storeTokenAdapter implements calendar.TokenStore over store.DB.
type storeTokenAdapter struct{ db *store.DB }

func (a *storeTokenAdapter) LoadToken() (*calendar.Token, error) {
	raw := a.db.GetGoogleToken()
	if raw == "" {
		return nil, fmt.Errorf("no Google token stored")
	}
	var t calendar.Token
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (a *storeTokenAdapter) SaveToken(t *calendar.Token) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return a.db.SetGoogleToken(string(b))
}

func (a *storeTokenAdapter) ClientCreds() (string, string) {
	return a.db.GetSetting("google_client_id"), a.db.GetGoogleClientSecret()
}

// NewGoogleCalendarService builds the production calendar service.
func NewGoogleCalendarService(db *store.DB) *GoogleCalendarService {
	return &GoogleCalendarService{DB: db, Client: calendar.NewClient(&storeTokenAdapter{db: db})}
}

// TokenStore exposes the adapter for the OAuth connect flow in the server.
func NewTokenStore(db *store.DB) calendar.TokenStore { return &storeTokenAdapter{db: db} }

func (g *GoogleCalendarService) Connected() bool {
	return g.DB.GetGoogleToken() != ""
}

func (g *GoogleCalendarService) location() *time.Location {
	prefs := g.DB.GetSchedulePrefs()
	if prefs.Timezone != "" {
		if loc, err := time.LoadLocation(prefs.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

func (g *GoogleCalendarService) calendarIDs() []string {
	prefs := g.DB.GetSchedulePrefs()
	if len(prefs.Calendars) > 0 {
		return prefs.Calendars
	}
	return []string{"primary"}
}

func (g *GoogleCalendarService) calPrefs(inPerson bool) calendar.Prefs {
	p := g.DB.GetSchedulePrefs()
	workdays := map[time.Weekday]bool{}
	for _, d := range p.Workdays {
		if d >= 0 && d <= 6 {
			workdays[time.Weekday(d)] = true
		}
	}
	prefs := calendar.Prefs{
		DayStartMin: p.DayStartMin,
		DayEndMin:   p.DayEndMin,
		Workdays:    workdays,
		Location:    g.location(),
	}
	if inPerson {
		prefs.Buffer = time.Duration(p.TravelBufferMin) * time.Minute
	}
	return prefs
}

func (g *GoogleCalendarService) ComputeSlots(ctx context.Context, from, to time.Time, dur time.Duration, inPerson bool) ([]Slot, error) {
	p := g.DB.GetSchedulePrefs()
	busy, err := g.Client.BusyAcross(ctx, g.calendarIDs(), from, to, g.location(), p.IgnoreTitles)
	if err != nil {
		return nil, err
	}
	raw := calendar.ComputeSlots(busy, from, to, dur, g.calPrefs(inPerson), 3)
	var out []Slot
	for _, s := range raw {
		out = append(out, Slot{Start: s.Start, End: s.End, Origin: "computed"})
	}
	return out, nil
}

func (g *GoogleCalendarService) VerifyFree(ctx context.Context, start, end time.Time) (bool, error) {
	p := g.DB.GetSchedulePrefs()
	return g.Client.VerifySlotFree(ctx, g.calendarIDs(), start, end, g.location(), p.IgnoreTitles)
}

func (g *GoogleCalendarService) Book(ctx context.Context, summary, description string, start, end time.Time, withMeet bool) (string, string, string, error) {
	primary := "primary"
	if ids := g.calendarIDs(); len(ids) > 0 {
		primary = ids[0]
	}
	ev, err := g.Client.CreateEvent(ctx, primary, summary, description, start, end, "", withMeet)
	if err != nil {
		return "", "", "", err
	}
	return ev.ID, ev.HTMLLink, ev.MeetLink, nil
}

func (g *GoogleCalendarService) CancelEvent(ctx context.Context, eventID string) error {
	primary := "primary"
	if ids := g.calendarIDs(); len(ids) > 0 {
		primary = ids[0]
	}
	return g.Client.DeleteEvent(ctx, primary, eventID)
}
