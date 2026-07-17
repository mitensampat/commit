package whatsapp

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/msfoundry/commit/schedule"
	"github.com/msfoundry/commit/store"

	_ "modernc.org/sqlite"
)

type Extractor interface {
	StartProcessingLoop(ctx context.Context)
	StartResolutionLoop(ctx context.Context)
}

type FindHandler interface {
	FindAnswer(ctx context.Context, query string) (string, error)
}

type Client struct {
	db          *store.DB
	dataDir     string
	extractor   Extractor
	findHandler FindHandler

	mu           sync.RWMutex
	wa           *whatsmeow.Client
	container    *sqlstore.Container
	qrChan       chan string
	connected    bool
	appCtx       context.Context
	loopsStarted bool

	pendingMu      sync.Mutex
	pendingChoices []string // person names awaiting disambiguation

	selfChatJID types.JID // observed self-chat thread (LID-safe), see rememberSelfChat

	scheduler *schedule.Manager // @schedule sessions (nil until InitScheduler)
}

func New(db *store.DB, dataDir string, extractor Extractor, appCtx context.Context) *Client {
	return &Client{
		db:        db,
		dataDir:   dataDir,
		extractor: extractor,
		qrChan:    make(chan string, 5),
		appCtx:    appCtx,
	}
}

func (c *Client) SetFindHandler(fh FindHandler) {
	c.findHandler = fh
}

func (c *Client) HasSession() bool {
	container, err := c.getContainer()
	if err != nil {
		return false
	}
	devices, err := container.GetAllDevices(context.Background())
	if err != nil {
		return false
	}
	return len(devices) > 0
}

func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

func (c *Client) QRChannel() <-chan string {
	return c.qrChan
}

func (c *Client) Connect(ctx context.Context) error {
	container, err := c.getContainer()
	if err != nil {
		return fmt.Errorf("get container: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("get device: %w", err)
	}

	client := whatsmeow.NewClient(deviceStore, waLog.Noop)
	c.mu.Lock()
	c.wa = client
	c.mu.Unlock()

	client.AddEventHandler(c.handleEvent)

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(ctx)
		if err := client.Connect(); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				select {
				case c.qrChan <- evt.Code:
				default:
				}
			}
		}
	} else {
		if err := client.Connect(); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
	}

	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()

	c.startLoops(ctx)

	<-ctx.Done()
	client.Disconnect()
	return nil
}

func (c *Client) Login(ctx context.Context) (<-chan string, error) {
	// Never clobber a healthy session: a stray visit to the QR screen used
	// to replace the paired client with an unpaired one, leaving receiving
	// alive on the old client and every send broken on the new one.
	if c.IsConnected() && c.HasSession() {
		empty := make(chan string)
		close(empty)
		return empty, nil
	}

	container, err := c.getContainer()
	if err != nil {
		return nil, fmt.Errorf("get container: %w", err)
	}

	deviceStore := container.NewDevice()
	client := whatsmeow.NewClient(deviceStore, waLog.Noop)
	client.AddEventHandler(c.handleEvent)

	qrCodes := make(chan string, 5)

	go func() {
		defer close(qrCodes)
		qrChan, _ := client.GetQRChannel(ctx)
		if err := client.Connect(); err != nil {
			log.Printf("connect error: %v", err)
			return
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				select {
				case qrCodes <- evt.Code:
				default:
				}
			} else if evt.Event == "success" {
				// Pairing succeeded — only now does the new client take over.
				c.mu.Lock()
				old := c.wa
				c.wa = client
				c.connected = true
				c.mu.Unlock()
				if old != nil && old != client {
					old.Disconnect()
				}
				c.startLoops(c.appCtx)
				return
			}
		}
		// QR expired or flow abandoned without pairing — tear the
		// provisional client down; the previous session (if any) is intact.
		client.Disconnect()
	}()

	return qrCodes, nil
}

func (c *Client) handleEvent(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.Message:
		if !c.handleBotCommand(context.Background(), evt) {
			c.handleMessage(evt)
		}
	case *events.CallOffer:
		c.handleCallEvent(evt.BasicCallMeta, false)
	case *events.CallOfferNotice:
		c.handleCallEvent(evt.BasicCallMeta, false)
	case *events.HistorySync:
		go c.handleHistorySync(evt)
	case *events.Connected:
		log.Println("WhatsApp connected")
		c.mu.Lock()
		c.connected = true
		c.mu.Unlock()
	case *events.Disconnected:
		log.Println("WhatsApp disconnected")
		c.mu.Lock()
		c.connected = false
		client := c.wa
		c.mu.Unlock()
		if client != nil {
			go c.reconnect(client)
		}
	}
}

