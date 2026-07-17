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

	case ActClassifyText:
		m.classifyAndApply(ctx, s, dec.Text)

	case ActApplyInstruction:
		m.applyInstruction(ctx, s, dec.Class)

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
	draft, err := m.redraft(ctx, s)
	if err != nil {
		return s.Draft // fall back to the full draft rather than failing
	}
	return draft
}

// pastSlotHeader explains, in the user's terms, why the old options are gone.
func (m *Manager) pastSlotHeader(s *Session, dec Decision) string {
	loc := m.location()
	name := s.ContactName
	switch dec.Reason {
	case "accepted_slot_passed", "held_slot_passed":
		if dec.Index >= 1 && dec.Index <= len(s.Slots) {
			return fmt.Sprintf("%s picked *%s* — but that's already been and gone. Not booking it. Fresh options:",
				name, FormatSlotShort(s.Slots[dec.Index-1], loc))
		}
		return fmt.Sprintf("%s picked a slot that's already passed. Not booking it. Fresh options:", name)
	case "all_slots_passed":
		return fmt.Sprintf("%s left it to me to pick, but every option I offered has already passed. Fresh options:", name)
	case "target_passed":
		return "That time has already passed — not booking it. Fresh options:"
	}
	return "Those options have expired. Fresh options:"
}

// classifyAndApply is the instruction-vs-draft gate. The user's free text over
// a pending draft is NEVER armed as the outbound message on a guess: an
// instruction ("he asked for Tue or Wed, our proposal is wrong") silently
// becoming the message sent to him is the failure this exists to prevent.
func (m *Manager) classifyAndApply(ctx context.Context, s *Session, text string) {
	if m.Classifier == nil {
		m.prompt(ctx, s, "Not sure if that's a note for me or the message you want sent. Say 'edit' first if it's the message, or tell me what to change.")
		return
	}
	class, err := m.Classifier.ClassifySelfText(ctx, SelfTextContext{
		ContactName: s.ContactName,
		Text:        text,
		Draft:       s.Draft,
		Slots:       s.Slots,
		Topic:       s.Topic,
		DurationMin: s.DurationMin,
		Format:      s.Format,
		Now:         time.Now(),
		Location:    m.location(),
	})
	if err != nil {
		// Can't tell what they meant → ask. Never fall back to arming it.
		log.Printf("schedule: self-text classification failed: %v", err)
		m.prompt(ctx, s, "I couldn't tell whether that was an instruction or the message text ("+err.Error()+"). Say 'edit' first if it's the message you want sent, or tell me what to change.")
		return
	}
	dec := ApplySelfText(s, text, class, time.Now())
	m.executeSelfDecision(ctx, s, dec)
}

