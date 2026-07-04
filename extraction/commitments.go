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
	Title        string `json:"title"`
	Context      string `json:"context"`
	Direction    string `json:"direction"` // "you_owe" or "they_owe"
	SourceQuote  string `json:"source_quote"`
	DueHint      string `json:"due_hint"`
	PersonName   string `json:"person_name"`
	Significance string `json:"significance"` // "high", "medium", "low"
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

	msgs, err := e.db.GetUnprocessedMessages(100)
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
			sig := ec.Significance
			if sig != "high" && sig != "medium" && sig != "low" {
				sig = "medium"
			}
			c := &store.Commitment{
				ChatJID:      chatJID,
				ChatName:     chatMsgs[0].ChatName,
				PersonName:   ec.PersonName,
				Title:        ec.Title,
				Context:      ec.Context,
				Direction:    ec.Direction,
				SourceQuote:  ec.SourceQuote,
				DueHint:      ec.DueHint,
				Status:       "open",
				IsGroup:      chatMsgs[0].IsGroup,
				Significance: sig,
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
	myStyle := e.db.GetMyStyle()
	prompt := buildExtractionPrompt(msgs, openCommitments, myStyle)
	model := e.db.GetModel()
	response, err := callClaude(ctx, apiKey, model, prompt)
	if err != nil {
		// Auto-fallback: if model not found, try fallback and save it
		if _, ok := err.(*ModelNotFoundError); ok && model != store.FallbackModel {
			log.Printf("model %s not available, falling back to %s", model, store.FallbackModel)
			e.db.SetModel(store.FallbackModel)
			response, err = callClaude(ctx, apiKey, store.FallbackModel, prompt)
		}
		if err != nil {
			return nil, err
		}
	}

	var result extractionResult
	jsonStr := extractJSON(response)
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &result, nil
}