func (c *Client) reconnect(client *whatsmeow.Client) {
	backoff := 5 * time.Second
	maxBackoff := 5 * time.Minute
	for {
		select {
		case <-c.appCtx.Done():
			return
		case <-time.After(backoff):
		}

		c.mu.RLock()
		current := c.wa
		c.mu.RUnlock()
		if current != client {
			return
		}

		log.Printf("attempting WhatsApp reconnect...")
		if err := client.Connect(); err != nil {
			log.Printf("reconnect failed: %v", err)
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		log.Println("WhatsApp reconnected")
		return
	}
}

func (c *Client) handleMessage(evt *events.Message) {
	text := extractText(evt.Message)

	chatJID := evt.Info.Chat.String()
	if evt.Info.Chat.Server == types.BroadcastServer {
		return
	}
	if c.db.IsChatMuted(chatJID) {
		return
	}

	if text == "" {
		// Nothing readable. If it's media and a scheduling session is waiting
		// on this person, the watcher needs to know something arrived — going
		// silent on a voice note is what makes @schedule look broken. The
		// message itself is NOT saved: we have no text, and the messages table
		// must stay honest.
		if kind, ok := mediaKindOf(evt.Message); ok {
			c.notifyScheduleWatcherMedia(chatJID, evt.Info.Chat.Server == types.GroupServer,
				evt.Info.IsFromMe, kind, evt.Info.Timestamp)
		}
		return
	}
	senderJID := evt.Info.Sender.String()
	isGroup := evt.Info.Chat.Server == types.GroupServer
	isFromMe := evt.Info.IsFromMe

	senderName := ""
	chatName := ""
	if evt.Info.PushName != "" {
		senderName = evt.Info.PushName
	}
	if isGroup {
		chatName = c.getChatName(evt.Info.Chat)
	} else if isFromMe {
		chatName = c.db.GetChatDisplayName(chatJID)
	} else {
		chatName = senderName
	}

	msg := &store.Message{
		ID:         evt.Info.ID,
		ChatJID:    chatJID,
		SenderJID:  senderJID,
		SenderName: senderName,
		ChatName:   chatName,
		Content:    text,
		Timestamp:  evt.Info.Timestamp,
		IsFromMe:   isFromMe,
		IsGroup:    isGroup,
	}

	if err := c.db.SaveMessage(msg); err != nil {
		log.Printf("save message error: %v", err)
	} else {
		c.notifyScheduleWatcher(msg)
	}
}

func (c *Client) handleCallEvent(meta types.BasicCallMeta, isFromMe bool) {
	callerJID := meta.From
	chatJID := types.NewJID(callerJID.User, types.DefaultUserServer).String()

	if meta.GroupJID.Server != "" {
		chatJID = meta.GroupJID.String()
	}

	if c.db.IsChatMuted(chatJID) {
		return
	}

	ownJID := c.GetOwnJID()
	if !ownJID.IsEmpty() && callerJID.User == ownJID.User {
		isFromMe = true
	}

	senderName := ""
	if !isFromMe {
		senderName = c.db.GetChatDisplayName(chatJID)
	}

	content := "[Voice call]"
	if isFromMe {
		content = "[Voice call placed]"
	} else {
		content = fmt.Sprintf("[Voice call from %s]", senderName)
	}

	msg := &store.Message{
		ID:         "call_" + meta.CallID,
		ChatJID:    chatJID,
		SenderJID:  callerJID.String(),
		SenderName: senderName,
		ChatName:   c.db.GetChatDisplayName(chatJID),
		Content:    content,
		Timestamp:  meta.Timestamp,
		IsFromMe:   isFromMe,
		IsGroup:    meta.GroupJID.Server != "",
	}

	if err := c.db.SaveMessage(msg); err != nil {
		log.Printf("save call event error: %v", err)
	} else {
		log.Printf("captured call event: %s in %s", content, chatJID)
	}
}

func (c *Client) getChatName(jid types.JID) string {
	c.mu.RLock()
	client := c.wa
	c.mu.RUnlock()

	if client == nil {
		return jid.String()
	}

	info, err := client.GetGroupInfo(context.Background(), jid)
	if err != nil {
		return jid.String()
	}
	return info.Name
}

func (c *Client) SendMessage(ctx context.Context, jid types.JID, text string) error {
	c.mu.RLock()
	client := c.wa
	c.mu.RUnlock()

	if client == nil {
		return fmt.Errorf("not connected")
	}

	_, err := client.SendMessage(ctx, jid, &waE2E.Message{
		Conversation: &text,
	})
	return err
}

func (c *Client) Notify(text string) {
	selfJID, ok := c.SelfChatTarget()
	if !ok {
		return
	}
	if err := c.SendMessage(c.appCtx, selfJID, text); err != nil {
		log.Printf("notify error: %v", err)
	}
}

func (c *Client) SendWelcomeMessages(ctx context.Context, onStage func(stage string)) {
	if selfJID, ok := c.SelfChatTarget(); ok {
		_ = c.SendMessage(ctx, selfJID, "✅ Connected to Commit. Your dashboard is ready.")
	}

	stages := []string{"connected", "scanning", "ready"}
	for _, s := range stages {
		if onStage != nil {
			onStage(s)
		}
	}
}

func (c *Client) isSelfChat(evt *events.Message) bool {
	chat := evt.Info.Chat
	sender := evt.Info.Sender

	// Old format: chat is your phone number @s.whatsapp.net
	ownJID := c.GetOwnJID()
	if !ownJID.IsEmpty() && chat.User == ownJID.User {
		c.rememberSelfChat(chat)
		return true
	}

	// New LID format: self-chat is your LID @lid, sender LID matches chat
	if chat.Server == types.HiddenUserServer && sender.User == chat.User {
		c.rememberSelfChat(chat)
		return true
	}

	return false
}

// rememberSelfChat caches (and persists) the JID of the user's self-chat as
// observed from real traffic. On LID-paired sessions Store.ID can be empty or
// phone-format while the actual self-chat thread is @lid — the observed JID
// is the only reliable target for self-sends (digest, reminders, @schedule).
func (c *Client) rememberSelfChat(jid types.JID) {
	c.mu.Lock()
	changed := c.selfChatJID.String() != jid.String()
	c.selfChatJID = jid
	c.mu.Unlock()
	if changed {
		if err := c.db.SetSetting("self_chat_jid", jid.String()); err != nil {
			log.Printf("persist self chat jid: %v", err)
		} else {
			log.Printf("self-chat JID learned: %s", jid.String())
		}
	}
}

// SelfChatTarget returns the best-known JID for messaging yourself:
// observed self-chat (memory, then persisted) with Store.ID as fallback.
func (c *Client) SelfChatTarget() (types.JID, bool) {
	c.mu.RLock()
	cached := c.selfChatJID
	c.mu.RUnlock()
	if !cached.IsEmpty() {
		return cached, true
	}
	if saved := c.db.GetSetting("self_chat_jid"); saved != "" {
		if jid, err := types.ParseJID(saved); err == nil && !jid.IsEmpty() {
			c.mu.Lock()
			c.selfChatJID = jid
			c.mu.Unlock()
			return jid, true
		}
	}
	own := c.GetOwnJID()
	if !own.IsEmpty() {
		return types.NewJID(own.User, types.DefaultUserServer), true
	}
	return types.JID{}, false
}

func (c *Client) GetOwnJID() types.JID {
	c.mu.RLock()
	client := c.wa
	c.mu.RUnlock()

	if client == nil || client.Store.ID == nil {
		return types.JID{}
	}
	return *client.Store.ID
}

func (c *Client) startLoops(ctx context.Context) {
	c.mu.Lock()
	if c.loopsStarted {
		c.mu.Unlock()
		return
	}
	c.loopsStarted = true
	c.mu.Unlock()
	go c.extractor.StartProcessingLoop(ctx)
	go c.extractor.StartResolutionLoop(ctx)
	go c.reminderLoop(ctx)
	go c.morningDigestLoop(ctx)
	go c.contactSyncLoop(ctx)
	go c.scheduleExpiryLoop(ctx)
}

func (c *Client) getContainer() (*sqlstore.Container, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.container != nil {
		return c.container, nil
	}
	dbPath := filepath.Join(c.dataDir, "whatsmeow.db")
	container, err := sqlstore.New(context.Background(), "sqlite", fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", dbPath), waLog.Noop)
	if err != nil {
		return nil, err
	}
	c.container = container
	return container, nil
}

func extractText(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if msg.Conversation != nil {
		return *msg.Conversation
	}
	if msg.ExtendedTextMessage != nil && msg.ExtendedTextMessage.Text != nil {
		return *msg.ExtendedTextMessage.Text
	}
	return ""
}

func (c *Client) reminderLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			due, err := c.db.GetDueReminders()
			if err != nil {
				log.Printf("reminder check error: %v", err)
				continue
			}
			for _, cm := range due {
				direction := "You promised"
				if cm.Direction == "they_owe" {
					direction = cm.PersonName + " promised"
				}
				text := fmt.Sprintf("⏰ Reminder: %s — %s\n\n%s", cm.Title, direction, cm.Context)

				if selfJID, ok := c.SelfChatTarget(); ok {
					if err := c.SendMessage(ctx, selfJID, text); err != nil {
						log.Printf("send reminder error: %v", err)
						continue
					}
				}
				c.db.ClearReminder(cm.ID)
			}
		}
	}
}

