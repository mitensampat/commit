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
}

// Session is the full engine-level state, serialized to the store as JSON.
type Session struct {
	ID          string        `json:"id"`
	ContactJID  string        `json:"contact_jid"`
	ContactName string        `json:"contact_name"`
	State       State         `json:"state"`
	Intent      SessionIntent `json:"intent"`

	Topic      string `json:"topic"`
	DurationMin int   `json:"duration_min"`
	Format     string `json:"format"` // "call", "video", "in-person"
	Window     string `json:"window"` // freeform, e.g. "this week"
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
)

// ReplyIntent is the interpreter's classification of the latest thread state.
type ReplyIntent string

const (
	ReplyAccept        ReplyIntent = "accept"
	ReplyReject        ReplyIntent = "reject"
	ReplyCounter       ReplyIntent = "counter_propose"
	ReplyAmbiguous     ReplyIntent = "ambiguous"
	ReplyUnrelated     ReplyIntent = "unrelated"
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
}

// SameOutcome reports whether two interpretations would book the same thing.
func (a *Interpretation) SameOutcome(b *Interpretation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Intent == b.Intent && a.SlotIndex == b.SlotIndex && a.CounterTime == b.CounterTime
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