func buildExtractionPrompt(msgs []*store.Message, openCommitments []*store.Commitment, myStyle string) string {
	var sb strings.Builder
	sb.WriteString(`Analyze these WhatsApp messages. Do two things:

1. EXTRACT NEW COMMITMENTS — promises, obligations, or things someone said they would do.

The user is a CEO. Only surface commitments that would matter to a sharp executive assistant — things that, if dropped, would damage a relationship, miss a deadline, lose money, or block someone important. Think: "Would this be embarrassing if forgotten?"

For each new commitment, return:
- title: short, specific description (not "Call X" — say what the call is about if known)
- context: one sentence explaining the situation and stakes
- direction: "you_owe" if the user (messages marked [ME]) made the promise, "they_owe" if someone else did
- source_quote: the exact message text that contains the commitment
- due_hint: any mentioned deadline or timeframe, converted to a concrete date/time if possible (e.g. "tomorrow" → "May 30", "by EOD" → "today evening"). Empty string if none
- person_name: the name of the other person involved (must be a real identifiable name, not "Unknown")
- significance: "high", "medium", or "low"

SIGNIFICANCE LEVELS — be ruthlessly honest:
- HIGH: Deliverables with named recipients and deadlines. Financial obligations. Legal/regulatory actions. Board-level decisions. Commitments to investors, partners, or senior stakeholders. Anything where dropping the ball has real consequences.
- MEDIUM: Professional follow-ups with clear action items. Sharing specific documents or information. Meeting arrangements with business purpose. Introductions promised to specific people.
- LOW: Everything else that technically qualifies as a commitment but is low-stakes. Social plans, vague "let's catch up" promises, micro-actions like "I'll ping you", routine operational minutiae.

DO NOT EXTRACT — these are not commitments at all:
- Social pleasantries: "I'll come say hi", "let's catch up sometime", "will come soon", "see you there"
- Conversational filler: "let me check", "I'll get back to you", "will do", "noted", "sure"
- Questions or requests without agreement: "can you...?", "would you mind...?"
- Offers or suggestions without commitment: "I could...", "maybe we should..."
- Greetings, reactions, emotional messages, thank-yous
- Status updates or announcements without a promise to act
- Vague intentions with no specific action: "we should think about this"
- Promises where the person is unknown/unidentifiable
- Ephemeral micro-coordination: "I'll call you in 2 mins", "coming now", "on my way"
- Messages in any language follow the same rules

The bar: if a commitment wouldn't survive a weekly priorities review, don't extract it. When in doubt, do NOT extract. An empty list is better than a noisy one.

2. AUTO-RESOLVE — this is critical. Check if ANY existing open commitments below have been fulfilled or made irrelevant. Be aggressive about detecting resolution:
- The promised action was done (sent a doc, made a call, shared info, etc.)
- The matter was handled or discussed ("done", "sorted", "taken care of")
- Someone confirms completion ("got it", "received", "thanks for doing that")
- The commitment became moot (plans changed, no longer needed)
- A file, link, photo, or voice note was shared that fulfills a promise
- The person followed through, even without saying "done"

Return JSON: {"commitments": [...], "resolved": ["id1", "id2"]}
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

	if myStyle != "" {
		sb.WriteString("\nUser's style context (use this to understand their communication patterns):\n")
		sb.WriteString(myStyle)
		sb.WriteString("\n\n")
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

func (e *Extractor) StartResolutionLoop(ctx context.Context) {
	log.Println("resolution sweep loop started")

	// Run staleness check immediately on startup, then periodically
	e.RunStalenessCheck()

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Minute):
		}
		e.RunStalenessCheck()
		if err := e.RunResolutionSweep(ctx); err != nil {
			log.Printf("resolution sweep error: %v", err)
		}
	}
}

func (e *Extractor) RunStalenessCheck() {
	// Rule 1: LOW significance >14 days + items >30 days with no chat activity
	candidates, err := e.db.GetStaleAutoCloseCandidates()
	if err != nil {
		log.Printf("staleness check error: %v", err)
		return
	}

	closed := 0
	for _, c := range candidates {
		if err := e.db.AutoResolveCommitment(c.ID); err != nil {
			log.Printf("staleness auto-close error for %s: %v", c.ID, err)
		} else {
			closed++
		}
	}

	// Rule 2: expired ephemeral deadlines (>3 days old with short-term due hints)
	deadlined, err := e.db.GetExpiredDeadlineCommitments()
	if err != nil {
		log.Printf("deadline check error: %v", err)
	} else {
		for _, c := range deadlined {
			if isEphemeralDeadline(c.DueHint) {
				if err := e.db.AutoResolveCommitment(c.ID); err != nil {
					log.Printf("deadline auto-close error for %s: %v", c.ID, err)
				} else {
					closed++
				}
			}
		}
	}

	if closed > 0 {
		log.Printf("staleness check: auto-closed %d stale commitments", closed)
	}
}

func isEphemeralDeadline(hint string) bool {
	h := strings.ToLower(hint)

	// Long-term markers — never treat as ephemeral
	longTerm := []string{"90 day", "60 day", "30 day", "end of year", "eoy",
		"end of july", "end of august", "end of september", "end of october",
		"end of november", "end of december", "q3", "q4",
		"july", "august", "september", "october", "november", "december"}
	for _, lt := range longTerm {
		if strings.Contains(h, lt) {
			return false
		}
	}

	ephemeral := []string{
		"today", "tonight", "this evening", "this morning", "this afternoon",
		"shortly", "immediately", "asap", "right away",
		"tomorrow", "by eod", "end of day",
		"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday",
		"next week", "this week",
	}
	for _, e := range ephemeral {
		if strings.Contains(h, e) {
			return true
		}
	}
	// Specific time patterns: "5:15 PM", "3pm"
	if strings.Contains(h, "pm") || strings.Contains(h, "am") {
		return true
	}
	// Past month dates: "Jun 5", "June 12" — but NOT July onwards
	pastMonths := []string{"jan ", "feb ", "mar ", "apr ", "may ", "jun ",
		"january", "february", "march", "april", "june"}
	for _, m := range pastMonths {
		if strings.Contains(h, m) {
			return true
		}
	}
	return false
}

func (e *Extractor) RunResolutionSweep(ctx context.Context) error {
	apiKey := e.db.GetAPIKey()
	if apiKey == "" {
		return nil
	}

	since := time.Now().Add(-48 * time.Hour)
	chatJIDs, err := e.db.GetChatsWithRecentOutbound(since)
	if err != nil {
		return fmt.Errorf("get chats: %w", err)
	}

	log.Printf("resolution sweep: checking %d chats with recent outbound messages", len(chatJIDs))
	resolved := 0

	for _, chatJID := range chatJIDs {
		commitments, err := e.db.GetOpenCommitmentsForChat(chatJID)
		if err != nil || len(commitments) == 0 {
			continue
		}

		msgs, err := e.db.GetRecentMessagesForChat(chatJID, since)
		if err != nil || len(msgs) == 0 {
			continue
		}

		prompt := buildResolutionPrompt(msgs, commitments)
		response, err := callClaude(ctx, apiKey, store.FallbackModel, prompt)
		if err != nil {
			if strings.Contains(err.Error(), "429") {
				return err
			}
			log.Printf("resolution sweep failed for %s: %v", chatJID, err)
			continue
		}

		var result struct {
			Resolved []string `json:"resolved"`
		}
		jsonStr := extractJSON(response)
		if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
			continue
		}

		for _, id := range result.Resolved {
			if err := e.db.AutoResolveCommitment(id); err != nil {
				log.Printf("resolution sweep auto-resolve error for %s: %v", id, err)
			} else {
				resolved++
				log.Printf("resolution sweep: auto-resolved %s", id)
			}
		}
	}

	if resolved > 0 {
		log.Printf("resolution sweep: resolved %d commitments", resolved)
	}
	return nil
}

func buildResolutionPrompt(msgs []*store.Message, commitments []*store.Commitment) string {
	var sb strings.Builder
	sb.WriteString(`Review these recent WhatsApp messages and determine which of the open commitments below have been RESOLVED.

A commitment is resolved when:
- The user said "done", "sorted", "sent", "handled", "taken care of", "completed", or similar
- A file, link, document, or information was shared that fulfills the promise
- Someone confirms completion ("got it", "received", "thanks for sending")
- A call was made that fulfills a "call X" or "speak with X" commitment (look for "[Voice call]" or "[Video call]" messages)
- The matter was discussed and concluded
- Plans changed, making the commitment moot
- The promised action clearly happened, even without explicit confirmation

Be AGGRESSIVE about detecting resolution. If the conversation shows the action was likely done, resolve it. When in doubt, resolve — it's better to clean up completed items than to leave stale commitments.

Return JSON: {"resolved": ["id1", "id2"]}
If nothing resolved, return {"resolved": []}

Open commitments:
`)

	for _, c := range commitments {
		dir := "You owe"
		if c.Direction == "they_owe" {
			dir = "They owe"
		}
		sb.WriteString(fmt.Sprintf("- [ID: %s] %s: %s (%s)\n", c.ID, dir, c.Title, c.PersonName))
	}

	sb.WriteString("\nRecent messages:\n")
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
