// Package schedule implements the @schedule feature: a self-chat driven
// meeting scheduler with an explicit consent contract.
//
// The core is a pure state machine (engine.go) plus an LLM reply interpreter
// behind an interface (interpreter.go), so the whole session lifecycle is
// testable without WhatsApp, Google Calendar, or the Claude API.
package schedule

import (
	"context"
	"time"
)

// State is the lifecycle position of a scheduling session.
type State string

const (
	// StateResolving — contact match was ambiguous; waiting for the user to pick.
	StateResolving State = "resolving"
	// StateSlotsProposed — slots + draft shown in self-chat; waiting for
	// propose / edit / leave it.
	StateSlotsProposed State = "slots_proposed"
	// StateAwaitingReply — draft sent to the counterpart; watcher active.
	StateAwaitingReply State = "awaiting_reply"
	// StateReplySurfaced — counterpart reply surfaced in self-chat with a
	// yes / edit / leave it prompt.
	StateReplySurfaced State = "reply_surfaced"
	// StateHeld — the counterpart gave a SOFT yes ("Tue works, let me confirm
	// tomorrow"). Nothing is booked and nothing may be booked off the soft yes
	// alone; the watcher stays live waiting for them to firm up. The user can
	// still force it with an explicit "yes".
	StateHeld State = "held"
	// StateConfirmCancel — @schedule cancel asked "yes / yes silent / leave it".
	StateConfirmCancel State = "confirm_cancel"
	StateClosed        State = "closed"
)

// SessionIntent is what the user asked for.
type SessionIntent string

const (
	IntentSchedule SessionIntent = "schedule"
	IntentMove     SessionIntent = "move"
	IntentCancel   SessionIntent = "cancel"
)

// Slot is a concrete proposable meeting time in the user's timezone.
type Slot struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Origin marks where the option came from: "computed" (our calendar) or
	// "counterpart" (they proposed it and we verified it free).
	Origin string `json:"origin,omitempty"`
	// Adjacent marks a slot that butts up against an existing meeting. Such a
	// slot keeps large free blocks intact, so it is the one to pick when the
	// counterpart defers the choice to us.
	Adjacent bool `json:"adjacent,omitempty"`
}

// Session is the full engine-level state, serialized to the store as JSON.
type Session struct {
	ID          string        `json:"id"`
	ContactJID  string        `json:"contact_jid"`
	ContactName string        `json:"contact_name"`
	State       State         `json:"state"`
	Intent      SessionIntent `json:"intent"`

	Topic       string `json:"topic"`
	DurationMin int    `json:"duration_min"`
	Format      string `json:"format"` // "call", "video", "in-person"
	Window      string `json:"window"` // freeform, e.g. "this week"
	// RequestedDays are the weekdays the counterpart asked for ("Tue/Wed");
	// PreferenceMet is false when nothing was free on them and we're
	// proposing other days instead.
	RequestedDays string `json:"requested_days,omitempty"`
	PreferenceMet bool   `json:"preference_met"`

	Slots []Slot `json:"slots"` // currently pickable options, 1-based in user-facing text
	Draft string `json:"draft"` // message to send to the counterpart
	// SentDraft is the draft as actually sent to the counterpart. Thread
	// interpretation must use this, not Draft, which the user may edit later.
	SentDraft string `json:"sent_draft,omitempty"`

	ContactTZ     string `json:"contact_tz"`      // IANA name, best inference or override
	ContactTZNote string `json:"contact_tz_note"` // e.g. "assuming SF from +1 number"

	// Consent scoping: bare consent words only count as the next self-chat
	// message after our prompt, or within ConsentWindow of it.
	LastPromptAt time.Time `json:"last_prompt_at"`

	// Idempotent propose: a second propose within ProposeDedupWindow is a no-op.
	ProposedAt    time.Time `json:"proposed_at"`
	ProposedSlots []int     `json:"proposed_slots"` // 1-based indices actually sent

	// AwaitingDraftEdit — the user said "edit"; the next scoped self-chat
	// message replaces the draft.
	AwaitingDraftEdit bool `json:"awaiting_draft_edit"`

	// Surfaced is the interpretation we showed the user in the yes/edit/leave-it
	// prompt. At "yes" time the thread is re-read; if the fresh interpretation
	// differs materially, we surface the change instead of booking.
	Surfaced *Interpretation `json:"surfaced,omitempty"`
	// SurfacedAtMsgTime — timestamp of the newest counterpart message that
	// Surfaced was computed from.
	SurfacedAtMsgTime time.Time `json:"surfaced_at_msg_time"`

	// LastActivity drives silent expiry (48h).
	LastActivity time.Time `json:"last_activity"`

	// LastMediaSurfacedAt — when we last told the user "they replied with a
	// voice note I can't hear". Rate-limits one such nudge per inbound burst
	// so five photos don't produce five messages.
	LastMediaSurfacedAt time.Time `json:"last_media_surfaced_at,omitempty"`
	// ToneNote — a standing instruction about how the draft should read
	// ("make it warmer"), applied on every redraft for the session's life.
	ToneNote string `json:"tone_note,omitempty"`

	// BookedEventID is set after a successful booking (used by move/cancel).
	BookedEventID string    `json:"booked_event_id,omitempty"`
	BookedSlot    *Slot     `json:"booked_slot,omitempty"`
	CreatedAt     time.Time `json:"created_at"`

	// Candidates — pending contact disambiguation choices (StateResolving).
	Candidates []ContactCandidate `json:"candidates,omitempty"`
	// Cmd — the original parsed command, kept while resolving.
	Cmd *Command `json:"cmd,omitempty"`
	// LastPromptID — WhatsApp message ID of Commit's last self-chat prompt
	// (adjacency check for consent scoping).
	LastPromptID string `json:"last_prompt_id,omitempty"`
	// OldEventID — for @schedule move: the event to delete once rebooked.
	OldEventID string `json:"old_event_id,omitempty"`
}

