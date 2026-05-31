package extraction

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/msfoundry/commit/store"
)

type Notifier interface {
	Notify(text string)
}

type Extractor struct {
	db       *store.DB
	notifier Notifier
	mu       sync.Mutex
	stopCh   chan struct{}

	debugMu       sync.RWMutex
	loopRunning   bool
	lastRunAt     time.Time
	lastError     string
	lastErrorAt   time.Time
	batchesRun    int
	msgsProcessed int
	lastNotifyAt  time.Time
}

type DebugStatus struct {
	LoopRunning   bool   `json:"loop_running"`
	LastRunAt     string `json:"last_run_at"`
	LastError     string `json:"last_error"`
	LastErrorAt   string `json:"last_error_at"`
	BatchesRun    int    `json:"batches_run"`
	MsgsProcessed int    `json:"msgs_processed"`
}

func New(db *store.DB, notifier Notifier) *Extractor {
	return &Extractor{db: db, notifier: notifier}
}

func (e *Extractor) SetNotifier(n Notifier) {
	e.notifier = n
}

type extractedCommitment struct {
	Title       string `json:"title"`
	Context     string `json:"context"`
	Direction   string `json:"direction"` // "you_owe" or "they_owe"
	SourceQuote string `json:"source_quote"`
	DueHint     string `json:"due_hint"`
	PersonName  string `json:"person_name"`
}

type extractionResult struct {
	Commitments []extractedCommitment `json:"commitments"`
	Resolved    []string              `json:"resolved"`
}

const (
	baseInterval = 10 * time.Second
	maxInterval  = 5 * time.Minute
)

func (e *Extractor) GetDebugStatus() DebugStatus {
	e.debugMu.RLock()
	defer e.debugMu.RUnlock()
	s := DebugStatus{
		LoopRunning:   e.loopRunning,
		BatchesRun:    e.batchesRun,
		MsgsProcessed: e.msgsProcessed,
	}
	if !e.lastRunAt.IsZero() {
		s.LastRunAt = e.lastRunAt.Format(time.RFC3339)
	}
	s.LastError = e.lastError
	if !e.lastErrorAt.IsZero() {
		s.LastErrorAt = e.lastErrorAt.Format(time.RFC3339)
	}
	return s
}

func (e *Extractor) StartProcessingLoop(ctx context.Context) {
	log.Println("extraction loop started")
	e.debugMu.Lock()
	e.loopRunning = true
	e.debugMu.Unlock()

	defer func() {
		e.debugMu.Lock()
		e.loopRunning = false
		e.debugMu.Unlock()
	}()

	interval := baseInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		err := e.ProcessBatch(ctx)
		if err != nil {
			log.Printf("extraction error: %v", err)
			e.debugMu.Lock()
			e.lastError = err.Error()
			e.lastErrorAt = time.Now()
			e.debugMu.Unlock()
			interval = min(interval*2, maxInterval)
		} else {
			interval = baseInterval
		}
	}
}

func (e *Extractor) ProcessBatch(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	apiKey := e.db.GetAPIKey()
	if apiKey == "" {
		return nil
	}

	msgs, err := e.db.GetUnprocessedMessages(50)
	if err != nil {
		return fmt.Errorf("get unprocessed: %w", err)
	}
	if len(msgs) == 0 {
		e.debugMu.Lock()
		e.lastRunAt = time.Now()
		e.batchesRun++
		e.debugMu.Unlock()
		return nil
	}
	log.Printf("processing %d unprocessed messages", len(msgs))

	grouped := groupMessagesByChat(msgs)
	mutedChats, _ := e.db.GetMutedChatJIDs()
	var extractionErr error

	for chatJID, chatMsgs := range grouped {
		if mutedChats[chatJID] {
			ids := make([]string, len(chatMsgs))
			for i, m := range chatMsgs {
				ids[i] = m.ID
			}
			e.db.MarkMessagesProcessed(ids)
			continue
		}
		openCommitments, _ := e.db.GetOpenCommitmentsForChat(chatJID)

		result, err := e.extractFromChat(ctx, apiKey, chatMsgs, openCommitments)
		if err != nil {
			log.Printf("extraction failed for %s: %v", chatJID, err)
			extractionErr = err
			if e.notifier != nil && strings.Contains(err.Error(), "429") {
				e.debugMu.RLock()
				lastNotify := e.lastNotifyAt
				e.debugMu.RUnlock()
				if time.Since(lastNotify) > time.Hour {
					e.notifier.Notify("⚠️ Commit: API rate limit hit. Extraction paused, will retry shortly.")
					e.debugMu.Lock()
					e.lastNotifyAt = time.Now()
					e.debugMu.Unlock()
				}
			}
			continue
		}

		for _, ec := range result.Commitments {
			c := &store.Commitment{
				ChatJID:     chatJID,
				ChatName:    chatMsgs[0].ChatName,
				PersonName:  ec.PersonName,
				Title:       ec.Title,
				Context:     ec.Context,
				Direction:   ec.Direction,
				SourceQuote: ec.SourceQuote,
				DueHint:     ec.DueHint,
				Status:      "open",
				IsGroup:     chatMsgs[0].IsGroup,
			}
			if ec.SourceQuote != "" {
				for _, m := range chatMsgs {
					if strings.Contains(m.Content, ec.SourceQuote) || strings.Contains(ec.SourceQuote, m.Content) {
						c.MessageID = m.ID
						c.SourceTime = m.Timestamp.Format("Jan 2, 3:04 PM")
						break
					}
				}
			}
			if c.SourceTime == "" && len(chatMsgs) > 0 {
				c.SourceTime = chatMsgs[len(chatMsgs)-1].Timestamp.Format("Jan 2, 3:04 PM")
			}
			if err := e.db.SaveCommitment(c); err != nil {
				log.Printf("save commitment error: %v", err)
			}
		}

		for _, resolvedID := range result.Resolved {
			if err := e.db.AutoResolveCommitment(resolvedID); err != nil {
				log.Printf("auto-resolve error for %s: %v", resolvedID, err)
			} else {
				log.Printf("auto-resolved commitment %s", resolvedID)
			}
		}

		ids := make([]string, len(chatMsgs))
		for i, m := range chatMsgs {
			ids[i] = m.ID
		}
		if err := e.db.MarkMessagesProcessed(ids); err != nil {
			log.Printf("mark processed error: %v", err)
		}
	}

	e.debugMu.Lock()
	e.lastRunAt = time.Now()
	e.batchesRun++
	e.msgsProcessed += len(msgs)
	e.debugMu.Unlock()

	return extractionErr
}

