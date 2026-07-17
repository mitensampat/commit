package schedule

import (
	"strconv"
	"strings"
	"time"
)

// Action is what the wiring layer must do next. The engine never performs
// side effects itself.
type Action string

const (
	ActNone Action = "none"
	// ActAsk — send Decision.Reply to the self-chat and wait.
	ActAsk Action = "ask"
	// ActPickContact — user answered a disambiguation prompt; Decision.Index
	// is the 1-based choice.
	ActPickContact Action = "pick_contact"
	// ActPropose — send the draft to the counterpart. Decision.Indices is the
	// subset of slots to include (empty = all).
	ActPropose Action = "propose"
	// ActAlreadyProposed — duplicate propose within the dedup window.
	ActAlreadyProposed Action = "already_proposed"
	// ActRequestBooking — user consented ("yes"); wiring must re-read the
	// thread, re-verify the slot, then call DecideBooking.
	ActRequestBooking Action = "request_booking"
	// ActReplaceDraft — Decision.Text is the new draft.
	ActReplaceDraft Action = "replace_draft"
	// ActEditPrompt — ask the user for the new draft text.
	ActEditPrompt Action = "edit_prompt"
	// ActClose — end the session quietly (leave it). Decision.Reason set.
	ActClose Action = "close"
	// ActCancelMeeting — delete the booked event and send a graceful note.
	ActCancelMeeting Action = "cancel_meeting"
	// ActCancelSilent — delete the booked event, send nothing.
	ActCancelSilent Action = "cancel_silent"
	// ActSurfaceReply — show the counterpart's reply in the self-chat with a
	// yes / edit / leave it prompt. Decision.Interp attached.
	ActSurfaceReply Action = "surface_reply"
	// ActSurfaceChange — correction race: the thread changed since the user
	// was prompted; show the change instead of booking.
	ActSurfaceChange Action = "surface_change"
	// ActStandDown — the user resolved it manually in the counterpart chat;
	// close without prompting.
	ActStandDown Action = "stand_down"
	// ActBook — verified and consented: create the event and confirm.
	ActBook Action = "book"
	// ActSlotTaken — the slot filled up between proposal and yes.
	ActSlotTaken Action = "slot_taken"
	// ActVerifyCounter — counterpart proposed an unoffered time; wiring must
	// check it against the calendar and surface it as a pickable option.
	ActVerifyCounter Action = "verify_counter"
	// ActExpire — 48h of silence; close silently.
	ActExpire Action = "expire"
	// ActClassifyText — scoped self-chat free text over a pending draft.
	// Wiring must classify it (instruction | draft | note | unclear) and call
	// ApplySelfText. The engine never arms free text as a draft on its own.
	ActClassifyText Action = "classify_text"
	// ActHold — soft yes. Tell the user which slot it points at, book
	// nothing, keep watching. Decision.Index is the slot it points at.
	ActHold Action = "hold"
	// ActSurfacePick — they deferred the choice to us, so we made it.
	// Decision.Index is our pick, Decision.Reason says why. Wiring verifies
	// it's free and shows it for a 'yes'. It does NOT book: nothing reaches
	// the counterpart without the user's word.
	ActSurfacePick Action = "surface_pick"
	// ActScopeChange — the meeting's shape changed. Wiring must recompute
	// slots at the new duration/format, redraft, and re-surface for 'propose'.
	ActScopeChange Action = "scope_change"
	// ActNotScheduling — it stopped being scheduling. Close with a one-line
	// reason. Decision.Reason is "wrong_person" (loud) or "not_scheduling".
	ActNotScheduling Action = "not_scheduling"
	// ActSlotPast — the picked slot's start is in the past (they replied
	// days late). Never book it; recompute fresh options.
	ActSlotPast Action = "slot_past"
	// ActSurfaceMedia — a message we can't read arrived while awaiting a
	// reply. Decision.Reason is the MediaKind.
	ActSurfaceMedia Action = "surface_media"
	// ActApplyInstruction — the user told Commit what to change. The session
	// is already mutated; Decision.Class says whether a recompute is needed.
	ActApplyInstruction Action = "apply_instruction"
)