// applyInstruction acts on a user instruction: recompute when the search
// itself changed, otherwise just rewrite.
func (m *Manager) applyInstruction(ctx context.Context, s *Session, class *SelfTextClass) {
	if class == nil {
		m.saveSession(s)
		return
	}
	var changed []string
	if class.Window != "" {
		changed = append(changed, "window → "+class.Window)
	}
	if class.DurationMin > 0 {
		changed = append(changed, fmt.Sprintf("duration → %d min", class.DurationMin))
	}
	if class.Format != "" {
		changed = append(changed, "format → "+class.Format)
	}
	if class.ToneNote != "" {
		changed = append(changed, "tone → "+class.ToneNote)
	}
	header := "Got it — " + strings.Join(changed, ", ") + "."

	if class.NeedsRecompute() {
		m.resurfaceOptions(ctx, s, header)
		return
	}

	// Tone-only: the options stand, only the wording changes.
	draft, err := m.redraft(ctx, s)
	if err != nil {
		m.prompt(ctx, s, "Couldn't redraft the message (Claude error): "+err.Error())
		return
	}
	s.Draft = draft
	s.State = StateSlotsProposed
	loc := m.location()
	m.prompt(ctx, s, header+"\n\nFree options:\n"+FormatSlotList(s.Slots, loc)+
		"\n\nDraft to send:\n———\n"+s.Draft+"\n———\n\n'propose' to send it · 'yes N' to book directly · 'edit' · 'leave it'")
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
		// Only the calendar gate applies to an explicit pick — plus the one
		// gate the user cannot waive: a slot in the past is unbookable no
		// matter how explicitly they picked it.
		switch {
		case !start.After(time.Now()):
			dec = Decision{Action: ActSlotPast, Index: directIndex, Reason: "target_passed"}
		case free:
			dec = Decision{Action: ActBook}
		default:
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
	m.bookSlot(ctx, s, Slot{Start: start, End: end}, desc)
}

// bookSlot creates the event, confirms to the counterpart when a proposal
// round happened, and closes the session. Shared by the consented "yes" path
// and the deference auto-pick.
func (m *Manager) bookSlot(ctx context.Context, s *Session, slot Slot, desc string) {
	loc := m.location()
	start, end := slot.Start, slot.End

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
	case ReplyDeference:
		// They deferred; we picked; the user said yes to that pick.
		if s.PickedIndex >= 1 && s.PickedIndex <= len(s.Slots) {
			sl := s.Slots[s.PickedIndex-1]
			return sl.Start, sl.End, fmt.Sprintf("%s left the choice to the user; Commit proposed option %d and the user confirmed.", s.ContactName, s.PickedIndex), true
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
	case ActSlotPast:
		m.resurfaceOptions(ctx, s, m.pastSlotHeader(s, dec))
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
	case ReplySoftYes:
		if interp.SlotIndex >= 1 && interp.SlotIndex <= len(s.Slots) {
			b.WriteString(fmt.Sprintf("%s is leaning *%s* — but it's a soft yes, not a commitment. I haven't booked it, and I'm still watching for them to firm up.",
				s.ContactName, FormatSlotShort(s.Slots[interp.SlotIndex-1], loc)))
		} else {
			b.WriteString(fmt.Sprintf("%s hedged — nothing booked.", s.ContactName))
		}
	case ReplyDeference:
		if s.PickedIndex >= 1 && s.PickedIndex <= len(s.Slots) {
			b.WriteString(fmt.Sprintf("%s left the pick to me — I'd take *%s*.", s.ContactName, FormatSlotShort(s.Slots[s.PickedIndex-1], loc)))
		} else {
			b.WriteString(fmt.Sprintf("%s left the pick to me.", s.ContactName))
		}
	case ReplyScopeChange:
		b.WriteString(fmt.Sprintf("%s wants to change the shape of this.", s.ContactName))
	case ReplyDirective:
		if t, err := time.Parse(time.RFC3339, interp.CounterTime); err == nil {
			b.WriteString(fmt.Sprintf("%s told you to make it *%s*.", s.ContactName, t.In(loc).Format("Mon 2, 3:04 PM")))
		} else {
			b.WriteString(fmt.Sprintf("%s named a time.", s.ContactName))
		}
	case ReplyNotScheduling:
		b.WriteString(fmt.Sprintf("%s isn't scheduling this here.", s.ContactName))
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
	if s == nil || !s.watching() {
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
	if (s.State == StateReplySurfaced || s.State == StateHeld) && interp.SameOutcome(s.Surfaced) {
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
	case ActHold:
		// A soft yes books NOTHING. Say what it points at, stay open, keep
		// watching for them to firm up.
		text := m.renderInterp(s, dec.Interp)
		text += "\n\n'yes' to lock it anyway · 'leave it' to drop · otherwise I'll wait for them."
		m.prompt(ctx, s, text)
	case ActSurfacePick:
		m.handleDeferencePick(ctx, s, dec)
	case ActScopeChange:
		m.handleScopeChange(ctx, s, dec.Interp)
	case ActNotScheduling:
		m.handleNotScheduling(ctx, s, dec)
	case ActSlotPast:
		m.resurfaceOptions(ctx, s, m.pastSlotHeader(s, dec))
	}
}

// handleDeferencePick shows the pick we made when the counterpart handed the
// choice back. It verifies the slot is still free and then asks for the one
// word — it does NOT book. Making the pick is the fussiness we removed; asking
// for consent is not fussiness, it's the invariant: nothing reaches the
// counterpart without the user's word.
func (m *Manager) handleDeferencePick(ctx context.Context, s *Session, dec Decision) {
	loc := m.location()
	if dec.Index < 1 || dec.Index > len(s.Slots) {
		m.prompt(ctx, s, fmt.Sprintf("%s left the pick to me but I've lost track of the options — say 'yes N' to lock one.", s.ContactName))
		return
	}
	sl := s.Slots[dec.Index-1]
	free, err := m.Cal.VerifyFree(ctx, sl.Start, sl.End)
	if err != nil {
		m.prompt(ctx, s, "Calendar check failed while picking for you: "+err.Error())
		return
	}
	if !free {
		m.prompt(ctx, s, fmt.Sprintf("%s left the pick to me, but *%s* just got taken on your calendar. Say 'yes N' for another, or 'leave it'.",
			s.ContactName, FormatSlotShort(sl, loc)))
		return
	}
	text := fmt.Sprintf("%s left the pick to me — I'd take *%s* (%s).",
		s.ContactName, FormatSlotShort(sl, loc), dec.Reason)
	if dec.Interp != nil && dec.Interp.SideNote != "" {
		text += "\nAlso: " + dec.Interp.SideNote
	}
	text += "\n\n'yes' to book it and confirm to them · 'yes N' for another · 'leave it'"
	m.prompt(ctx, s, text)
}

// handleScopeChange recomputes for the new shape. A 30-minute slot may not
// hold an hour and an in-person meeting needs the travel buffer, so the old
// options cannot simply be re-offered.
func (m *Manager) handleScopeChange(ctx context.Context, s *Session, interp *Interpretation) {
	var parts []string
	if interp.NewDurationMin > 0 {
		parts = append(parts, fmt.Sprintf("%d min", interp.NewDurationMin))
	}
	if interp.NewFormat != "" {
		parts = append(parts, interp.NewFormat)
	}
	header := fmt.Sprintf("%s wants to change the shape: now %s. Recomputed for that:",
		s.ContactName, ifStr(len(parts) > 0, strings.Join(parts, ", "), "a different setup"))

	// Zoom: we only do Google Meet. Say so plainly rather than quietly
	// sending a Meet link and hoping they don't notice.
	var extra []string
	if p := strings.ToLower(interp.RequestedPlatform); p != "" && p != "meet" && p != "google meet" {
		extra = append(extra, fmt.Sprintf("⚠️ They asked for %s. I can only create Google Meet links — I can't make a %s one. Either 'propose' and I'll send a Meet link instead, or paste me the %s link and I'll put it on the invite.",
			interp.RequestedPlatform, interp.RequestedPlatform, interp.RequestedPlatform))
	}
	if interp.NeedsVenue {
		extra = append(extra, "📍 Nobody's named a place yet — say 'edit' to write the message with one in.")
	}
	if interp.SideNote != "" {
		extra = append(extra, "Also: "+interp.SideNote)
	}
	if len(extra) > 0 {
		header += "\n\n" + strings.Join(extra, "\n")
	}
	m.resurfaceOptions(ctx, s, header)
}

// handleNotScheduling stands down. Quiet for "let's do it on email"; loud for
// "who is this?" — the user texted a stranger and needs to know.
func (m *Manager) handleNotScheduling(ctx context.Context, s *Session, dec Decision) {
	m.saveSession(s)
	if dec.Reason == "wrong_person" {
		m.sendSelfPlain(ctx, fmt.Sprintf("🛑 STOP — %s doesn't know who you are (\"who is this?\" / wrong number). You may have messaged the wrong person. I've dropped this session and sent nothing further. Check the chat.", s.ContactName))
		return
	}
	reason := "they're not settling this over WhatsApp"
	if dec.Interp != nil && dec.Interp.SideNote != "" {
		reason = dec.Interp.SideNote
	}
	m.sendSelfPlain(ctx, fmt.Sprintf("Dropped the %s session — %s.", s.ContactName, reason))
}

// OnContactMedia handles a message we cannot read (a voice note, a photo).
// extractText returns "" for these, so without this the session would sit in
// awaiting_reply looking broken while the counterpart thinks they replied.
func (m *Manager) OnContactMedia(ctx context.Context, chatJID string, isFromMe bool, kind MediaKind, ts time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if isFromMe {
		// Our own media isn't a reply to interpret.
		return
	}
	s := m.openSessionFor(chatJID)
	if s == nil {
		return
	}
	dec := HandleMediaMessage(s, kind, ts)
	if dec.Action != ActSurfaceMedia {
		m.saveSession(s)
		return
	}
	m.prompt(ctx, s, fmt.Sprintf("%s replied with a %s — I can't read that. Tell me what they said and I'll take it from there, or say 'leave it' and handle this one yourself.",
		s.ContactName, dec.Reason))
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