func (e *Extractor) extractFromChat(ctx context.Context, apiKey string, msgs []*store.Message, openCommitments []*store.Commitment) (*extractionResult, error) {
	prompt := buildExtractionPrompt(msgs, openCommitments)
	response, err := callClaude(ctx, apiKey, prompt)
	if err != nil {
		return nil, err
	}

	var result extractionResult
	jsonStr := extractJSON(response)
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &result, nil
}

func buildExtractionPrompt(msgs []*store.Message, openCommitments []*store.Commitment) string {
	var sb strings.Builder
	sb.WriteString(`Analyze these WhatsApp messages. Do two things:

1. EXTRACT NEW COMMITMENTS — promises, obligations, or things someone said they would do.
For each new commitment, return:
- title: short description of the commitment
- context: one sentence explaining the situation
- direction: "you_owe" if the user (messages marked [ME]) made the promise, "they_owe" if someone else did
- source_quote: the exact message text that contains the commitment
- due_hint: any mentioned deadline or timeframe, converted to a concrete date/time if possible (e.g. "tomorrow" → "May 30", "by EOD" → "today evening"). Empty string if none
- person_name: the name of the other person involved

IMPORTANT — only extract REAL commitments. A commitment is someone explicitly stating they WILL do something, or agreeing to a request. Do NOT extract:
- Questions ("can you...?", "would you mind...?") — these are requests, not commitments, unless answered with agreement
- Offers or suggestions ("I could...", "maybe we should...") — only extract if they clearly commit to action
- Greetings, small talk, reactions, or emotional messages
- Status updates or announcements that don't involve a promise to act
- Rhetorical statements or vague intentions ("we should catch up sometime")
- Messages in any language follow the same rules — translate mentally but apply the same strict standard

When in doubt, do NOT extract. False positives are worse than missed commitments.

2. AUTO-RESOLVE — this is critical. Carefully check if ANY of the existing open commitments below have been fulfilled, completed, or made irrelevant by the new messages. Be aggressive about detecting resolution. Mark a commitment as resolved if:
- The promised action was done (sent a doc, made a call, shared info, etc.)
- The conversation shows the matter was handled or discussed ("done", "sorted", "taken care of")
- Someone explicitly confirms completion ("got it", "received", "thanks for doing that")
- The commitment became moot (plans changed, no longer needed, topic moved on with resolution)
- A file, link, photo, or voice note was shared that fulfills a promise to send something
- The person followed through on what they said they'd do, even if they didn't explicitly say "done"

Return JSON in this format:
{"commitments": [...], "resolved": ["id1", "id2"]}

"commitments" = new commitments found. "resolved" = IDs of existing commitments now fulfilled.
If nothing found, return {"commitments": [], "resolved": []}.
`)

	if len(openCommitments) > 0 {
		sb.WriteString("\nExisting open commitments for this chat:\n")
		for _, c := range openCommitments {
			dir := "You owe"
			if c.Direction == "they_owe" {
				dir = "They owe"
			}
			sb.WriteString(fmt.Sprintf("- [ID: %s] %s: %s (%s)\n", c.ID, dir, c.Title, c.PersonName))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("Messages:\n")
	for _, m := range msgs {
		prefix := m.SenderName
		if m.IsFromMe {
			prefix = "[ME]"
		}
		sb.WriteString(fmt.Sprintf("[%s] %s: %s\n",
			m.Timestamp.Format("Jan 2 3:04PM"),
			prefix,
			m.Content,
		))
	}

	return sb.String()
}

func groupMessagesByChat(msgs []*store.Message) map[string][]*store.Message {
	grouped := make(map[string][]*store.Message)
	for _, m := range msgs {
		grouped[m.ChatJID] = append(grouped[m.ChatJID], m)
	}
	return grouped
}

func extractJSON(s string) string {
	start := strings.Index(s, "{")
	if start == -1 {
		return "{\"commitments\":[]}"
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}
