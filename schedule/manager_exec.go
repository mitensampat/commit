package schedule

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// executeSelfDecision performs the side effects for a decision produced by a
// self-chat message.
func (m *Manager) executeSelfDecision(ctx context.Context, s *Session, dec Decision) {
	loc := m.location()
	switch dec.Action {
	case ActNone:
		m.saveSession(s)

	case ActAsk, ActAlreadyProposed, ActEditPrompt:
		m.prompt(ctx, s, dec.Reply)

	case ActReplaceDraft:
		m.prompt(ctx, s, dec.Reply)

	case ActClose:
		m.saveSession(s)
		m.sendSelfPlain(ctx, dec.Reply)

	case ActPickContact:
		if dec.Index < 1 || dec.Index > len(s.Candidates) {
			m.prompt(ctx, s, fmt.Sprintf("Pick a number between 1 and %d.", len(s.Candidates)))
			return
		}
		chosen := s.Candidates[dec.Index-1]
		cmd := s.Cmd
		s.State = StateClosed
		m.saveSession(s)
		if cmd == nil {
			cmd = &Command{Verb: s.Intent, Name: chosen.Name}
		}
		m.startSession(ctx, cmd, chosen, time.Now())

	case ActPropose:
		// Narrow to the chosen subset so slot numbering stays consistent for
		// the rest of the session.
		if len(dec.Indices) > 0 {
			var subset []Slot
			for _, i := range dec.Indices {
				subset = append(subset, s.Slots[i-1])
			}
			s.Slots = subset
			s.ProposedSlots = nil
			s.Draft = m.redraftForSubset(ctx, s)
		}
		s.SentDraft = s.Draft
		if _, err := m.Sender.SendTo(ctx, s.ContactJID, s.Draft); err != nil {
			// WhatsApp send failure → explicit failure message (req 8).
			s.State = StateSlotsProposed
			s.ProposedAt = time.Time{}
			m.prompt(ctx, s, fmt.Sprintf("Couldn't send the message to %s (WhatsApp error: %v). Nothing went out — try 'propose' again.", s.ContactName, err))
			return
		}
		m.saveSession(s)
		m.sendSelfPlain(ctx, fmt.Sprintf("Sent to %s. I'll ping you here when they reply.", s.ContactName))

	case ActRequestBooking:
		m.finalizeBooking(ctx, s, dec.Index)

	case ActCancelMeeting:
		when := ""
		if s.BookedSlot != nil {
			when = FormatSlotShort(*s.BookedSlot, loc)
		}
		note, err := GenerateDraft(ctx, m.Creds, DraftRequest{
			ContactName: s.ContactName,
			Topic:       s.Topic,
			MyStyle:     m.DB.GetMyStyle(),
			Location:    loc,
			Cancel:      true,
			BookedWhen:  when,
		})
		if err != nil {
			m.prompt(ctx, s, "Couldn't draft the cancellation note (Claude error): "+err.Error())
			return
		}
		if err := m.Cal.CancelEvent(ctx, s.BookedEventID); err != nil {
			m.prompt(ctx, s, "Couldn't delete the calendar event: "+err.Error())
			return
		}
		if _, err := m.Sender.SendTo(ctx, s.ContactJID, note); err != nil {
			m.prompt(ctx, s, fmt.Sprintf("Event deleted, but the note to %s failed to send (WhatsApp error: %v). You may want to tell them yourself.", s.ContactName, err))
			s.State = StateClosed
			m.saveSession(s)
			return
		}
		s.State = StateClosed
		m.saveSession(s)
		m.sendSelfPlain(ctx, fmt.Sprintf("Cancelled — event deleted and %s got a graceful note.", s.ContactName))

	case ActCancelSilent:
		if err := m.Cal.CancelEvent(ctx, s.BookedEventID); err != nil {
			m.prompt(ctx, s, "Couldn't delete the calendar event: "+err.Error())
			return
		}
		s.State = StateClosed
		m.saveSession(s)
		m.sendSelfPlain(ctx, "Deleted the event. Said nothing.")

	default:
		m.saveSession(s)
	}
}

// redraftForSubset regenerates the draft when only a subset of slots is sent.
func (m *Manager) redraftForSubset(ctx context.Context, s *Session) string {
	loc := m.location()
	draft, err := GenerateDraft(ctx, m.Creds, DraftRequest{
		ContactName:   s.ContactName,
		Topic:         s.Topic,
		Format:        s.Format,
		Slots:         s.Slots,
		MyStyle:       m.DB.GetMyStyle(),
		Location:      loc,
		ContactTZ:     differentTZOnly(s.ContactTZ, loc),
		ContactTZNote: s.ContactTZNote,
		StyleSamples:  m.styleSamples(s.ContactJID),
	})
	if err != nil {
		return s.Draft // fall back to the full draft rather than failing
	}
	return draft
}

