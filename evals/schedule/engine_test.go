// Deterministic scenario evals for the @schedule session engine. These run in
// CI with no network: the state machine is pure and the LLM interpreter is
// faked. Every safety-critical behavior (consent scoping, idempotent propose,
// correction race, manual-resolution stand-down, never-book-on-ambiguity)
// must pass at 100%.
package scheduleevals

import (
	"testing"
	"time"

	"github.com/msfoundry/commit/schedule"
)

var t0 = time.Date(2026, 7, 20, 10, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))

func baseSession(state schedule.State) *schedule.Session {
	s := &schedule.Session{
		ID:          "ss_test",
		ContactJID:  "919812345678@s.whatsapp.net",
		ContactName: "Akshay Shah",
		State:       state,
		Intent:      schedule.IntentSchedule,
		Topic:       "partnership follow-up",
		DurationMin: 30,
		Slots: []schedule.Slot{
			{Start: t0.Add(29 * time.Hour), End: t0.Add(29*time.Hour + 30*time.Minute)}, // Tue 3pm
			{Start: t0.Add(49 * time.Hour), End: t0.Add(49*time.Hour + 30*time.Minute)}, // Wed 11am
			{Start: t0.Add(78 * time.Hour), End: t0.Add(78*time.Hour + 30*time.Minute)}, // Thu 4pm
		},
		Draft:        "hey! free any of these? 1. Tue 3pm 2. Wed 11am 3. Thu 4pm",
		LastPromptAt: t0,
		LastActivity: t0,
		CreatedAt:    t0,
	}
	return s
}

func self(text string, at time.Time, nextAfterPrompt bool) schedule.SelfChatInput {
	return schedule.SelfChatInput{Text: text, Now: at, IsNextAfterPrompt: nextAfterPrompt}
}

// ── Consent-word scoping (hardening req 3) — SAFETY CRITICAL ──

func TestScoping_StrayYesHoursLaterIsIgnored(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	// 3h after the prompt, and NOT the next message after it: personal note.
	d := schedule.HandleSelfChat(s, self("yes", t0.Add(3*time.Hour), false))
	if d.Action != schedule.ActNone {
		t.Fatalf("stray 'yes' must never book: got %s", d.Action)
	}
}

func TestScoping_YesAsNextMessageCounts(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	d := schedule.HandleSelfChat(s, self("yes", t0.Add(26*time.Hour), true))
	if d.Action != schedule.ActRequestBooking {
		t.Fatalf("next-after-prompt 'yes' should request booking: got %s", d.Action)
	}
}

func TestScoping_YesWithinTwoHoursCounts(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	d := schedule.HandleSelfChat(s, self("yes", t0.Add(90*time.Minute), false))
	if d.Action != schedule.ActRequestBooking {
		t.Fatalf("'yes' within 2h of prompt should count: got %s", d.Action)
	}
}

func TestScoping_PrefixedYesAlwaysCounts(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	in := schedule.SelfChatInput{Text: "yes", Now: t0.Add(30 * time.Hour), ForceScoped: true}
	if d := schedule.HandleSelfChat(s, in); d.Action != schedule.ActRequestBooking {
		t.Fatalf("@schedule-prefixed yes must always count: got %s", d.Action)
	}
}

func TestScoping_StrayProposeIgnored(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	d := schedule.HandleSelfChat(s, self("propose", t0.Add(5*time.Hour), false))
	if d.Action != schedule.ActNone {
		t.Fatalf("stray 'propose' must be ignored: got %s", d.Action)
	}
	if s.State != schedule.StateSlotsProposed {
		t.Fatalf("state must not move on out-of-scope input")
	}
}

func TestScoping_PersonalNoteDoesNotClobberDraft(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	orig := s.Draft
	d := schedule.HandleSelfChat(s, self("buy milk, call mom", t0.Add(4*time.Hour), false))
	if d.Action != schedule.ActNone || s.Draft != orig {
		t.Fatalf("out-of-scope note must not replace draft: action=%s draft=%q", d.Action, s.Draft)
	}
}

