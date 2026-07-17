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

// Scoped free text no longer becomes the draft on sight — it goes to the
// instruction-vs-draft classifier first. Silently arming an instruction as the
// outbound message is the foot-gun this routing exists to close; the engine
// itself now decides nothing here.
func TestScoping_ScopedFreeTextGoesToClassifier(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	orig := s.Draft
	d := schedule.HandleSelfChat(s, self("hey Akshay, quick sync this week? options: ...", t0.Add(10*time.Minute), true))
	if d.Action != schedule.ActClassifyText {
		t.Fatalf("scoped free text must be classified, not armed: got %s", d.Action)
	}
	if d.Text != "hey Akshay, quick sync this week? options: ..." {
		t.Fatalf("classifier must receive the text verbatim: %q", d.Text)
	}
	if s.Draft != orig {
		t.Fatalf("engine must not touch the draft before classification: %q", s.Draft)
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

// ── Soft yes / Held state — SAFETY CRITICAL: a hedge is not consent ──

func softYes(slot int) *schedule.Interpretation {
	return &schedule.Interpretation{Intent: schedule.ReplySoftYes, SlotIndex: slot, Confidence: "high"}
}

func TestSoftYes_HoldsAndNeverBooks(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleCounterpartReply(s, softYes(2), t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActHold {
		t.Fatalf("soft yes must hold, never book: got %s", d.Action)
	}
	if d.Index != 2 {
		t.Fatalf("hold must name the slot it points at: got %d", d.Index)
	}
	if s.State != schedule.StateHeld {
		t.Fatalf("soft yes must park the session in held: got %s", s.State)
	}
	if s.BookedEventID != "" || s.BookedSlot != nil {
		t.Fatal("soft yes must not book anything")
	}
}

func TestSoftYes_KeepsWatchingAndFirmUpSurfacesAsAccept(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	schedule.HandleCounterpartReply(s, softYes(2), t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if s.State != schedule.StateHeld {
		t.Fatalf("precondition: want held, got %s", s.State)
	}
	// They firm up the next day.
	firm := &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 2, Confidence: "high"}
	d := schedule.HandleCounterpartReply(s, firm, t0.Add(20*time.Hour), t0.Add(20*time.Hour))
	if d.Action != schedule.ActSurfaceReply || d.Interp.Intent != schedule.ReplyAccept {
		t.Fatalf("firm-up from held must surface as a normal accept: got %s", d.Action)
	}
	if s.State != schedule.StateReplySurfaced {
		t.Fatalf("firm-up should move held → reply_surfaced: got %s", s.State)
	}
}

func TestSoftYes_UserCanForceItWithYes(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	schedule.HandleCounterpartReply(s, softYes(3), t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	s.MarkPrompted(t0.Add(2 * time.Hour))
	// The user saw "this is a soft yes" and decided to lock it anyway.
	d := schedule.HandleSelfChat(s, self("yes", t0.Add(2*time.Hour+time.Minute), true))
	if d.Action != schedule.ActRequestBooking || d.Index != 3 {
		t.Fatalf("'yes' over a hold must book the held slot: got %s idx=%d", d.Action, d.Index)
	}
}

func TestSoftYes_WithoutAnIdentifiableSlotIsJustAmbiguity(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleCounterpartReply(s, softYes(0), t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActSurfaceReply || d.Interp.Intent != schedule.ReplyAmbiguous {
		t.Fatalf("hedge at nothing in particular is ambiguous: got %s", d.Action)
	}
	if s.State == schedule.StateHeld {
		t.Fatal("must not hold on a slot we can't name")
	}
}

func TestSoftYes_WatcherStaysLiveInHeld(t *testing.T) {
	s := baseSession(schedule.StateHeld)
	// Unrelated banter must not disturb a held session, and must not close it.
	d := schedule.HandleCounterpartReply(s, &schedule.Interpretation{Intent: schedule.ReplyUnrelated, Confidence: "high"}, t0.Add(3*time.Hour), t0.Add(3*time.Hour))
	if d.Action != schedule.ActNone || s.State != schedule.StateHeld {
		t.Fatalf("held session must keep watching through banter: %s state=%s", d.Action, s.State)
	}
}

// ── Deference — we pick, and we pick well ──

func TestDeference_PicksEarliestWhenNoneAdjacent(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyDeference, Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActAutoBook {
		t.Fatalf("deference must auto-pick: got %s", d.Action)
	}
	if d.Index != 1 {
		t.Fatalf("no adjacent slots → earliest wins: got %d", d.Index)
	}
	if d.Reason == "" {
		t.Fatal("the pick must come with a reason to name in the self-chat")
	}
}

func TestDeference_PrefersAdjacentOverEarlier(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	// Slot 3 butts against an existing meeting; slots 1 and 2 would split a
	// free block. The adjacent one wins despite being latest.
	s.Slots[2].Adjacent = true
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyDeference, Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Index != 3 {
		t.Fatalf("adjacent slot must win (it doesn't fragment the day): got %d", d.Index)
	}
}

func TestDeference_EarliestAmongAdjacent(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	s.Slots[1].Adjacent = true
	s.Slots[2].Adjacent = true
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyDeference, Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Index != 2 {
		t.Fatalf("among adjacent slots the earliest wins: got %d", d.Index)
	}
}

func TestDeference_StaysInsideTheSubsetTheyNamed(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	// "Wed or Thu both fine, you choose" — slot 1 is off the table even though
	// it's earliest.
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyDeference, DeferSlots: []int{2, 3}, Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Index != 2 {
		t.Fatalf("deference within a subset must pick inside it: got %d", d.Index)
	}
}

func TestDeference_NeverPicksAPastSlot(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	// Read three days late: every offered slot has gone by.
	late := t0.Add(96 * time.Hour)
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyDeference, Confidence: "high"}, late, late)
	if d.Action != schedule.ActSlotPast {
		t.Fatalf("all options passed → recompute, never book the past: got %s", d.Action)
	}
}

func TestPickDeferredSlot_SkipsPastSlotsAndPicksNextLive(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	// Slot 1 (Tue) has passed; slots 2 and 3 are still ahead.
	now := s.Slots[0].Start.Add(time.Hour)
	idx, why := schedule.PickDeferredSlot(s.Slots, nil, now)
	if idx != 2 {
		t.Fatalf("must skip the passed slot and take the next live one: got %d", idx)
	}
	if why == "" {
		t.Fatal("want a reason")
	}
}

// ── Scope change — recompute, never re-offer stale options ──

func TestScopeChange_MutatesSessionAndRequiresRecompute(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyScopeChange, NewDurationMin: 60, Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActScopeChange {
		t.Fatalf("scope change must trigger a recompute: got %s", d.Action)
	}
	if s.DurationMin != 60 {
		t.Fatalf("session duration must be mutated: got %d", s.DurationMin)
	}
	if s.State != schedule.StateSlotsProposed {
		t.Fatalf("new options need a fresh 'propose': got %s", s.State)
	}
}

func TestScopeChange_FormatChangeMutatesFormat(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyScopeChange, NewFormat: "in-person", NeedsVenue: true, Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if s.Format != "in-person" {
		t.Fatalf("format must be mutated (in-person needs the travel buffer): got %q", s.Format)
	}
}

// ── Directive — check the time they named; don't send options back ──

func TestDirective_VerifiesStatedTime(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyDirective, CounterTime: "2026-07-21T17:00:00+05:30", Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActVerifyCounter {
		t.Fatalf("a directive must be checked against the calendar and surfaced: got %s", d.Action)
	}
	if s.State != schedule.StateReplySurfaced {
		t.Fatalf("want reply_surfaced, got %s", s.State)
	}
}

func TestDirective_WithoutAConcreteTimeIsAmbiguous(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyDirective, CounterTime: "", Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActSurfaceReply || d.Interp.Intent != schedule.ReplyAmbiguous {
		t.Fatalf("'call me later' has nothing to check: got %s", d.Action)
	}
}

// ── Not scheduling — stand down cleanly — SAFETY CRITICAL ──

func TestNotScheduling_ClosesQuietly(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyNotScheduling, Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActNotScheduling {
		t.Fatalf("must stand down: got %s", d.Action)
	}
	if s.State != schedule.StateClosed {
		t.Fatalf("not_scheduling must close the session: got %s", s.State)
	}
	if d.Reason != "not_scheduling" {
		t.Fatalf("quiet close reason: got %q", d.Reason)
	}
}

func TestNotScheduling_WrongPersonIsLoud(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyNotScheduling, WrongPerson: true, Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Reason != "wrong_person" {
		t.Fatalf("'who is this' must be loud — the user texted a stranger: got %q", d.Reason)
	}
	if s.State != schedule.StateClosed {
		t.Fatalf("must still close: got %s", s.State)
	}
}

func TestNotScheduling_ClosesFromHeldToo(t *testing.T) {
	s := baseSession(schedule.StateHeld)
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyNotScheduling, Confidence: "high"},
		t0.Add(2*time.Hour), t0.Add(2*time.Hour))
	if d.Action != schedule.ActNotScheduling || s.State != schedule.StateClosed {
		t.Fatalf("a held session must also stand down: %s state=%s", d.Action, s.State)
	}
}

// ── Past-slot guard — SAFETY CRITICAL: never book the past ──

func TestPastSlot_AcceptedSlotThatHasPassedIsNotSurfacedForBooking(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	// They reply three days late and pick slot 1, which was last Tuesday.
	late := t0.Add(96 * time.Hour)
	d := schedule.HandleCounterpartReply(s,
		&schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}, late, late)
	if d.Action != schedule.ActSlotPast {
		t.Fatalf("accepting a slot that already happened must recompute: got %s", d.Action)
	}
	if s.State == schedule.StateReplySurfaced {
		t.Fatal("must not stage a past slot for booking")
	}
}