// finalizeBooking implements the "yes" path: re-read the latest thread state,
// re-verify the slot, and only then book (hardening req 2).
func (m *Manager) finalizeBooking(ctx context.Context, s *Session, directIndex int) {
	loc := m.location()

	// Explicit "yes N": the user picked a specific slot themselves (after
	// seeing whatever was surfaced). That is direct, scoped consent to a
	// concrete option — no thread re-read needed, only the calendar check.
	explicit := directIndex >= 1 && directIndex <= len(s.Slots)

	// 1. Re-read the thread if there was a proposal round.
	var fresh *Interpretation
	if s.Surfaced != nil && !explicit {
		rc := ReplyContext{
			ContactName: s.ContactName,
			Slots:       s.Slots,
			Draft:       s.sentDraft(),
			Thread:      m.threadSince(s.ContactJID, s.ProposedAt),
			Now:         time.Now(),
			Location:    loc,
		}
		var err error
		fresh, err = m.Interp.InterpretReply(ctx, rc)
		if err != nil {
			m.prompt(ctx, s, "Couldn't re-read the thread before booking ("+err.Error()+") — not booking. Say 'yes' again to retry.")
			return
		}
	}

	// 2. Work out the target window.
	start, end, desc, ok := m.bookingTarget(s, fresh, directIndex, explicit)
	if !ok {
		dec := DecideBooking(s, fresh, true, time.Now())
		m.surfaceBookingDecision(ctx, s, dec)
		return
	}

	// 3. Re-verify the slot is still free.
	free, err := m.Cal.VerifyFree(ctx, start, end)
	if err != nil {
		m.prompt(ctx, s, "Calendar check failed: "+err.Error())
		return
	}

	var dec Decision
	if explicit {
		// Only the calendar gate applies to an explicit pick.
		if free {
			dec = Decision{Action: ActBook}
		} else {
			dec = Decision{Action: ActSlotTaken, Reply: "That slot got taken on your calendar since I proposed it. Want fresh options?"}
		}
	} else {
		dec = DecideBooking(s, fresh, free, time.Now())
	}
	if dec.Action != ActBook {
		m.surfaceBookingDecision(ctx, s, dec)
		return
	}

	// 4. Book.
	summary := s.ContactName
	if s.Topic != "" {
		summary += " — " + s.Topic
	}
	withMeet := s.Format != "in-person" && s.Format != "call"
	eventID, htmlLink, meetLink, err := m.Cal.Book(ctx, summary, "Scheduled via Commit. "+desc, start, end, withMeet)
	if err != nil {
		m.prompt(ctx, s, "Couldn't create the event: "+err.Error())
		return
	}
	s.BookedEventID = eventID
	slot := Slot{Start: start, End: end}
	s.BookedSlot = &slot

	// Move: delete the old event once the new one exists.
	if s.OldEventID != "" {
		if err := m.Cal.CancelEvent(ctx, s.OldEventID); err != nil {
			m.sendSelfPlain(ctx, "Heads up: new event booked, but deleting the old one failed: "+err.Error())
		}
	}

	// 5. Confirm to the counterpart (only if a proposal round happened).
	// Casual continuation tone — same register as the proposal.
	confirm := fmt.Sprintf("sounds good — %s it is.", FormatSlotShort(slot, loc))
	if meetLink != "" {
		confirm += " here's the meet link: " + meetLink + " — it's on the invite too. look forward!"
	} else if htmlLink != "" {
		confirm += " here's the calendar invite: " + htmlLink + " — click to add it to your calendar. look forward!"
	} else {
		confirm += " look forward!"
	}
	if s.Surfaced != nil {
		if _, err := m.Sender.SendTo(ctx, s.ContactJID, confirm); err != nil {
			// Booked, but the confirmation failed — be explicit (req 8).
			s.State = StateClosed
			m.saveSession(s)
			m.sendSelfPlain(ctx, fmt.Sprintf("Event booked (%s), but the confirmation to %s failed to send (WhatsApp error: %v) — you may want to confirm with them yourself.",
				FormatSlotShort(slot, loc), s.ContactName, err))
			return
		}
	}
	s.State = StateClosed
	m.saveSession(s)
	done := fmt.Sprintf("Booked: %s with %s.", FormatSlotShort(slot, loc), s.ContactName)
	if meetLink != "" {
		done += "\nMeet: " + meetLink
	}
	if htmlLink != "" {
		done += "\nCalendar: " + htmlLink
	}
	m.sendSelfPlain(ctx, done)
}

