package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/msfoundry/commit/store"
)

// Manager orchestrates scheduling sessions: it owns persistence and side
// effects, and delegates every decision to the pure engine. It is transport-
// agnostic — the whatsapp package and the dry-run harness both drive it
// through the same three entry points:
//
//	HandleSelfChat   — a message in the user's self-chat
//	OnContactMessage — a message in a chat that has an open session
//	RunExpirySweep   — periodic 48h-silence cleanup
type Manager struct {
	DB     *store.DB
	Cal    CalendarService
	Interp ReplyInterpreter
	// Classifier reads the user's own self-chat free text (instruction vs
	// draft). When nil, free text is never armed as a draft — we ask instead.
	Classifier SelfTextClassifier
	Sender     Sender
	Creds      Creds
	SelfJID    func() string // the self-chat JID as seen in incoming messages

	mu sync.Mutex // serializes session mutations
}

// computeSlotsFor runs the window → slots search for the session's CURRENT
// shape (window, duration, format), honoring the days the counterpart asked
// for and falling back loudly when nothing is free on them.
//
// The search never starts before now: a session that sat overnight must not
// re-offer this morning's slots.
func (m *Manager) computeSlotsFor(ctx context.Context, s *Session) ([]Slot, error) {
	loc := m.location()
	now := time.Now()
	from, to := WindowRange(s.Window, now, loc)
	if from.Before(now) {
		from = now
	}
	if !to.After(from) {
		// The window has already gone by entirely ("today", read tomorrow).
		// Fall back to the default forward horizon rather than searching an
		// empty or backwards range.
		from, to = now, now.AddDate(0, 0, 7)
	}
	dur := time.Duration(s.DurationMin) * time.Minute
	if dur == 0 {
		dur = 30 * time.Minute
	}
	prefDays := PreferredDays(s.Window)
	inPerson := s.Format == "in-person"

	slots, err := m.Cal.ComputeSlots(ctx, from, to, dur, inPerson, prefDays)
	if err != nil {
		return nil, err
	}
	s.RequestedDays = FormatDays(prefDays)
	s.PreferenceMet = true
	if len(slots) == 0 && len(prefDays) > 0 {
		s.PreferenceMet = false
		slots, err = m.Cal.ComputeSlots(ctx, from, to, dur, inPerson, nil)
		if err != nil {
			return nil, err
		}
	}
	// Belt and braces: whatever the calendar returns, a past slot is never
	// offerable.
	var out []Slot
	for _, sl := range slots {
		if sl.Start.After(now) {
			out = append(out, sl)
		}
	}
	return out, nil
}

// redraft regenerates the outbound message for the session's current shape.
func (m *Manager) redraft(ctx context.Context, s *Session) (string, error) {
	loc := m.location()
	return GenerateDraft(ctx, m.Creds, DraftRequest{
		ContactName:   s.ContactName,
		Topic:         s.Topic,
		Format:        s.Format,
		Slots:         s.Slots,
		MyStyle:       m.DB.GetMyStyle(),
		Location:      loc,
		ContactTZ:     differentTZOnly(s.ContactTZ, loc),
		ContactTZNote: s.ContactTZNote,
		StyleSamples:  m.styleSamples(s.ContactJID),
		RequestedDays: s.RequestedDays,
		PreferenceMet: s.PreferenceMet,
		ToneNote:      s.ToneNote,
	})
}