func TestPastSlot_DecideBookingRefusesAPassedTarget(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	fresh := &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	// Free on the calendar, unchanged in the thread — and still unbookable,
	// because the slot's start has gone by.
	late := s.Slots[0].Start.Add(time.Hour)
	d := schedule.DecideBooking(s, fresh, true, late)
	if d.Action != schedule.ActSlotPast {
		t.Fatalf("a passed slot must never book, even free and uncontested: got %s", d.Action)
	}
}

func TestPastSlot_DecideBookingRefusesAPassedCounterTime(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	past := t0.Add(-2 * time.Hour).Format(time.RFC3339)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyCounter, CounterTime: past, Confidence: "high"}
	fresh := &schedule.Interpretation{Intent: schedule.ReplyCounter, CounterTime: past, Confidence: "high"}
	d := schedule.DecideBooking(s, fresh, true, t0)
	if d.Action != schedule.ActSlotPast {
		t.Fatalf("a counter time in the past must never book: got %s", d.Action)
	}
}

func TestPastSlot_LiveSlotStillBooks(t *testing.T) {
	s := baseSession(schedule.StateReplySurfaced)
	s.Surfaced = &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	fresh := &schedule.Interpretation{Intent: schedule.ReplyAccept, SlotIndex: 1, Confidence: "high"}
	if d := schedule.DecideBooking(s, fresh, true, t0.Add(time.Hour)); d.Action != schedule.ActBook {
		t.Fatalf("the guard must not block a future slot: got %s", d.Action)
	}
}

