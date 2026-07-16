package whatsapp

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/msfoundry/commit/schedule"
	"github.com/msfoundry/commit/store"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// waSender adapts the whatsmeow client to schedule.Sender, returning message
// IDs so the manager can do consent-scoping adjacency checks.
type waSender struct{ c *Client }

func (s *waSender) SendSelf(ctx context.Context, text string) (string, error) {
	target, ok := s.c.SelfChatTarget()
	if !ok {
		log.Printf("schedule: SendSelf: no self-chat target known yet")
		return "", errNotConnected
	}
	return s.send(ctx, target, text)
}

func (s *waSender) SendTo(ctx context.Context, jid, text string) (string, error) {
	parsed, err := types.ParseJID(jid)
	if err != nil {
		return "", err
	}
	return s.send(ctx, parsed, text)
}

func (s *waSender) send(ctx context.Context, jid types.JID, text string) (string, error) {
	s.c.mu.RLock()
	client := s.c.wa
	s.c.mu.RUnlock()
	if client == nil {
		log.Printf("schedule: send: c.wa is nil")
		return "", errNotConnected
	}
	resp, err := client.SendMessage(ctx, jid, &waE2E.Message{Conversation: &text})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// InitScheduler wires the @schedule manager into the client. Called from main.
func (c *Client) InitScheduler(db *store.DB) {
	creds := schedule.Creds{
		APIKey: db.GetAPIKey,
		// Interpretation and drafting run on haiku to match production cost;
		// the model is pinned rather than following the extraction model.
		Model: func() string { return store.FallbackModel },
	}
	c.scheduler = &schedule.Manager{
		DB:     db,
		Cal:    schedule.NewGoogleCalendarService(db),
		Interp: &schedule.LLMInterpreter{Creds: creds},
		Sender: &waSender{c: c},
		Creds:  creds,
		SelfJID: func() string {
			target, ok := c.SelfChatTarget()
			if !ok {
				return ""
			}
			return target.String()
		},
	}
}

// Scheduler exposes the manager (used by the server for the sessions API).
func (c *Client) Scheduler() *schedule.Manager { return c.scheduler }

// handleScheduleSelfChat routes self-chat messages into the scheduler. It
// runs after the message has been saved (so adjacency checks see it) and
// returns true when the scheduler consumed the message.
func (c *Client) handleScheduleSelfChat(evt *events.Message, text string) bool {
	if c.scheduler == nil {
		return false
	}
	consumed := false
	lower := strings.ToLower(strings.TrimSpace(text))
	if strings.HasPrefix(lower, "@schedule") {
		// Commands can be slow (calendar + LLM) — ack happens inside, so run
		// async but report consumption immediately.
		go c.scheduler.HandleSelfChat(context.Background(), text, evt.Info.ID, evt.Info.Timestamp)
		return true
	}
	consumed = c.scheduler.HandleSelfChat(context.Background(), text, evt.Info.ID, evt.Info.Timestamp)
	return consumed
}

// notifyScheduleWatcher feeds saved 1:1 messages to the session watcher.
func (c *Client) notifyScheduleWatcher(msg *store.Message) {
	if c.scheduler == nil || msg.IsGroup {
		return
	}
	// Skip the self-chat itself; that path is handled by handleBotCommand.
	own := c.GetOwnJID()
	if !own.IsEmpty() && msg.ChatJID == types.NewJID(own.User, types.DefaultUserServer).String() {
		return
	}
	go c.scheduler.OnContactMessage(context.Background(), msg.ChatJID, msg.IsFromMe, msg.Content, msg.Timestamp)
}

// scheduleExpiryLoop closes sessions after 48h of silence.
func (c *Client) scheduleExpiryLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.scheduler != nil {
				c.scheduler.RunExpirySweep(time.Now())
			}
		}
	}
}

var errNotConnected = notConnectedError{}

type notConnectedError struct{}

func (notConnectedError) Error() string { return "WhatsApp not connected" }