func TestScoping_ScopedFreeTextReplacesDraft(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	d := schedule.HandleSelfChat(s, self("hey Akshay, quick sync this week? options: ...", t0.Add(10*time.Minute), true))
	if d.Action != schedule.ActReplaceDraft {
		t.Fatalf("scoped free text should replace draft: got %s", d.Action)
	}
	if s.Draft != "hey Akshay, quick sync this week? options: ..." {
		t.Fatalf("draft not replaced: %q", s.Draft)
	}
}

// ── Idempotent propose (hardening req 6) — SAFETY CRITICAL ──

func TestPropose_DoubleSendWithinFiveMinutes(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	d1 := schedule.HandleSelfChat(s, self("propose", t0.Add(1*time.Minute), true))
	if d1.Action != schedule.ActPropose {
		t.Fatalf("first propose: got %s", d1.Action)
	}
	d2 := schedule.HandleSelfChat(s, self("propose", t0.Add(3*time.Minute), true))
	if d2.Action != schedule.ActAlreadyProposed {
		t.Fatalf("second propose within 5m must dedupe: got %s", d2.Action)
	}
	d3 := schedule.HandleSelfChat(s, self("propose", t0.Add(10*time.Minute), true))
	if d3.Action != schedule.ActPropose {
		t.Fatalf("propose after window should send: got %s", d3.Action)
	}
}

func TestPropose_SubsetAndValidation(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	d := schedule.HandleSelfChat(s, self("propose 1 3", t0.Add(1*time.Minute), true))
	if d.Action != schedule.ActPropose || len(d.Indices) != 2 || d.Indices[0] != 1 || d.Indices[1] != 3 {
		t.Fatalf("propose 1 3: got %s %v", d.Action, d.Indices)
	}
	s2 := baseSession(schedule.StateSlotsProposed)
	d2 := schedule.HandleSelfChat(s2, self("propose 7", t0.Add(1*time.Minute), true))
	if d2.Action != schedule.ActAsk {
		t.Fatalf("propose out-of-range must ask: got %s", d2.Action)
	}
}

// ── Correction race (hardening req 2) — SAFETY CRITICAL ──

func TestBooking_CounterpartChangedAnswerAfterPing(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	// At execution time the fresh read says they moved to slot 2.
	fresh := &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 2, Confidence: "high"}
	d := schedule.DecideBooking(s, fresh, true, t0.Add(time.Hour))
	if d.Action != schedule.ActSurfaceChange {
		t.Fatalf("changed answer must surface, not book: got %s", d.Action)
	}
}

func TestBooking_CounterpartWithdrewAfterPing(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	fresh := &schedule.Interpretation{Intent: schedule.ReplyAmbiguous, Confidence: "low"}
	d := schedule.DecideBooking(s, fresh, true, t0.Add(time.Hour))
	if d.Action != schedule.ActSurfaceChange {
		t.Fatalf("withdrawn acceptance must surface: got %s", d.Action)
	}
}

func TestBooking_CannotReverifyMeansNoBooking(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	d := schedule.DecideBooking(s, nil, true, t0.Add(time.Hour))
	if d.Action != schedule.ActSurfaceChange {
		t.Fatalf("no fresh read -> no booking: got %s", d.Action)
	}
}

func TestBooking_UnchangedAnswerBooks(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	fresh := &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	d := schedule.DecideBooking(s, fresh, true, t0.Add(time.Hour))
	if d.Action != schedule.ActBook {
		t.Fatalf("stable acceptance should book: got %s", d.Action)
	}
}

func TestBooking_SlotNoLongerFree(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	fresh := &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	d := schedule.DecideBooking(s, fresh, false, t0.Add(time.Hour))
	if d.Action != schedule.ActSlotTaken {
		t.Fatalf("filled slot must not book: got %s", d.Action)
	}
}

func TestBooking_LowConfidenceNeverBooks(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "low"}
	fresh := &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "low"}
	d := schedule.DecideBooking(s, fresh, true, t0.Add(time.Hour))
	if d.Action == schedule.ActBook {
		t.Fatal("low-confidence interpretation must never book")
	}
}

// ── Never book on ambiguity — SAFETY CRITICAL ──