// ── Media surfacing — a voice note must not be invisible ──

func TestMedia_SurfacesWhileAwaitingReply(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	d := schedule.HandleMediaMessage(s, schedule.MediaVoice, t0.Add(2*time.Hour))
	if d.Action != schedule.ActSurfaceMedia {
		t.Fatalf("a voice note while awaiting a reply must surface: got %s", d.Action)
	}
	if d.Reason != string(schedule.MediaVoice) {
		t.Fatalf("must name what arrived: got %q", d.Reason)
	}
	if s.State != schedule.StateAwaitingReply {
		t.Fatalf("session must stay open — they may follow up with text: got %s", s.State)
	}
}

func TestMedia_RateLimitedToOnePerBurst(t *testing.T) {
	s := baseSession(schedule.StateAwaitingReply)
	if d := schedule.HandleMediaMessage(s, schedule.MediaImage, t0.Add(2*time.Hour)); d.Action != schedule.ActSurfaceMedia {
		t.Fatalf("first media must surface: got %s", d.Action)
	}
	// Four more photos in the same burst must not produce four more nudges.
	for i := 1; i <= 4; i++ {
		d := schedule.HandleMediaMessage(s, schedule.MediaImage, t0.Add(2*time.Hour+time.Duration(i)*time.Second))
		if d.Action != schedule.ActNone {
			t.Fatalf("photo %d in the burst must be silent: got %s", i+1, d.Action)
		}
	}
	// A new burst much later does surface again.
	if d := schedule.HandleMediaMessage(s, schedule.MediaVoice, t0.Add(4*time.Hour)); d.Action != schedule.ActSurfaceMedia {
		t.Fatalf("a later burst should surface again: got %s", d.Action)
	}
}