// bookingTarget resolves what window "yes" refers to.
func (m *Manager) bookingTarget(s *Session, fresh *Interpretation, directIndex int, explicit bool) (time.Time, time.Time, string, bool) {
	dur := time.Duration(s.DurationMin) * time.Minute
	if dur == 0 {
		dur = 30 * time.Minute
	}
	// Explicit "yes N" — the user's own pick wins.
	if explicit {
		sl := s.Slots[directIndex-1]
		return sl.Start, sl.End, "Picked explicitly by the user.", true
	}
	if s.Surfaced == nil {
		return time.Time{}, time.Time{}, "", false
	}
	// The engine's DecideBooking guards staleness; here we just resolve the
	// surfaced outcome to a window.
	if fresh == nil || !fresh.SameOutcome(s.Surfaced) {
		return time.Time{}, time.Time{}, "", false
	}
	switch s.Surfaced.Intent {
	case ReplyAccept:
		if s.Surfaced.SlotIndex >= 1 && s.Surfaced.SlotIndex <= len(s.Slots) {
			sl := s.Slots[s.Surfaced.SlotIndex-1]
			return sl.Start, sl.End, fmt.Sprintf("%s accepted option %d.", s.ContactName, s.Surfaced.SlotIndex), true
		}
	case ReplyCounter:
		if t, err := time.Parse(time.RFC3339, s.Surfaced.CounterTime); err == nil {
			return t, t.Add(dur), fmt.Sprintf("%s proposed this time.", s.ContactName), true
		}
	}
	return time.Time{}, time.Time{}, "", false
}

func (m *Manager) surfaceBookingDecision(ctx context.Context, s *Session, dec Decision) {
	switch dec.Action {
	case ActSurfaceChange:
		text := "Hold on — the thread moved since I pinged you:\n" + m.renderInterp(s, dec.Interp)
		if dec.Reply != "" && dec.Interp == nil {
			text = dec.Reply
		}
		text += "\n\n'yes' if that's right · 'yes N' to lock option N anyway · 'leave it'"
		m.prompt(ctx, s, text)
	case ActSlotTaken:
		m.prompt(ctx, s, dec.Reply)
	default:
		if dec.Reply != "" {
			m.prompt(ctx, s, dec.Reply)
		} else {
			m.prompt(ctx, s, "Something didn't line up — take a look at the thread and tell me what to do.")
		}
	}
}

// renderInterp renders an interpretation for the self-chat.
func (m *Manager) renderInterp(s *Session, interp *Interpretation) string {
	if interp == nil {
		return "(couldn't read the thread)"
	}
	loc := m.location()
	var b strings.Builder
	switch interp.Intent {
	case ReplyAccept:
		if interp.SlotIndex >= 1 && interp.SlotIndex <= len(s.Slots) {
			b.WriteString(fmt.Sprintf("%s is good with *%s*.", s.ContactName, FormatSlotShort(s.Slots[interp.SlotIndex-1], loc)))
		} else {
			b.WriteString(fmt.Sprintf("%s agreed, but I'm not sure to which option.", s.ContactName))
		}
	case ReplyCounter:
		if t, err := time.Parse(time.RFC3339, interp.CounterTime); err == nil {
			b.WriteString(fmt.Sprintf("%s proposed *%s* instead.", s.ContactName, t.In(loc).Format("Mon 2, 3:04 PM")))
		} else {
			b.WriteString(fmt.Sprintf("%s proposed a different time.", s.ContactName))
		}
	case ReplyReject:
		b.WriteString(fmt.Sprintf("%s can't make any of these.", s.ContactName))
	case ReplyAmbiguous:
		b.WriteString(fmt.Sprintf("%s replied but it's not a clear yes — take a look at the chat.", s.ContactName))
	case ReplyUnrelated:
		b.WriteString(fmt.Sprintf("%s messaged, but not about the meeting.", s.ContactName))
	}
	if interp.SideNote != "" {
		b.WriteString("\nAlso: " + interp.SideNote)
	}
	return b.String()
}

// ── watcher entry point ──