// morningDigestLoop sends one self-chat message per day, after 8am local,
// with the top 3 items from the same ranking that powers the Today view.
// Tenets: one moment per day, only your own commitments, only if something
// actually has consequences — a quiet day sends nothing.
func (c *Client) morningDigestLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			if now.Hour() < 8 {
				continue
			}
			today := now.Format("2006-01-02")
			if c.db.GetSetting("last_morning_digest") == today {
				continue
			}
			cands, err := c.db.GetTodayCandidates()
			if err != nil {
				log.Printf("morning digest candidates error: %v", err)
				continue
			}
			items := store.RankToday(cands, now, 3)
			if len(items) == 0 {
				// Nothing worth a nudge today — stay quiet, but don't retry all day.
				c.db.SetSetting("last_morning_digest", today)
				continue
			}
			var b strings.Builder
			b.WriteString("☀️ Worth acting on today:\n")
			for i, it := range items {
				b.WriteString(fmt.Sprintf("\n%d. %s", i+1, it.Title))
				if it.Reason != "" {
					b.WriteString(" — " + it.Reason)
				}
			}
			selfJID, ok := c.SelfChatTarget()
			if !ok {
				continue
			}
			if err := c.SendMessage(ctx, selfJID, b.String()); err != nil {
				log.Printf("morning digest send error: %v", err)
				continue
			}
			c.db.SetSetting("last_morning_digest", today)
		}
	}
}