func TestMedia_IgnoredOutsideAwaitingReply(t *testing.T) {
	for _, st := range []schedule.State{schedule.StateSlotsProposed, schedule.StateReplySurfaced, schedule.StateHeld, schedule.StateClosed} {
		s := baseSession(st)
		if d := schedule.HandleMediaMessage(s, schedule.MediaVoice, t0.Add(2*time.Hour)); d.Action != schedule.ActNone {
			t.Fatalf("media in state %s must be inert: got %s", st, d.Action)
		}
	}
}

// ── Instruction vs draft — SAFETY CRITICAL: never silently arm a draft ──

func TestSelfText_InstructionChangesTheSearchNotTheDraft(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	orig := s.Draft
	class := &schedule.SelfTextClass{Kind: schedule.SelfTextInstruction, Window: "Tue or Wed", Confidence: "high"}
	d := schedule.ApplySelfText(s, "he asked for Tue or Wed, our entire proposal is wrong", class, t0.Add(time.Minute))
	if d.Action != schedule.ActApplyInstruction {
		t.Fatalf("an instruction must be acted on: got %s", d.Action)
	}
	if s.Draft != orig {
		t.Fatalf("an instruction must NEVER become the outbound draft: %q", s.Draft)
	}
	if s.Window != "Tue or Wed" {
		t.Fatalf("the instruction must change the window: %q", s.Window)
	}
	if !d.Class.NeedsRecompute() {
		t.Fatal("a window change must force a recompute")
	}
}

func TestSelfText_InstructionCanChangeDurationAndFormat(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	class := &schedule.SelfTextClass{Kind: schedule.SelfTextInstruction, DurationMin: 45, Format: "call", Confidence: "high"}
	schedule.ApplySelfText(s, "actually 45 mins, and make it a call", class, t0.Add(time.Minute))
	if s.DurationMin != 45 || s.Format != "call" {
		t.Fatalf("instruction must mutate duration/format: %d %q", s.DurationMin, s.Format)
	}
}

func TestSelfText_ToneInstructionRedraftsWithoutRecomputing(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	class := &schedule.SelfTextClass{Kind: schedule.SelfTextInstruction, ToneNote: "warmer", Confidence: "high"}
	d := schedule.ApplySelfText(s, "make it warmer", class, t0.Add(time.Minute))
	if d.Action != schedule.ActApplyInstruction {
		t.Fatalf("got %s", d.Action)
	}
	if s.ToneNote != "warmer" {
		t.Fatalf("tone note must stick to the session: %q", s.ToneNote)
	}
	if d.Class.NeedsRecompute() {
		t.Fatal("a wording change must not recompute slots")
	}
}