// resurfaceOptions recomputes slots + draft for the session's current shape
// and puts them back in front of the user for a 'propose'. Used whenever the
// shape changed underneath the old options: a scope change, a user
// instruction, or slots that simply expired.
func (m *Manager) resurfaceOptions(ctx context.Context, s *Session, header string) {
	loc := m.location()
	slots, err := m.computeSlotsFor(ctx, s)
	if err != nil {
		m.prompt(ctx, s, "Calendar error: "+err.Error())
		return
	}
	if len(slots) == 0 {
		m.prompt(ctx, s, header+fmt.Sprintf("\n\nBut you have nothing free for a %d-min %s with %s in that window. Try another window — '@schedule %s next week'.",
			s.DurationMin, ifStr(s.Format != "", s.Format, "meeting"), s.ContactName, strings.ToLower(firstWord(s.ContactName))))
		return
	}
	s.Slots = slots
	draft, err := m.redraft(ctx, s)
	if err != nil {
		m.prompt(ctx, s, "Couldn't redraft the message (Claude error): "+err.Error())
		return
	}
	s.Draft = draft
	// The old proposal round is void: these are different options.
	s.ProposedAt = time.Time{}
	s.ProposedSlots = nil
	s.Surfaced = nil
	s.State = StateSlotsProposed

	var sb strings.Builder
	sb.WriteString(header)
	if !s.PreferenceMet && s.RequestedDays != "" {
		sb.WriteString(fmt.Sprintf("\n\n⚠️ %s asked for %s — you have nothing free on those days. Closest alternatives below; the draft says so.",
			s.ContactName, s.RequestedDays))
	}
	sb.WriteString("\n\nFree options:\n")
	sb.WriteString(FormatSlotList(s.Slots, loc))
	sb.WriteString("\n\nDraft to send:\n———\n" + s.Draft + "\n———\n\n")
	sb.WriteString("'propose' to send it · 'propose 1 3' for a subset · 'yes N' to book directly · 'edit' · 'leave it'")
	m.prompt(ctx, s, sb.String())
}