// OnContactMessage is called for every saved message in a 1:1 chat that has
// an open session (both directions). It surfaces counterpart replies and
// stands down on manual resolution.
func (m *Manager) OnContactMessage(ctx context.Context, chatJID string, isFromMe bool, text string, ts time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.openSessionFor(chatJID)
	if s == nil || (s.State != StateAwaitingReply && s.State != StateReplySurfaced) {
		return
	}
	// Ignore our own outbound artifacts (the proposal draft itself).
	if isFromMe && strings.TrimSpace(text) == strings.TrimSpace(s.sentDraft()) {
		return
	}

	rc := ReplyContext{
		ContactName: s.ContactName,
		Slots:       s.Slots,
		Draft:       s.sentDraft(),
		Thread:      m.threadSince(s.ContactJID, s.ProposedAt),
		Now:         time.Now(),
		Location:    m.location(),
	}

	if isFromMe {
		// Manual resolution detection (req 9).
		finalized, err := m.Interp.InterpretOwnMessage(ctx, rc)
		if err != nil {
			log.Printf("schedule: own-message interpretation failed: %v", err)
			return
		}
		dec := HandleOwnMessage(s, finalized, time.Now())
		if dec.Action == ActStandDown {
			m.saveSession(s)
			log.Printf("schedule: standing down for %s — user resolved manually", s.ContactName)
		}
		return
	}

	interp, err := m.Interp.InterpretReply(ctx, rc)
	if err != nil {
		// Interpretation failure must not silently drop a reply (req 7/8).
		m.prompt(ctx, s, fmt.Sprintf("%s replied but I couldn't read the thread (%v) — take a look.", s.ContactName, err))
		return
	}

	// Don't re-prompt if nothing materially changed since the last surface.
	if s.State == StateReplySurfaced && interp.SameOutcome(s.Surfaced) {
		return
	}

	dec := HandleCounterpartReply(s, interp, ts, time.Now())
	switch dec.Action {
	case ActNone:
		m.saveSession(s)
	case ActSurfaceReply:
		text := m.renderInterp(s, dec.Interp)
		switch dec.Interp.Intent {
		case ReplyAccept:
			text += "\n\n'yes' to book · 'edit' · 'leave it'"
		case ReplyReject:
			text += "\n\n'leave it' to drop, or '@schedule " + strings.ToLower(firstWord(s.ContactName)) + " next week' to try a new window"
		default:
			text += "\n\n'yes' if it's actually settled · 'edit' · 'leave it'"
		}
		m.prompt(ctx, s, text)
	case ActVerifyCounter:
		m.handleCounterVerification(ctx, s, dec.Interp)
	}
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return s
	}
	return f[0]
}

// handleCounterVerification checks an unoffered counterpart time against the
// calendar and surfaces it as a pickable option (hardening req 10).
func (m *Manager) handleCounterVerification(ctx context.Context, s *Session, interp *Interpretation) {
	loc := m.location()
	t, err := time.Parse(time.RFC3339, interp.CounterTime)
	if err != nil {
		m.prompt(ctx, s, fmt.Sprintf("%s proposed a different time but I couldn't pin it down — take a look at the chat.", s.ContactName))
		return
	}
	dur := time.Duration(s.DurationMin) * time.Minute
	if dur == 0 {
		dur = 30 * time.Minute
	}
	free, err := m.Cal.VerifyFree(ctx, t, t.Add(dur))
	if err != nil {
		m.prompt(ctx, s, "Calendar check failed while verifying their proposal: "+err.Error())
		return
	}
	when := t.In(loc).Format("Mon 2, 3:04 PM")
	if free {
		m.prompt(ctx, s, fmt.Sprintf("%s proposed *%s* — that's free on your side.\n\n'yes' to book it · 'edit' · 'leave it'", s.ContactName, when))
	} else {
		m.prompt(ctx, s, fmt.Sprintf("%s proposed *%s* but you're busy then. Reply with what to do — 'edit' to counter, or 'leave it'.", s.ContactName, when))
	}
}

// ── expiry sweep ──

// RunExpirySweep closes sessions silent for 48h. Call periodically.
func (m *Manager) RunExpirySweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.allOpenSessions() {
		if dec := CheckExpiry(s, now); dec.Action == ActExpire {
			m.saveSession(s)
			log.Printf("schedule: session with %s expired silently after 48h", s.ContactName)
		}
	}
}

// sentDraft returns the draft as sent to the counterpart, falling back to the
// current draft when nothing has been sent yet.
func (s *Session) sentDraft() string {
	if s.SentDraft != "" {
		return s.SentDraft
	}
	return s.Draft
}
