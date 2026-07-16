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
		// Scoped free text over a pending draft replaces the draft.
		if s.AwaitingDraftEdit || s.State == StateSlotsProposed || s.State == StateReplySurfaced {
			s.Draft = text
			s.AwaitingDraftEdit = false
			s.touch(in.Now)
			return Decision{Action: ActReplaceDraft, Text: text,
				Reply: "Draft updated. It'll go out on your next 'propose' or 'yes'."}
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

// HandleCounterpartReply processes a fresh interpretation of the counterpart
// thread while the watcher is active. msgTime is the newest counterpart
// message timestamp the interpretation covers.
func HandleCounterpartReply(s *Session, interp *Interpretation, msgTime, now time.Time) Decision {
	if s == nil || (s.State != StateAwaitingReply && s.State != StateReplySurfaced) {
		return Decision{Action: ActNone}
	}
	if interp == nil {
		return Decision{Action: ActNone}
	}

	switch interp.Intent {
	case ReplyUnrelated:
		// Keep watching; don't extend the session's life over small talk.
		return Decision{Action: ActNone}

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
	if s == nil || (s.State != StateAwaitingReply && s.State != StateReplySurfaced) {
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
		if fresh.Confidence == "low" || (fresh.Intent != ReplyAccept && fresh.Intent != ReplyCounter) {
			return Decision{Action: ActSurfaceChange, Interp: fresh, Reason: "not_a_clear_yes"}
		}
	}

	if !slotFree {
		s.touch(now)
		return Decision{Action: ActSlotTaken,
			Reply: "That slot got taken on your calendar since I proposed it. Want fresh options?"}
	}

	s.touch(now)
	return Decision{Action: ActBook}
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