func (m *Manager) location() *time.Location {
	prefs := m.DB.GetSchedulePrefs()
	if prefs.Timezone != "" {
		if loc, err := time.LoadLocation(prefs.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

// ── persistence ──

func (m *Manager) saveSession(s *Session) {
	data, err := json.Marshal(s)
	if err != nil {
		log.Printf("schedule: marshal session: %v", err)
		return
	}
	row := &store.ScheduleSessionRow{
		ID:          s.ID,
		ContactJID:  s.ContactJID,
		ContactName: s.ContactName,
		State:       string(s.State),
		Intent:      string(s.Intent),
		Data:        string(data),
		CreatedAt:   s.CreatedAt,
	}
	if s.State == StateClosed {
		row.ClosedAt = time.Now()
	}
	if err := m.DB.SaveScheduleSession(row); err != nil {
		log.Printf("schedule: save session: %v", err)
	}
}

func sessionFromRow(row *store.ScheduleSessionRow) *Session {
	if row == nil {
		return nil
	}
	var s Session
	if err := json.Unmarshal([]byte(row.Data), &s); err != nil {
		log.Printf("schedule: unmarshal session %s: %v", row.ID, err)
		return nil
	}
	if s.ID == "" {
		s.ID = row.ID
	}
	return &s
}

func (m *Manager) openSessionFor(contactJID string) *Session {
	row, err := m.DB.GetOpenScheduleSession(contactJID)
	if err != nil || row == nil {
		return nil
	}
	return sessionFromRow(row)
}

func (m *Manager) allOpenSessions() []*Session {
	rows, err := m.DB.GetOpenScheduleSessions()
	if err != nil {
		return nil
	}
	var out []*Session
	for _, r := range rows {
		if s := sessionFromRow(r); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// ── self-chat entry point ──

// HandleSelfChat processes a self-chat message. Returns true if the message
// was consumed by the scheduler (so the caller skips other handling).
// msgID/ts identify the incoming message for the adjacency check.
func (m *Manager) HandleSelfChat(ctx context.Context, text, msgID string, ts time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)

	if strings.HasPrefix(lower, "@schedule") {
		rest := strings.TrimSpace(trimmed[len("@schedule"):])
		// "@schedule yes" etc: prefixed consent words always count.
		if _, ok := parseConsentText(rest); ok {
			if s := m.latestPromptedSession(); s != nil {
				m.runSelfChatDecision(ctx, s, SelfChatInput{Text: rest, Now: ts, ForceScoped: true})
				return true
			}
			m.sendSelfPlain(ctx, "No active scheduling session.")
			return true
		}
		m.handleCommand(ctx, rest, ts)
		return true
	}

	// Bare consent words / draft edits: route to the most recently prompted
	// open session; the engine decides whether the message is in scope.
	s := m.latestPromptedSession()
	if s == nil {
		return false
	}
	in := SelfChatInput{
		Text:              trimmed,
		Now:               ts,
		IsNextAfterPrompt: m.isNextAfterPrompt(s, msgID, ts),
	}
	// A personal note must fall through untouched (and unconsumed).
	before := s.State
	dec := HandleSelfChat(s, in)
	if dec.Action == ActNone && before == s.State {
		return false
	}
	m.executeSelfDecision(ctx, s, dec)
	return true
}

func parseConsentText(text string) (consentCmd, bool) { return parseConsent(text) }

func (m *Manager) latestPromptedSession() *Session {
	sessions := m.allOpenSessions()
	if len(sessions) == 0 {
		return nil
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastPromptAt.After(sessions[j].LastPromptAt)
	})
	return sessions[0]
}

// isNextAfterPrompt reports whether msgID is the first self-chat message
// after the session's last prompt (hardening req 3).
func (m *Manager) isNextAfterPrompt(s *Session, msgID string, ts time.Time) bool {
	if s.LastPromptAt.IsZero() || m.SelfJID == nil {
		return false
	}
	selfJID := m.SelfJID()
	if selfJID == "" {
		return false
	}
	n, err := m.DB.CountMessagesBetween(selfJID, s.LastPromptAt.Unix(), ts.Unix(), []string{msgID, s.LastPromptID})
	if err != nil {
		return false
	}
	return n == 0
}

func (m *Manager) runSelfChatDecision(ctx context.Context, s *Session, in SelfChatInput) {
	dec := HandleSelfChat(s, in)
	m.executeSelfDecision(ctx, s, dec)
}

// sendSelfPlain sends without touching any session prompt bookkeeping.
func (m *Manager) sendSelfPlain(ctx context.Context, text string) {
	if _, err := m.Sender.SendSelf(ctx, text); err != nil {
		log.Printf("schedule: self send failed: %v", err)
	}
}

// prompt sends a self-chat message that opens/renews the consent window.
func (m *Manager) prompt(ctx context.Context, s *Session, text string) {
	id, err := m.Sender.SendSelf(ctx, text)
	if err != nil {
		log.Printf("schedule: prompt send failed: %v", err)
		return
	}
	s.MarkPrompted(time.Now())
	s.LastPromptID = id
	m.saveSession(s)
}

// ── @schedule command ──

func (m *Manager) handleCommand(ctx context.Context, rest string, ts time.Time) {
	cmd, err := ParseCommand(rest)
	if err != nil {
		m.sendSelfPlain(ctx, err.Error())
		return
	}

	// Instant ack, before any slow work.
	m.sendSelfPlain(ctx, "on it — checking your calendar…")

	cands := m.resolveContacts(cmd.Name)
	switch len(cands) {
	case 0:
		m.sendSelfPlain(ctx, fmt.Sprintf("I couldn't find anyone matching \"%s\" in your chats.", cmd.Name))
		return
	case 1:
		m.startSession(ctx, cmd, cands[0], ts)
	default:
		// AMBIGUOUS → always ask, never guess (hardening req 1).
		if existing := m.openSessionFor(cands[0].JID); existing != nil && existing.State == StateResolving {
			// fall through: re-ask below
		}
		s := &Session{
			ID:           store.GenerateScheduleSessionID(),
			ContactJID:   "pending:" + strings.ToLower(cmd.Name),
			ContactName:  cmd.Name,
			State:        StateResolving,
			Intent:       cmd.Verb,
			Cmd:          cmd,
			Candidates:   cands,
			CreatedAt:    ts,
			LastActivity: ts,
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("A few people match \"%s\" — who did you mean?\n", cmd.Name))
		for i, c := range cands {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, c.Name))
		}
		sb.WriteString("Reply with a number, or 'leave it'.")
		m.prompt(ctx, s, sb.String())
	}
}

// resolveContacts fuzzy-matches a name against known 1:1 chats.
// resolveContacts finds chats matching the typed name. Users type the name
// they see in WhatsApp (their address book — "Allish Jain"), while the chat's
// own metadata may only carry a push name ("Allish"), so every known name for
// a contact is considered. Only the best-scoring band is returned: a clear
// winner resolves, a tie asks (hardening req 1 — never guess).
func (m *Manager) resolveContacts(name string) []ContactCandidate {
	chats, err := m.DB.GetDirectChats()
	if err != nil {
		return nil
	}
	q := normalizeQuery(name)
	if q == "" {
		return nil
	}

	type scored struct {
		ContactCandidate
		score int
	}
	var all []scored
	seen := map[string]bool{}

	for jid, display := range chats {
		if strings.HasSuffix(jid, "@g.us") || strings.Contains(jid, "@broadcast") {
			continue
		}
		// Every name this identity is known by: chat display + synced
		// address-book names.
		candidates := []string{display}
		known := m.DB.GetContactNames(jid)
		candidates = append(candidates, known.Names()...)

		best, bestName := 0, display
		for _, cand := range candidates {
			if s := nameMatchScore(q, cand); s > best {
				best, bestName = s, cand
			}
		}
		if best == 0 {
			continue
		}
		// Prefer the fullest known name for display, not whatever matched.
		if full := known.Best(); full != "" && len(full) > len(bestName) {
			bestName = full
		}
		if seen[jid] {
			continue
		}
		seen[jid] = true
		all = append(all, scored{ContactCandidate{JID: jid, Name: bestName}, best})
	}
	if len(all) == 0 {
		return nil
	}

	// Keep only the strongest band — "Allish Jain" shouldn't drag in every
	// weak partial match once an exact-ish one exists.
	top := 0
	for _, s := range all {
		if s.score > top {
			top = s.score
		}
	}
	var out []ContactCandidate
	dedup := map[string]bool{}
	for _, s := range all {
		if s.score != top {
			continue
		}
		if k := strings.ToLower(s.Name); !dedup[k] {
			dedup[k] = true
			out = append(out, s.ContactCandidate)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// normalizeQuery lowercases and collapses whitespace so "  KUNAL   shah "
// and "kunal shah" are the same query.
func normalizeQuery(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// nameMatchScore rates how well a typed name matches a known one. 0 = no
// match. Higher bands beat lower ones outright, so a full-name hit wins over
// incidental first-name overlap with other contacts.
func nameMatchScore(query, target string) int {
	t := strings.ToLower(strings.TrimSpace(target))
	if query == "" || t == "" {
		return 0
	}
	switch {
	case t == query:
		return 100
	// "allish jain" typed, "allish" known (or the reverse): same person.
	case strings.Contains(t, query), strings.Contains(query, t):
		return 90
	}
	qWords, tWords := strings.Fields(query), strings.Fields(t)
	matched := 0
	for _, qw := range qWords {
		for _, tw := range tWords {
			if strings.HasPrefix(tw, qw) || strings.HasPrefix(qw, tw) {
				matched++
				break
			}
		}
	}
	switch {
	case matched == 0:
		return 0
	case matched == len(qWords):
		return 70 // every word the user typed landed
	default:
		return 40 + matched // partial overlap — a candidate worth asking about
	}
}

// startSession runs the full command flow for a resolved contact.
func (m *Manager) startSession(ctx context.Context, cmd *Command, contact ContactCandidate, ts time.Time) {
	// One session per contact: a new @schedule replaces any stale one.
	if existing := m.openSessionFor(contact.JID); existing != nil {
		existing.State = StateClosed
		m.saveSession(existing)
	}

	switch cmd.Verb {
	case IntentCancel:
		m.startCancel(ctx, contact, ts)
		return
	case IntentMove:
		// Move = rebook: remember the old event, then run the schedule flow.
	}

	if !m.Cal.Connected() {
		m.sendSelfPlain(ctx, "Google Calendar isn't connected — open Commit Settings → Scheduling and hit Connect, then try again.")
		return
	}

	s := &Session{
		ID:           store.GenerateScheduleSessionID(),
		ContactJID:   contact.JID,
		ContactName:  contact.Name,
		State:        StateSlotsProposed,
		Intent:       cmd.Verb,
		Cmd:          cmd,
		CreatedAt:    ts,
		LastActivity: ts,
	}
	if cmd.Verb == IntentMove {
		if old := m.lastBookedSession(contact.JID); old != nil {
			s.OldEventID = old.BookedEventID
		} else {
			m.sendSelfPlain(ctx, fmt.Sprintf("I don't have a meeting on record with %s to move — scheduling a fresh one instead.", contact.Name))
		}
	}

	// Context inference from the recent thread.
	thread := m.recentThread(contact.JID, 10)
	ic, err := InferContext(ctx, m.Creds, contact.Name, thread, cmd)
	if err != nil {
		m.sendSelfPlain(ctx, "Couldn't infer meeting context (Claude error): "+err.Error())
		return
	}
	s.Topic = ic.Topic
	s.DurationMin = ic.DurationMin
	s.Format = ic.Format
	s.Window = ic.Window

	// Timezone: per-contact override beats phone-prefix inference (req 5).
	if tz := m.DB.GetContactTZOverride(contact.JID); tz != "" {
		s.ContactTZ = tz
		s.ContactTZNote = "you told me they're in " + tz
	} else {
		s.ContactTZ, s.ContactTZNote = InferContactTZ(contact.JID)
	}

	// Slot computation. Honors the days they actually asked for; if nothing is
	// free on those days it falls back to the rest of the window — but never
	// silently: both the user and the draft have to acknowledge the miss.
	loc := m.location()
	slots, err := m.computeSlotsFor(ctx, s)
	if err != nil {
		// OAuth failures must be loud (req 7).
		m.sendSelfPlain(ctx, "Calendar error: "+err.Error())
		return
	}
	if len(slots) == 0 {
		m.sendSelfPlain(ctx, fmt.Sprintf("No free slots for %s in that window (%s). Try a different window?",
			contact.Name, ifStr(s.Window != "", s.Window, "the next week")))
		return
	}
	s.Slots = slots

	// Draft in the user's style.
	draft, err := m.redraft(ctx, s)
	if err != nil {
		m.sendSelfPlain(ctx, "Couldn't draft the message (Claude error): "+err.Error())
		return
	}
	s.Draft = draft

	// The reply always echoes the resolved full name + inferred context so a
	// wrong fuzzy match gets caught here (hardening req 1).
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Scheduling with *%s* — %s, %d min", s.ContactName, s.Topic, s.DurationMin))
	if s.Format != "" {
		sb.WriteString(", " + s.Format)
	}
	if s.Window != "" {
		sb.WriteString(", " + s.Window)
	}
	if s.ContactTZ != "" && s.ContactTZ != loc.String() {
		sb.WriteString(fmt.Sprintf("\n(their timezone: %s — %s)", s.ContactTZ, s.ContactTZNote))
	}
	if !s.PreferenceMet && s.RequestedDays != "" {
		sb.WriteString(fmt.Sprintf("\n\n⚠️ %s asked for %s — you have nothing free on those days. Closest alternatives below; the draft says so.",
			s.ContactName, s.RequestedDays))
	}
	sb.WriteString("\n\nFree options:\n")
	sb.WriteString(FormatSlotList(s.Slots, loc))
	sb.WriteString("\n\nDraft to send:\n———\n" + s.Draft + "\n———\n\n")
	sb.WriteString("'propose' to send it · 'propose 1 3' for a subset · 'yes N' to book directly · 'edit' · 'leave it'")
	m.prompt(ctx, s, sb.String())
}

func differentTZOnly(tz string, loc *time.Location) string {
	if tz == "" || tz == loc.String() {
		return ""
	}
	if ctz, err := time.LoadLocation(tz); err == nil {
		// Same offset right now → not worth confusing anyone.
		now := time.Now()
		_, o1 := now.In(ctz).Zone()
		_, o2 := now.In(loc).Zone()
		if o1 == o2 {
			return ""
		}
	}
	return tz
}

func (m *Manager) lastBookedSession(contactJID string) *Session {
	// Booked sessions are closed; look up the most recent one for the contact.
	row, err := m.DB.GetLastBookedScheduleSession(contactJID)
	if err != nil || row == nil {
		return nil
	}
	return sessionFromRow(row)
}

func (m *Manager) startCancel(ctx context.Context, contact ContactCandidate, ts time.Time) {
	booked := m.lastBookedSession(contact.JID)
	if booked == nil || booked.BookedEventID == "" {
		m.sendSelfPlain(ctx, fmt.Sprintf("I don't have a meeting on record with %s to cancel.", contact.Name))
		return
	}
	loc := m.location()
	when := ""
	if booked.BookedSlot != nil {
		when = FormatSlotShort(*booked.BookedSlot, loc)
	}
	s := &Session{
		ID:            store.GenerateScheduleSessionID(),
		ContactJID:    contact.JID,
		ContactName:   contact.Name,
		State:         StateConfirmCancel,
		Intent:        IntentCancel,
		Topic:         booked.Topic,
		BookedEventID: booked.BookedEventID,
		BookedSlot:    booked.BookedSlot,
		CreatedAt:     ts,
		LastActivity:  ts,
	}
	m.prompt(ctx, s, fmt.Sprintf("Cancel your meeting with *%s*%s?\n\n'yes' sends them a graceful note · 'yes silent' just deletes the event · 'leave it'",
		contact.Name, ifStr(when != "", " ("+when+")", "")))
}

func ifStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// recentThread converts recent chat messages to interpreter ThreadMsgs.
func (m *Manager) recentThread(chatJID string, n int) []ThreadMsg {
	msgs, err := m.DB.GetLastNMessagesForChat(chatJID, n)
	if err != nil {
		return nil
	}
	var out []ThreadMsg
	for _, msg := range msgs {
		out = append(out, ThreadMsg{FromMe: msg.IsFromMe, Text: msg.Content, Time: msg.Timestamp})
	}
	return out
}

// threadSince returns messages in the contact chat after t (for re-reads).
func (m *Manager) threadSince(chatJID string, t time.Time) []ThreadMsg {
	msgs, err := m.DB.GetRecentMessagesForChat(chatJID, t)
	if err != nil {
		return nil
	}
	var out []ThreadMsg
	for _, msg := range msgs {
		out = append(out, ThreadMsg{FromMe: msg.IsFromMe, Text: msg.Content, Time: msg.Timestamp})
	}
	return out
}

func (m *Manager) styleSamples(chatJID string) []string {
	msgs, err := m.DB.GetLastNMessagesForChat(chatJID, 30)
	if err != nil {
		return nil
	}
	var out []string
	for i := len(msgs) - 1; i >= 0 && len(out) < 5; i-- {
		if msgs[i].IsFromMe && len(msgs[i].Content) > 10 && !strings.HasPrefix(msgs[i].Content, "@") {
			out = append(out, msgs[i].Content)
		}
	}
	return out
}