// Decision is the engine's output. The engine mutates the session in place
// (state transitions) and returns what the wiring should do.
type Decision struct {
	Action  Action
	Reply   string          // suggested self-chat text (wiring may re-render)
	Index   int             // 1-based slot or contact index, when relevant
	Indices []int           // propose subset
	Text    string          // replacement draft text
	Interp  *Interpretation // for surface actions
	Class   *SelfTextClass  // for ActApplyInstruction
	Reason  string
}

// SelfChatInput is a message the user typed in their self-chat, with the
// scoping facts the engine needs (hardening req 3).
type SelfChatInput struct {
	Text string
	Now  time.Time
	// IsNextAfterPrompt — this is the first self-chat message after Commit's
	// last prompt (nothing else, from anyone, in between).
	IsNextAfterPrompt bool
	// ForceScoped — the text carried the @schedule prefix, which always counts.
	ForceScoped bool
}

// consentCmd is a parsed consent word.
type consentCmd struct {
	kind    string // "propose", "yes", "yes_silent", "edit", "leave_it"
	indices []int  // propose 1 3 / yes 2
}

// parseConsent recognizes the consent vocabulary. Anything else returns ok=false.
func parseConsent(text string) (consentCmd, bool) {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.TrimSuffix(t, ".")
	fields := strings.Fields(t)
	if len(fields) == 0 {
		return consentCmd{}, false
	}
	switch fields[0] {
	case "propose":
		cmd := consentCmd{kind: "propose"}
		for _, f := range fields[1:] {
			n, err := strconv.Atoi(f)
			if err != nil {
				return consentCmd{}, false
			}
			cmd.indices = append(cmd.indices, n)
		}
		return cmd, true
	case "yes":
		if len(fields) == 2 && fields[1] == "silent" {
			return consentCmd{kind: "yes_silent"}, true
		}
		cmd := consentCmd{kind: "yes"}
		if len(fields) == 2 {
			if n, err := strconv.Atoi(fields[1]); err == nil {
				cmd.indices = []int{n}
				return cmd, true
			}
			return consentCmd{}, false
		}
		if len(fields) > 2 {
			return consentCmd{}, false
		}
		return cmd, true
	case "edit":
		if len(fields) == 1 {
			return consentCmd{kind: "edit"}, true
		}
		return consentCmd{}, false
	case "leave":
		if len(fields) == 2 && fields[1] == "it" {
			return consentCmd{kind: "leave_it"}, true
		}
		return consentCmd{}, false
	}
	return consentCmd{}, false
}

// scoped reports whether a self-chat message may carry consent semantics
// (hardening req 3): it must be the next message after Commit's prompt, or
// arrive within ConsentWindow of it, or carry the @schedule prefix.
func (s *Session) scoped(in SelfChatInput) bool {
	if in.ForceScoped || in.IsNextAfterPrompt {
		return true
	}
	if s.LastPromptAt.IsZero() {
		return false
	}
	return in.Now.Sub(s.LastPromptAt) <= ConsentWindow && in.Now.After(s.LastPromptAt)
}