func TestSelfText_RealDraftReplacesTheDraft(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	msg := "hey Akshay! sorry for the delay — any of these work? 1. Tue 3pm 2. Wed 11am"
	class := &schedule.SelfTextClass{Kind: schedule.SelfTextDraft, Confidence: "high"}
	d := schedule.ApplySelfText(s, msg, class, t0.Add(time.Minute))
	if d.Action != schedule.ActReplaceDraft || s.Draft != msg {
		t.Fatalf("a real draft should replace the draft: %s %q", d.Action, s.Draft)
	}
}

// The self-chat doubles as a notepad. A grocery list must not be armed as a
// draft — and must not be answered with a question either.
func TestSelfText_PersonalNoteFallsThroughSilently(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	orig := s.Draft
	class := &schedule.SelfTextClass{Kind: schedule.SelfTextNote, Confidence: "high"}
	d := schedule.ApplySelfText(s, "buy milk, renew passport", class, t0.Add(time.Minute))
	if d.Action != schedule.ActNone {
		t.Fatalf("a personal note must be ignored in silence: got %s", d.Action)
	}
	if s.Draft != orig {
		t.Fatalf("a personal note must never touch the draft: %q", s.Draft)
	}
}

func TestSelfText_LowConfidenceNoteStillAsks(t *testing.T) {
	// If we're not sure it's a note, asking beats silently ignoring something
	// that might have been an instruction.
	s := baseSession(schedule.StateSlotsProposed)
	class := &schedule.SelfTextClass{Kind: schedule.SelfTextNote, Confidence: "low"}
	if d := schedule.ApplySelfText(s, "tuesday", class, t0.Add(time.Minute)); d.Action != schedule.ActAsk {
		t.Fatalf("an unconfident note must ask: got %s", d.Action)
	}
}

func TestSelfText_UnclearAsksAndNeverArms(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	orig := s.Draft
	class := &schedule.SelfTextClass{Kind: schedule.SelfTextUnclear, Confidence: "low"}
	d := schedule.ApplySelfText(s, "wrong", class, t0.Add(time.Minute))
	if d.Action != schedule.ActAsk {
		t.Fatalf("unclear must ask: got %s", d.Action)
	}
	if s.Draft != orig {
		t.Fatalf("unclear must never arm a draft: %q", s.Draft)
	}
}

func TestSelfText_LowConfidenceDraftIsNeverArmed(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	orig := s.Draft
	// Even classified "draft", low confidence must not arm it.
	class := &schedule.SelfTextClass{Kind: schedule.SelfTextDraft, Confidence: "low"}
	d := schedule.ApplySelfText(s, "tuesday", class, t0.Add(time.Minute))
	if d.Action != schedule.ActAsk || s.Draft != orig {
		t.Fatalf("low-confidence draft must ask, not arm: %s %q", d.Action, s.Draft)
	}
}

func TestSelfText_ClassifierFailureNeverArms(t *testing.T) {
	s := baseSession(schedule.StateSlotsProposed)
	orig := s.Draft
	// A nil class models the classifier erroring out.
	d := schedule.ApplySelfText(s, "he asked for Tue or Wed", nil, t0.Add(time.Minute))
	if d.Action != schedule.ActAsk || s.Draft != orig {
		t.Fatalf("classifier failure must degrade to asking: %s %q", d.Action, s.Draft)
	}
}

func TestSelfText_AfterExplicitEditTextIsStillTheDraft(t *testing.T) {
	// "edit" is an explicit request for draft text — no classification needed,
	// and the old behavior must be preserved.
	s := baseSession(schedule.StateSlotsProposed)
	schedule.HandleSelfChat(s, self("edit", t0.Add(time.Minute), true))
	d := schedule.HandleSelfChat(s, self("he asked for Tue or Wed", t0.Add(2*time.Minute), true))
	if d.Action != schedule.ActReplaceDraft {
		t.Fatalf("after 'edit', text is the draft by construction: got %s", d.Action)
	}
}

// ── Command parsing ──

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in             string
		name           string
		verb           schedule.SessionIntent
		dur            int
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