// ContactCandidate is one fuzzy-match option during contact resolution.
type ContactCandidate struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

// Consent scoping and idempotency windows (hardening reqs 3 and 6).
const (
	ConsentWindow      = 2 * time.Hour
	ProposeDedupWindow = 5 * time.Minute
	SessionExpiry      = 48 * time.Hour
	// MediaBurstWindow — one "I can't read that" nudge per session per burst.
	MediaBurstWindow = 15 * time.Minute
)

// MediaKind names a non-text message we received but cannot read. WhatsApp
// replies are often voice notes; going silent on one makes the session look
// broken, so the watcher surfaces the fact rather than the content.
type MediaKind string

const (
	MediaVoice    MediaKind = "voice note"
	MediaAudio    MediaKind = "audio message"
	MediaImage    MediaKind = "photo"
	MediaVideo    MediaKind = "video"
	MediaDocument MediaKind = "document"
	MediaSticker  MediaKind = "sticker"
)

// SelfTextKind classifies the user's free text in their own self-chat while a
// draft is pending. The distinction is a foot-gun: an instruction silently
// armed as the outbound draft is one "propose" away from being sent to the
// counterpart.
type SelfTextKind string

const (
	// SelfTextInstruction — they are telling Commit what to change ("he asked
	// for Tue or Wed", "make it warmer", "actually 45 mins").
	SelfTextInstruction SelfTextKind = "instruction"
	// SelfTextDraft — a fully-written message addressed to the counterpart.
	SelfTextDraft SelfTextKind = "draft"
	// SelfTextNote — a personal note that has nothing to do with this session
	// ("buy milk, renew passport"). The self-chat doubles as a notepad, so
	// this must fall through in silence: not armed, and not asked about.
	SelfTextNote SelfTextKind = "note"
	// SelfTextUnclear — it IS about the scheduling, but we can't tell whether
	// it's an instruction or the message. Ask; never silently arm it.
	SelfTextUnclear SelfTextKind = "unclear"
)

// SelfTextClass is the classifier's structured reading of the user's message.
type SelfTextClass struct {
	Kind SelfTextKind `json:"kind"`
	// Window — a new window/day phrase to re-search ("Tue or Wed", "only next
	// week"). Fed straight to WindowRange + PreferredDays.
	Window string `json:"window,omitempty"`
	// DurationMin — a new duration, 0 if unchanged.
	DurationMin int `json:"duration_min,omitempty"`
	// Format — a new format ("call" | "video" | "in-person"), "" if unchanged.
	Format string `json:"format,omitempty"`
	// ToneNote — how the draft should read differently ("warmer", "shorter").
	ToneNote string `json:"tone_note,omitempty"`
	// Confidence — "high" or "low". Low degrades to unclear.
	Confidence string `json:"confidence"`
}

// NeedsRecompute reports whether an instruction changes the search itself
// (days/duration/format) rather than only the wording.
func (c *SelfTextClass) NeedsRecompute() bool {
	return c.Window != "" || c.DurationMin > 0 || c.Format != ""
}

// SelfTextContext is everything the self-text classifier sees.
type SelfTextContext struct {
	ContactName string
	Text        string
	Draft       string // the draft currently on the table
	Slots       []Slot
	Topic       string
	DurationMin int
	Format      string
	Now         time.Time
	Location    *time.Location
}

// SelfTextClassifier reads the user's own self-chat free text. Implemented by
// the LLM (interpreter.go) in production and by fakes in tests.
type SelfTextClassifier interface {
	ClassifySelfText(ctx context.Context, sc SelfTextContext) (*SelfTextClass, error)
}

// ReplyIntent is the interpreter's classification of the latest thread state.
type ReplyIntent string