// HandleSelfChat processes a self-chat message against an open session.
// ActNone means "this was a personal note — pretend we never saw it".
func HandleSelfChat(s *Session, in SelfChatInput) Decision {
	if s == nil || s.State == StateClosed {
		return Decision{Action: ActNone}
	}
	if !s.scoped(in) {
		// The self-chat doubles as a notepad. Out-of-scope text — including a
		// stray "yes" — must never move a session (hardening req 3).
		return Decision{Action: ActNone}
	}

	text := strings.TrimSpace(in.Text)

	// Contact disambiguation replies: a bare number or letter.
	if s.State == StateResolving {
		if n := parsePick(text); n > 0 {
			s.touch(in.Now)
			return Decision{Action: ActPickContact, Index: n}
		}
		return Decision{Action: ActAsk, Reply: "Reply with the number of the person you meant, or 'leave it' to drop this."}
	}

	cmd, isConsent := parseConsent(text)
	if !isConsent {
		// After an explicit "edit" we ASKED for draft text, so the next
		// message is unambiguously the draft — no classification needed.
		if s.AwaitingDraftEdit {
			s.Draft = text
			s.AwaitingDraftEdit = false
			s.touch(in.Now)
			return Decision{Action: ActReplaceDraft, Text: text,
				Reply: "Draft updated. It'll go out on your next 'propose' or 'yes'."}
		}
		// Unprompted free text over a pending draft is the foot-gun: silently
		// arming "he asked for Tue or Wed, our proposal is wrong" as the
		// message to SEND him is one 'propose' from disaster. Hand it to the
		// classifier instead — the engine stays pure and decides nothing here.
		if s.State == StateSlotsProposed || s.State == StateReplySurfaced || s.State == StateHeld {
			s.touch(in.Now)
			return Decision{Action: ActClassifyText, Text: text}
		}
		return Decision{Action: ActNone}
	}

	switch cmd.kind {
	case "leave_it":
		s.close(in.Now, "leave_it")
		return Decision{Action: ActClose, Reason: "leave_it", Reply: "Left it. Session closed."}

	case "edit":
		s.AwaitingDraftEdit = true
		s.touch(in.Now)
		return Decision{Action: ActEditPrompt, Reply: "Send me the new message text and I'll use that instead."}

	case "propose":
		if s.State != StateSlotsProposed && s.State != StateAwaitingReply && s.State != StateReplySurfaced {
			return Decision{Action: ActAsk, Reply: "Nothing to propose yet."}
		}
		if len(s.Slots) == 0 {
			return Decision{Action: ActAsk, Reply: "No slots on the table yet."}
		}
		// Idempotent propose (hardening req 6).
		if !s.ProposedAt.IsZero() && in.Now.Sub(s.ProposedAt) < ProposeDedupWindow && sameInts(cmd.indices, s.ProposedSlots) {
			return Decision{Action: ActAlreadyProposed, Reply: "Already sent that a moment ago — not sending it twice."}
		}
		for _, idx := range cmd.indices {
			if idx < 1 || idx > len(s.Slots) {
				return Decision{Action: ActAsk, Reply: "I only have " + strconv.Itoa(len(s.Slots)) + " slots — pick from those."}
			}
		}
		s.ProposedAt = in.Now
		s.ProposedSlots = cmd.indices
		s.State = StateAwaitingReply
		s.touch(in.Now)
		return Decision{Action: ActPropose, Indices: cmd.indices}

	case "yes":
		switch s.State {
		case StateConfirmCancel:
			s.touch(in.Now)
			return Decision{Action: ActCancelMeeting}
		case StateReplySurfaced:
			s.touch(in.Now)
			// Wiring must re-read the thread and re-verify before booking
			// (hardening req 2). DecideBooking makes the final call.
			return Decision{Action: ActRequestBooking, Index: cmd.firstIndex()}
		case StateHeld:
			// The user saw "this is a soft yes" and said book it anyway.
			// That's their call to make — treat it as an explicit pick of the
			// held slot so the hedge doesn't gate their own decision.
			idx := cmd.firstIndex()
			if idx == 0 && s.Surfaced != nil {
				idx = s.Surfaced.SlotIndex
			}
			if idx < 1 || idx > len(s.Slots) {
				return Decision{Action: ActAsk, Reply: "Which slot? Reply 'yes N'."}
			}
			s.touch(in.Now)
			return Decision{Action: ActRequestBooking, Index: idx}
		case StateSlotsProposed:
			// Direct booking without a proposal round — only with an explicit
			// slot number; a bare "yes" over three options is not consent to
			// any particular one.
			if idx := cmd.firstIndex(); idx > 0 {
				if idx > len(s.Slots) {
					return Decision{Action: ActAsk, Reply: "I only have " + strconv.Itoa(len(s.Slots)) + " slots — pick from those."}
				}
				s.touch(in.Now)
				return Decision{Action: ActRequestBooking, Index: idx}
			}
			return Decision{Action: ActAsk, Reply: "Which slot? Reply 'yes 2' to book one directly, or 'propose' to send the options to " + s.ContactName + " first."}
		case StateAwaitingReply:
			return Decision{Action: ActAsk, Reply: "Still waiting on " + s.ContactName + " — I'll ping you here when they reply."}
		}
		return Decision{Action: ActNone}

	case "yes_silent":
		if s.State == StateConfirmCancel {
			s.touch(in.Now)
			return Decision{Action: ActCancelSilent}
		}
		return Decision{Action: ActAsk, Reply: "'yes silent' only applies to a cancel."}
	}

	return Decision{Action: ActNone}
}