// Logout disconnects and removes the session
func (c *Client) Logout() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.wa != nil {
		c.wa.Disconnect()
		c.wa = nil
	}
	c.connected = false

	dbPath := filepath.Join(c.dataDir, "whatsmeow.db")
	os.Remove(dbPath)
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	return nil
}

func (c *Client) handleHistorySync(evt *events.HistorySync) {
	data := evt.Data
	if data == nil {
		return
	}
	conversations := data.GetConversations()
	if len(conversations) == 0 {
		return
	}

	count := 0
	for _, conv := range conversations {
		chatJID := conv.GetID()
		if chatJID == "" || chatJID == "status@broadcast" {
			continue
		}
		if c.db.IsChatMuted(chatJID) {
			continue
		}
		isGroup := strings.HasSuffix(chatJID, "@g.us")
		chatName := conv.GetDisplayName()
		if chatName == "" {
			chatName = conv.GetName()
		}

		for _, histMsg := range conv.GetMessages() {
			webMsg := histMsg.GetMessage()
			if webMsg == nil || webMsg.GetMessage() == nil {
				continue
			}
			key := webMsg.GetKey()
			if key == nil {
				continue
			}

			text := extractText(webMsg.GetMessage())
			if text == "" {
				continue
			}

			ts := webMsg.GetMessageTimestamp()
			if ts == 0 {
				continue
			}
			msgTime := time.Unix(int64(ts), 0)
			if msgTime.Before(time.Now().AddDate(0, 0, -7)) {
				continue
			}

			senderName := webMsg.GetPushName()
			isFromMe := key.GetFromMe()
			senderJID := key.GetParticipant()
			if senderJID == "" && !isGroup {
				if isFromMe {
					ownJID := c.GetOwnJID()
					if !ownJID.IsEmpty() {
						senderJID = ownJID.String()
					}
				} else {
					senderJID = chatJID
				}
			}

			if chatName == "" && !isGroup && !isFromMe {
				chatName = senderName
			}

			msg := &store.Message{
				ID:         key.GetID(),
				ChatJID:    chatJID,
				SenderJID:  senderJID,
				SenderName: senderName,
				ChatName:   chatName,
				Content:    text,
				Timestamp:  msgTime,
				IsFromMe:   isFromMe,
				IsGroup:    isGroup,
			}
			if err := c.db.SaveMessage(msg); err == nil {
				count++
			}
		}
	}
	if count > 0 {
		log.Printf("history sync: saved %d messages from %d conversations", count, len(conversations))
	}
}