const (
	ReplyAccept    ReplyIntent = "accept"
	ReplyReject    ReplyIntent = "reject"
	ReplyCounter   ReplyIntent = "counter_propose"
	ReplyAmbiguous ReplyIntent = "ambiguous"
	ReplyUnrelated ReplyIntent = "unrelated"

	// ReplySoftYes — a hedged acceptance ("Tue works, let me confirm
	// tomorrow", "probably Wed"). SAFETY-CRITICAL: never books. Points at a
	// slot via SlotIndex, holds the session, keeps watching.
	ReplySoftYes ReplyIntent = "soft_yes"
	// ReplyDeference — they hand the choice back ("you pick", "any of these
	// work"). We pick and book. DeferSlots narrows the subset when they
	// deferred within one ("Tue or Wed, you choose").
	ReplyDeference ReplyIntent = "deference"
	// ReplyScopeChange — the shape of the meeting changed, not the time
	// ("make it an hour", "coffee instead", "just call me"). Carries
	// NewDurationMin / NewFormat / NeedsVenue / RequestedPlatform.
	ReplyScopeChange ReplyIntent = "scope_change"
	// ReplyDirective — an instruction, not a negotiation ("call me at 5").
	// We check the stated time (CounterTime) and surface; we never send
	// options back at someone who just told us when to show up.
	ReplyDirective ReplyIntent = "directive"
	// ReplyNotScheduling — this stopped being a scheduling conversation
	// ("do it on email", "my assistant will set it up", "who is this?").
	// We stand down and close. WrongPerson makes the close loud.
	ReplyNotScheduling ReplyIntent = "not_scheduling"
)

// Interpretation is the structured reading of the counterpart thread. It
// always reflects the LATEST state: a correction after an acceptance must
// come back as the corrected intent, not the stale acceptance.
type Interpretation struct {
	Intent ReplyIntent `json:"intent"`
	// SlotIndex — 1-based index into the offered slots the counterpart
	// accepted; 0 if none / not applicable.
	SlotIndex int `json:"slot_index"`
	// CounterTime — RFC3339 in the user's timezone when the counterpart
	// proposed a concrete unoffered time; "" otherwise. Vague windows
	// ("early next week") are ambiguous, not counters.
	CounterTime string `json:"counter_time"`
	// SideNote — non-scheduling content worth relaying ("also send the deck").
	SideNote string `json:"side_note"`
	// Confidence — "high" or "low". The engine never books on low.
	Confidence string `json:"confidence"`

	// DeferSlots — for "deference": the 1-based subset they said they're fine
	// with ("Tue or Wed, you choose" → [1,2]). Empty means any offered slot.
	DeferSlots []int `json:"defer_slots,omitempty"`

	// NewDurationMin — for "scope_change": the duration they asked for, 0 if
	// unchanged.
	NewDurationMin int `json:"new_duration_min,omitempty"`
	// NewFormat — for "scope_change": "call" | "video" | "in-person" | "".
	NewFormat string `json:"new_format,omitempty"`
	// NeedsVenue — for "scope_change": the new shape needs a place decided
	// ("let's grab coffee") and nobody has named one.
	NeedsVenue bool `json:"needs_venue,omitempty"`
	// RequestedPlatform — for "scope_change": a video tool they named by name
	// ("zoom", "teams", "meet"). We only do Meet, so a mismatch has to be said
	// out loud rather than silently papered over.
	RequestedPlatform string `json:"requested_platform,omitempty"`

	// WrongPerson — for "not_scheduling": "who is this?" / "wrong number".
	// The user texted a stranger; the close must be loud.
	WrongPerson bool `json:"wrong_person,omitempty"`
}

// SameOutcome reports whether two interpretations would book the same thing.
// Scope fields participate: a 30→60 change and a 30→45 change are different
// outcomes, and treating them as equal would let a stale scope through the
// correction-race gate.
func (a *Interpretation) SameOutcome(b *Interpretation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Intent == b.Intent && a.SlotIndex == b.SlotIndex && a.CounterTime == b.CounterTime &&
		a.NewDurationMin == b.NewDurationMin && a.NewFormat == b.NewFormat
}

// ThreadMsg is one message in the counterpart chat, for interpretation.
type ThreadMsg struct {
	FromMe bool
	Text   string
	Time   time.Time
}

// ReplyContext is everything the interpreter sees.
type ReplyContext struct {
	ContactName string
	Slots       []Slot // options that were offered (1-based in the draft)
	Draft       string // the message we sent
	Thread      []ThreadMsg
	Now         time.Time
	Location    *time.Location // user's timezone
}

// ReplyInterpreter reads the counterpart thread. Implemented by the LLM
// (interpreter.go) in production and by fakes in tests.
type ReplyInterpreter interface {
	// InterpretReply classifies the latest scheduling state of the thread.
	InterpretReply(ctx context.Context, rc ReplyContext) (*Interpretation, error)
	// InterpretOwnMessage reports whether the user's own message in the
	// counterpart chat finalized a meeting time themselves (watcher must
	// stand down — hardening req 9).
	InterpretOwnMessage(ctx context.Context, rc ReplyContext) (bool, error)
}