// watching reports whether the watcher is live for this session.
func (s *Session) watching() bool {
	return s.State == StateAwaitingReply || s.State == StateReplySurfaced || s.State == StateHeld
}

// slotPassed reports whether a slot's start has already gone by. Booking or
// offering a slot from last Tuesday is never right, however clearly the
// counterpart picked it.
func slotPassed(sl Slot, now time.Time) bool { return !sl.Start.After(now) }

// PickDeferredSlot chooses on the user's behalf when the counterpart defers
// ("you pick"). subset (1-based) narrows the candidates when they deferred
// within one ("Tue or Wed, you choose"). The pick is the earliest slot that
// doesn't fragment the day: adjacent slots butt against an existing meeting
// and keep large free blocks intact, so they win outright; otherwise the
// earliest. Past slots are never candidates. Returns a 1-based index and the
// one-clause reason, or 0 when nothing is left.
func PickDeferredSlot(slots []Slot, subset []int, now time.Time) (int, string) {
	allowed := map[int]bool{}
	for _, i := range subset {
		if i >= 1 && i <= len(slots) {
			allowed[i] = true
		}
	}
	best, bestAdj := 0, false
	for i, sl := range slots {
		idx := i + 1
		if len(allowed) > 0 && !allowed[idx] {
			continue
		}
		if slotPassed(sl, now) {
			continue
		}
		switch {
		case best == 0:
			best, bestAdj = idx, sl.Adjacent
		case sl.Adjacent && !bestAdj:
			// An adjacent slot beats an earlier non-adjacent one.
			best, bestAdj = idx, true
		case sl.Adjacent == bestAdj && sl.Start.Before(slots[best-1].Start):
			best = idx
		}
	}
	if best == 0 {
		return 0, ""
	}
	if bestAdj {
		return best, "it sits right next to what you already have that day"
	}
	return best, "it's the earliest of the ones on the table"
}