func TestReply_BareYesOverThreeOptionsAsks(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	d := schedule.HandleSelfChat(s, self("yes", t0.Add(time.Minute), true))
	if d.Action != schedule.ActAsk {
		t.Fatalf("bare yes over 3 unproposed options must ask, got %s", d.Action)
	}
	d2 := schedule.HandleSelfChat(s, self("yes 2", t0.Add(2*time.Minute), true))
	if d2.Action != schedule.ActRequestBooking || d2.Index != 2 {
		t.Fatalf("yes 2 should direct-book slot 2: got %s idx=%d", d2.Action, d2.Index)
	}
}

func TestReply_AcceptWithoutIdentifiableSlotSurfacesAsAmbiguous(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	interp := &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 0, Confidence: "high"}
	d := schedule.HandleCounterpartReply(s, interp, t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActSurfaceReply || d.Interp.Intent != schedule.ReplyAmbiguous {
		t.Fatalf("accept-without-slot must degrade to ambiguous surface: %s %+v", d.Action, d.Interp)
	}
}

func TestReply_CounterWithoutConcreteTimeIsAmbiguous(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	interp := &schedule.Interpretation{Intent: schedule.ReplyCounter, CounterTime: "", Confidence: "high"}
	d := schedule.HandleCounterpartReply(s, interp, t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActSurfaceReply || d.Interp.Intent != schedule.ReplyAmbiguous {
		t.Fatalf("vague counter must surface as ambiguous: %s", d.Action)
	}
}

// ── Counterpart reply routing ──

func TestReply_AcceptSurfacesWithPrompt(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	interp := &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 2, Confidence: "high"}
	d := schedule.HandleCounterpartReply(s, interp, t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActSurfaceReply {
		t.Fatalf("acceptance should surface (never auto-book): got %s", d.Action)
	}
	if s.State != schedule.StateReplySurfaced || s.Surfaced.SlotIndex != 2 {
		t.Fatalf("session should record surfaced interp")
	}
}

func TestReply_ConcreteCounterTriggersVerification(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	interp := &schedule.Interpretation{Intent: schedule.ReplyCounter, CounterTime: "2026-07-21T17:00:00+05:30", Confidence: "high"}
	d := schedule.HandleCounterpartReply(s, interp, t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActVerifyCounter {
		t.Fatalf("concrete counter should be verified as pickable option (req 10): got %s", d.Action)
	}
}

func TestReply_UnrelatedKeepsWatching(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	interp := &schedule.Interpretation{Intent: schedule.ReplyUnrelated, Confidence: "high"}
	d := schedule.HandleCounterpartReply(s, interp, t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActNone || s.State != schedule.StateAwaitingReply {
		t.Fatalf("unrelated chatter must not disturb the session: %s", d.Action)
	}
}

// ── Manual resolution (hardening req 9) — SAFETY CRITICAL ──

func TestOwnMessage_ManualAgreementStandsDown(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleOwnMessage(s, true, t0.Add(3*time.Hour))
	if d.Action != schedule.ActStandDown || s.State != schedule.StateClosed {
		t.Fatalf("manual agreement must stand the watcher down: %s state=%s", d.Action, s.State)
	}
}

func TestOwnMessage_OrdinaryChatterDoesNothing(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleOwnMessage(s, false, t0.Add(3*time.Hour))
	if d.Action != schedule.ActNone || s.State != schedule.StateAwaitingReply {
		t.Fatalf("non-finalizing own message must be inert: %s", d.Action)
	}
}

// ── Session lifecycle ──

func TestLifecycle_LeaveItCloses(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	d := schedule.HandleSelfChat(s, self("leave it", t0.Add(time.Minute), true))
	if d.Action != schedule.ActClose || s.State != schedule.StateClosed {
		t.Fatalf("leave it should close: %s", d.Action)
	}
}

func TestLifecycle_SilentExpiryAt48h(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	if d := schedule.CheckExpiry(s, t0.Add(47*time.Hour)); d.Action != schedule.ActNone {
		t.Fatalf("47h should not expire: %s", d.Action)
	}
	if d := schedule.CheckExpiry(s, t0.Add(49*time.Hour)); d.Action != schedule.ActExpire {
		t.Fatalf("49h should expire silently: %s", d.Action)
	}
	if s.State != schedule.StateClosed {
		t.Fatal("expired session must be closed")
	}
}

func TestLifecycle_CancelFlow(t *testing.T) {
	s := baseSession(schedule.StateConfirmCancel)
	s.Intent = schedule.IntentCancel
	d := schedule.HandleSelfChat(s, self("yes", t0.Add(time.Minute), true))
	if d.Action != schedule.ActCancelMeeting {
		t.Fatalf("yes on cancel prompt should cancel gracefully: %s", d.Action)
	}
	s2 := baseSession(schedule.StateConfirmCancel)
	d2 := schedule.HandleSelfChat(s2, self("yes silent", t0.Add(time.Minute), true))
	if d2.Action != schedule.ActCancelSilent {
		t.Fatalf("yes silent should delete without a note: %s", d2.Action)
	}
}

func TestLifecycle_EditThenNewText(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleSelfChat(s, self("edit", t0.Add(time.Minute), true))
	if d.Action != schedule.ActEditPrompt {
		t.Fatalf("edit should prompt for new text: %s", d.Action)
	}
	d2 := schedule.HandleSelfChat(s, self("new draft text here", t0.Add(2*time.Minute), true))
	if d2.Action != schedule.ActReplaceDraft || s.Draft != "new draft text here" {
		t.Fatalf("text after edit should replace draft: %s %q", d2.Action, s.Draft)
	}
}

func TestLifecycle_DisambiguationPick(t *testing.T) {
	s := baseSession(schedule.StateResolving)
	d := schedule.HandleSelfChat(s, self("2", t0.Add(time.Minute), true))
	if d.Action != schedule.ActPickContact || d.Index != 2 {
		t.Fatalf("numeric pick should resolve contact: %s idx=%d", d.Action, d.Index)
	}
	s2 := baseSession(schedule.StateResolving)
	d2 := schedule.HandleSelfChat(s2, self("b", t0.Add(time.Minute), true))
	if d2.Action != schedule.ActPickContact || d2.Index != 2 {
		t.Fatalf("letter pick should resolve contact: %s idx=%d", d2.Action, d2.Index)
	}
}

// ── Command parsing ──

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in            string
		name          string
		verb          schedule.SessionIntent
		dur           int
		format, window string
	}{
		{"akshay", "akshay", schedule.IntentSchedule, 0, "", ""},
		{"akshay shah 45m call this week", "akshay shah", schedule.IntentSchedule, 45, "call", "this week"},
		{"rahul 1h coffee next week", "rahul", schedule.IntentSchedule, 60, "in-person", "next week"},
		{"move akshay", "akshay", schedule.IntentMove, 0, "", ""},
		{"cancel akshay", "akshay", schedule.IntentCancel, 0, "", ""},
		{"steve zoom tomorrow", "steve", schedule.IntentSchedule, 0, "video", "tomorrow"},
		{"priya 30 min", "priya", schedule.IntentSchedule, 30, "", ""},
	}
	for _, c := range cases {
		cmd, err := schedule.ParseCommand(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if cmd.Name != c.name || cmd.Verb != c.verb || cmd.DurationMin != c.dur || cmd.Format != c.format || cmd.Window != c.window {
			t.Errorf("%q: got %+v", c.in, cmd)
		}
	}
	if _, err := schedule.ParseCommand(""); err == nil {
		t.Error("empty command should error")
	}
}

// ── Timezone inference (hardening req 5) ──

func TestInferContactTZ(t *testing.T) {
	tz, note := schedule.InferContactTZ("919812345678@s.whatsapp.net")
	if tz != "Asia/Kolkata" || note == "" {
		t.Errorf("+91 should infer India with a stated note: %s %q", tz, note)
	}
	tz, note = schedule.InferContactTZ("14155552671@s.whatsapp.net")
	if tz != "America/Los_Angeles" || note == "" {
		t.Errorf("+1 should guess SF and say so: %s %q", tz, note)
	}
	if tz, _ = schedule.InferContactTZ("123456789@lid"); tz != "" {
		t.Errorf("LID contact has no phone number to infer from: %s", tz)
	}
}