// HandleCounterpartReply processes a fresh interpretation of the counterpart
// thread while the watcher is active. msgTime is the newest counterpart
// message timestamp the interpretation covers.
func HandleCounterpartReply(s *Session, interp *Interpretation, msgTime, now time.Time) Decision {
	if s == nil || !s.watching() {
		return Decision{Action: ActNone}
	}
	if interp == nil {
		return Decision{Action: ActNone}
	}

	switch interp.Intent {
	case ReplyUnrelated:
		// Keep watching; don't extend the session's life over small talk.
		return Decision{Action: ActNone}

	case ReplyNotScheduling:
		// It stopped being scheduling. Say so once, close cleanly.
		reason := "not_scheduling"
		if interp.WrongPerson {
			reason = "wrong_person"
		}
		s.Surfaced = interp
		s.SurfacedAtMsgTime = msgTime
		s.close(now, reason)
		return Decision{Action: ActNotScheduling, Interp: interp, Reason: reason}

	case ReplySoftYes:
		// SAFETY-CRITICAL: a hedge is not consent. Nothing books off this.
		if interp.SlotIndex < 1 || interp.SlotIndex > len(s.Slots) {
			// Hedged at nothing in particular — that's plain ambiguity.
			return surfaceReply(s, &Interpretation{Intent: ReplyAmbiguous, SideNote: interp.SideNote, Confidence: "low"}, msgTime, now)
		}
		if slotPassed(s.Slots[interp.SlotIndex-1], now) {
			return Decision{Action: ActSlotPast, Index: interp.SlotIndex, Interp: interp, Reason: "held_slot_passed"}
		}
		s.Surfaced = interp
		s.SurfacedAtMsgTime = msgTime
		s.State = StateHeld
		s.touch(now)
		return Decision{Action: ActHold, Index: interp.SlotIndex, Interp: interp}

	case ReplyDeference:
		// They handed the choice back, so we make it — asking "which one?"
		// after the counterpart already declined to choose is the fussiness
		// worth removing. But making the pick is not permission to send it:
		// nothing reaches the counterpart without the user's word, so we
		// surface the pick and wait for the same one 'yes' every other path
		// costs.
		idx, why := PickDeferredSlot(s.Slots, interp.DeferSlots, now)
		if idx == 0 {
			return Decision{Action: ActSlotPast, Interp: interp, Reason: "all_slots_passed"}
		}
		s.Surfaced = interp
		s.SurfacedAtMsgTime = msgTime
		s.PickedIndex = idx
		s.State = StateReplySurfaced
		s.touch(now)
		return Decision{Action: ActSurfacePick, Index: idx, Reason: why, Interp: interp}

	case ReplyScopeChange:
		// The shape changed, not the time. Slots computed for 30 minutes may
		// not hold an hour, and in-person needs the travel buffer — so the
		// wiring must recompute rather than reuse.
		if interp.NewDurationMin > 0 {
			s.DurationMin = interp.NewDurationMin
		}
		if interp.NewFormat != "" {
			s.Format = interp.NewFormat
		}
		s.Surfaced = interp
		s.SurfacedAtMsgTime = msgTime
		s.State = StateSlotsProposed
		s.touch(now)
		return Decision{Action: ActScopeChange, Interp: interp}

	case ReplyDirective:
		// They told us when, they didn't ask. Sending three options back at
		// someone who said "call me at 5" is a move a human would never make.
		if interp.CounterTime == "" {
			return surfaceReply(s, &Interpretation{Intent: ReplyAmbiguous, SideNote: interp.SideNote, Confidence: "low"}, msgTime, now)
		}
		s.Surfaced = interp
		s.SurfacedAtMsgTime = msgTime
		s.State = StateReplySurfaced
		s.touch(now)
		return Decision{Action: ActVerifyCounter, Interp: interp}

	case ReplyCounter:
		if interp.CounterTime == "" {
			// A counter with no concrete time is just ambiguity.
			return surfaceReply(s, &Interpretation{Intent: ReplyAmbiguous, SideNote: interp.SideNote, Confidence: interp.Confidence}, msgTime, now)
		}
		s.Surfaced = interp
		s.SurfacedAtMsgTime = msgTime
		s.State = StateReplySurfaced
		s.touch(now)
		// Wiring verifies the time against the calendar, appends it as a
		// pickable option, and prompts (hardening req 10).
		return Decision{Action: ActVerifyCounter, Interp: interp}

	case ReplyAccept:
		if interp.SlotIndex < 1 || interp.SlotIndex > len(s.Slots) {
			// "Sounds good" over several options — which one? Never guess.
			return surfaceReply(s, &Interpretation{Intent: ReplyAmbiguous, SideNote: interp.SideNote, Confidence: "low"}, msgTime, now)
		}
		// They replied three days late and picked last Tuesday.
		if slotPassed(s.Slots[interp.SlotIndex-1], now) {
			return Decision{Action: ActSlotPast, Index: interp.SlotIndex, Interp: interp, Reason: "accepted_slot_passed"}
		}
		return surfaceReply(s, interp, msgTime, now)

	case ReplyReject, ReplyAmbiguous:
		return surfaceReply(s, interp, msgTime, now)
	}
	return Decision{Action: ActNone}
}

func surfaceReply(s *Session, interp *Interpretation, msgTime, now time.Time) Decision {
	s.Surfaced = interp
	s.SurfacedAtMsgTime = msgTime
	s.State = StateReplySurfaced
	s.touch(now)
	return Decision{Action: ActSurfaceReply, Interp: interp}
}

// HandleOwnMessage — the user themselves texted in the counterpart chat
// mid-session. If they finalized a time on their own, stand down silently
// (hardening req 9).
func HandleOwnMessage(s *Session, finalizedManually bool, now time.Time) Decision {
	if s == nil || !s.watching() {
		return Decision{Action: ActNone}
	}
	if !finalizedManually {
		return Decision{Action: ActNone}
	}
	s.close(now, "manual_resolution")
	return Decision{Action: ActStandDown}
}

// DecideBooking makes the final call after the user's "yes": fresh is a
// just-computed interpretation of the LATEST thread state (nil if there was
// never a proposal round), slotFree is the just-re-verified calendar check.
func DecideBooking(s *Session, fresh *Interpretation, slotFree bool, now time.Time) Decision {
	if s == nil || s.State == StateClosed {
		return Decision{Action: ActNone}
	}

	// Correction race (hardening req 2): if the counterpart changed their
	// answer after we pinged the user, booking on the stale answer is the
	// worst failure mode. Surface the change instead.
	if s.Surfaced != nil {
		if fresh == nil {
			return Decision{Action: ActSurfaceChange, Reason: "could_not_reverify",
				Reply: "I couldn't re-read the thread to confirm nothing changed — not booking. Take a look and say 'yes' again."}
		}
		if !fresh.SameOutcome(s.Surfaced) {
			s.Surfaced = fresh
			s.touch(now)
			return Decision{Action: ActSurfaceChange, Interp: fresh, Reason: "thread_changed"}
		}
		// Which readings are a mandate to book, once the user has said yes?
		// An acceptance and a counter-proposal name a time themselves.
		// Deference names none — but "you pick" IS a clear answer, and the
		// user consented to the concrete pick we showed them, so it books
		// through this same verified path rather than around it.
		if fresh.Confidence == "low" || (fresh.Intent != ReplyAccept && fresh.Intent != ReplyCounter && fresh.Intent != ReplyDeference) {
			return Decision{Action: ActSurfaceChange, Interp: fresh, Reason: "not_a_clear_yes"}
		}
	}

	// The passage of time: a slot that was fine when we proposed it may have
	// simply gone by. Booking into the past is never right.
	if start, ok := s.targetStart(); ok && !start.After(now) {
		s.touch(now)
		return Decision{Action: ActSlotPast, Reason: "target_passed"}
	}

	if !slotFree {
		s.touch(now)
		return Decision{Action: ActSlotTaken,
			Reply: "That slot got taken on your calendar since I proposed it. Want fresh options?"}
	}

	s.touch(now)
	return Decision{Action: ActBook}
}

// targetStart resolves the start time the surfaced outcome would book, so the
// past-slot guard can run without duplicating bookingTarget's logic.
func (s *Session) targetStart() (time.Time, bool) {
	if s.Surfaced == nil {
		return time.Time{}, false
	}
	switch s.Surfaced.Intent {
	case ReplyAccept, ReplySoftYes:
		if i := s.Surfaced.SlotIndex; i >= 1 && i <= len(s.Slots) {
			return s.Slots[i-1].Start, true
		}
	case ReplyDeference:
		// The counterpart named no slot — the pick is ours, recorded when we
		// surfaced it.
		if i := s.PickedIndex; i >= 1 && i <= len(s.Slots) {
			return s.Slots[i-1].Start, true
		}
	case ReplyCounter, ReplyDirective:
		if t, err := time.Parse(time.RFC3339, s.Surfaced.CounterTime); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ApplySelfText resolves the user's classified free text. Safety contract:
// anything short of a confident "this is a draft" must never become the
// outbound message.
func ApplySelfText(s *Session, text string, class *SelfTextClass, now time.Time) Decision {
	if s == nil || s.State == StateClosed {
		return Decision{Action: ActNone}
	}
	// A personal note is none of our business. The self-chat is also the
	// user's notepad — "buy milk" must not be armed as a draft, and must not
	// be asked about either.
	if class != nil && class.Kind == SelfTextNote && class.Confidence == "high" {
		return Decision{Action: ActNone, Reason: "personal_note"}
	}
	// Unclear, unconfident, or unclassifiable → ask. Never arm.
	if class == nil || class.Confidence != "high" ||
		(class.Kind != SelfTextDraft && class.Kind != SelfTextInstruction) {
		s.touch(now)
		return Decision{Action: ActAsk, Reason: "unclear_self_text",
			Reply: "Not sure if that's a note for me or the message you want sent. Say 'edit' first if it's the message, or tell me what to change."}
	}

	if class.Kind == SelfTextDraft {
		s.Draft = text
		s.AwaitingDraftEdit = false
		s.touch(now)
		return Decision{Action: ActReplaceDraft, Text: text,
			Reply: "Draft updated. It'll go out on your next 'propose' or 'yes'."}
	}

	// Instruction: act on it.
	if class.Window != "" {
		s.Window = class.Window
	}
	if class.DurationMin > 0 {
		s.DurationMin = class.DurationMin
	}
	if class.Format != "" {
		s.Format = class.Format
	}
	if class.ToneNote != "" {
		s.ToneNote = class.ToneNote
	}
	s.touch(now)
	return Decision{Action: ActApplyInstruction, Class: class, Text: text}
}

// HandleMediaMessage — the counterpart replied with something we can't read
// (a voice note, a photo). Going silent makes the session look broken, so we
// say so; but five photos must not produce five nudges.
func HandleMediaMessage(s *Session, kind MediaKind, now time.Time) Decision {
	if s == nil || s.State != StateAwaitingReply {
		return Decision{Action: ActNone}
	}
	if !s.LastMediaSurfacedAt.IsZero() && now.Sub(s.LastMediaSurfacedAt) < MediaBurstWindow {
		return Decision{Action: ActNone}
	}
	s.LastMediaSurfacedAt = now
	s.touch(now)
	return Decision{Action: ActSurfaceMedia, Reason: string(kind)}
}

// CheckExpiry closes sessions that have been silent too long.
func CheckExpiry(s *Session, now time.Time) Decision {
	if s == nil || s.State == StateClosed {
		return Decision{Action: ActNone}
	}
	if now.Sub(s.LastActivity) > SessionExpiry {
		s.close(now, "expired")
		return Decision{Action: ActExpire}
	}
	return Decision{Action: ActNone}
}

// MarkPrompted records that Commit just prompted the user in the self-chat —
// this opens the consent window.
func (s *Session) MarkPrompted(now time.Time) {
	s.LastPromptAt = now
	s.touch(now)
}

func (s *Session) touch(now time.Time) { s.LastActivity = now }

func (s *Session) close(now time.Time, reason string) {
	s.State = StateClosed
	s.touch(now)
}

func (c consentCmd) firstIndex() int {
	if len(c.indices) > 0 {
		return c.indices[0]
	}
	return 0
}

func parsePick(text string) int {
	t := strings.ToLower(strings.TrimSpace(text))
	if n, err := strconv.Atoi(t); err == nil && n > 0 && n < 100 {
		return n
	}
	if len(t) == 1 && t[0] >= 'a' && t[0] <= 'z' {
		return int(t[0]-'a') + 1
	}
	return 0
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
