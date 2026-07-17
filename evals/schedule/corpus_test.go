package scheduleevals

import (
	"time"

	"github.com/msfoundry/commit/schedule"
)

// The LLM eval corpus: thread state in → expected interpretation out.
// Categories marked safetyCritical must pass at 100%; others must clear 95%.

type llmCase struct {
	name     string
	category string
	// slots offered (defaults to the standard three when nil)
	slots []schedule.Slot
	// draft override (defaults to stdDraft); must be consistent with slots
	draft string
	// thread after the draft, oldest first
	thread []schedule.ThreadMsg
	// grading
	wantIntents     []schedule.ReplyIntent // any of these passes
	wantSlot        int                    // -1 = don't care
	wantSideNote    bool                   // side_note must be non-empty
	forbidAccept    bool                   // classifying as accept is an automatic fail
	wantCounterDay  time.Weekday           // for counters: proposed time must land on this weekday (-1 = don't care)
	wantCounterHour int                    // local hour the counter must land on (-1 = don't care)

	// New-taxonomy grading. Zero values mean "don't care".
	wantDeferSlots  []int  // deference: exact subset they limited us to
	wantDuration    int    // scope_change: new_duration_min
	wantFormat      string // scope_change: new_format
	wantPlatform    string // scope_change: requested_platform
	wantNeedsVenue  bool   // scope_change: needs_venue must be true
	wantWrongPerson int    // not_scheduling: 1 = must be true, -1 = must be false, 0 = don't care
}

var evalLoc = time.FixedZone("IST", 5*3600+1800)

// Standard scenario: Mon Jul 20 2026 10:00 IST; offered
// 1. Tue Jul 21 3:00–3:30 PM, 2. Wed Jul 22 11:00–11:30 AM, 3. Thu Jul 23 4:00–4:30 PM.
var evalNow = time.Date(2026, 7, 20, 10, 0, 0, 0, evalLoc)

func slot(day, hour, min int) schedule.Slot {
	st := time.Date(2026, 7, day, hour, min, 0, 0, evalLoc)
	return schedule.Slot{Start: st, End: st.Add(30 * time.Minute)}
}

var stdSlots = []schedule.Slot{slot(21, 15, 0), slot(22, 11, 0), slot(23, 16, 0)}

const stdDraft = "hey! wanted to catch up on the partnership stuff — free any of these?\n1. Tue 3pm\n2. Wed 11am\n3. Thu 4pm"

func them(texts ...string) []schedule.ThreadMsg {
	var out []schedule.ThreadMsg
	base := evalNow.Add(2 * time.Hour)
	for i, t := range texts {
		out = append(out, schedule.ThreadMsg{FromMe: false, Text: t, Time: base.Add(time.Duration(i) * 10 * time.Minute)})
	}
	return out
}

func accept(name, text string, slotIdx int) llmCase {
	return llmCase{name: name, category: "acceptance", thread: them(text),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: slotIdx, wantCounterDay: -1, wantCounterHour: -1}
}

func ambiguous(name, text string) llmCase {
	return llmCase{name: name, category: "ambiguous", thread: them(text),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAmbiguous},
		wantSlot:    -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1}
}

func reject(name, text string) llmCase {
	return llmCase{name: name, category: "rejection", thread: them(text),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyReject}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1}
}

func unrelated(name, text string) llmCase {
	return llmCase{name: name, category: "non_reply", thread: them(text),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyUnrelated}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1}
}

// softYesCase — a hedged pointer at one option. SAFETY CRITICAL: must never
// come back as accept, or we book a meeting they never agreed to.
func softYesCase(name, text string, slotIdx int) llmCase {
	return llmCase{name: name, category: "soft_yes", thread: them(text),
		wantIntents: []schedule.ReplyIntent{schedule.ReplySoftYes}, wantSlot: slotIdx,
		forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1}
}

func deferCase(name, text string, subset []int) llmCase {
	return llmCase{name: name, category: "deference", thread: them(text),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyDeference}, wantSlot: -1,
		wantDeferSlots: subset, wantCounterDay: -1, wantCounterHour: -1}
}

func notSchedulingCase(name, text string, wrongPerson int) llmCase {
	return llmCase{name: name, category: "not_scheduling", thread: them(text),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyNotScheduling}, wantSlot: -1,
		forbidAccept: true, wantWrongPerson: wrongPerson, wantCounterDay: -1, wantCounterHour: -1}
}

var llmCases = []llmCase{
	// ── Acceptances ──
	accept("plain_day", "Tue works", 1),
	accept("time_only_emoji", "3pm 👍", 1),
	accept("ordinal_reference", "let's do the second one", 2),
	accept("day_time_exact", "Wed 11 is perfect", 2),
	accept("day_reference", "the thursday slot pls", 3),
	accept("works_with_day", "works for me — tuesday", 1),
	accept("option_number", "option 3 pls", 3),
	accept("casual_yes_day", "yes thursday 4 works", 3),
	{name: "single_option_sounds_good", category: "acceptance",
		slots:  []schedule.Slot{slot(21, 15, 0)},
		draft:  "hey! quick sync on the partnership — does Tue 3pm work?",
		thread: them("sounds good"), wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: 1, wantCounterDay: -1, wantCounterHour: -1},
	{name: "single_option_thumbs", category: "acceptance",
		slots:  []schedule.Slot{slot(22, 11, 0)},
		draft:  "hey! quick sync on the partnership — does Wed 11am work?",
		thread: them("👍"), wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: 1, wantCounterDay: -1, wantCounterHour: -1},

	// ── Corrections after acceptance (SAFETY CRITICAL: latest state wins;
	//    returning the stale acceptance is the correction-race bug) ──
	{name: "correct_to_other_offered_slot", category: "correction",
		thread:      them("Tue works", "actually hold on — tue just got blocked. wed instead?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: 2, wantCounterDay: -1, wantCounterHour: -1},
	{name: "correct_to_new_time", category: "correction",
		thread:      them("Tue 3 works!", "so sorry, something came up tuesday. could we do friday 2pm?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter}, wantSlot: 0, wantCounterDay: time.Friday, wantCounterHour: 14},
	{name: "correct_withdraw_no_alternative", category: "correction",
		thread:      them("Wed 11 is great", "wait no, I'm traveling wed after all. hmm"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAmbiguous, schedule.ReplyReject}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	// Under the soft-yes taxonomy this reads as a hedge aimed at Thursday
	// rather than as formless ambiguity. Both are safe — neither books — and
	// soft_yes is the more precise answer, so both grade as a pass.
	{name: "correct_to_unsure", category: "correction",
		thread:      them("thursday!", "hmm actually let me check my calendar first, don't lock it yet"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAmbiguous, schedule.ReplySoftYes}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "correct_to_full_reject", category: "correction",
		thread:      them("Tue 3 works", "actually I can't do this week at all, really sorry"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyReject, schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},

	// ── Counter-proposals (req 10: verify + surface as pickable) ──
	{name: "counter_same_day_later", category: "counter",
		thread:      them("can we do Tue 5 instead?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter}, wantSlot: 0, wantCounterDay: time.Tuesday, wantCounterHour: 17},
	{name: "counter_friday_morning", category: "counter",
		thread:      them("how about friday 10am?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter}, wantSlot: 0, wantCounterDay: time.Friday, wantCounterHour: 10},
	{name: "counter_explicit_date", category: "counter",
		thread:      them("none of those work — monday the 27th, 3pm?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter}, wantSlot: 0, wantCounterDay: time.Monday, wantCounterHour: 15},
	{name: "counter_shift_offered_slot", category: "counter",
		thread:      them("could we push tue to 4:30?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter}, wantSlot: 0, wantCounterDay: time.Tuesday, wantCounterHour: 16},
	{name: "counter_same_time_other_day", category: "counter",
		thread: them("what about same time wednesday?"),
		// Hard case: "same time" = 3pm (slot 1's time) on Wed. Ambiguous is acceptable; accepting a slot is not.
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter, schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},

	// ── Ambiguous (SAFETY CRITICAL: never classify as accept) ──
	ambiguous("vague_window", "early next week is better for me"),
	ambiguous("will_check", "let me check and get back to you"),
	ambiguous("sounds_good_multi", "sounds good!"),
	// "either works" sits on the ambiguous/deference boundary: read as a
	// sloppy "any of them work" it IS deference, and picking is then the right
	// move — they blessed every option. Both readings are safe (neither books
	// a slot they didn't agree to); guessing a single slot is not.
	{name: "either_works", category: "ambiguous", thread: them("either works"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAmbiguous, schedule.ReplyDeference},
		wantSlot:    -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	ambiguous("maybe", "maybe, not sure yet"),
	ambiguous("two_candidates", "probably tue or thu"),

	// ── Rejections ──
	reject("slammed", "can't this week, totally slammed"),
	reject("pass", "sorry, gonna pass on this for now"),
	{name: "revisit_next_month", category: "rejection",
		thread:      them("no bandwidth rn — let's revisit next month?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyReject, schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	reject("flat_no", "not going to work, sorry"),

	// ── Mixed-topic replies ──
	{name: "accept_plus_ask", category: "mixed",
		thread:      them("Tue works. also can you send over the deck before?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: 1, wantSideNote: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "accept_plus_chatter", category: "mixed",
		thread:      them("wed 11 👍 btw did you watch the game last night??"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: 2, wantCounterDay: -1, wantCounterHour: -1},
	{name: "reject_plus_info", category: "mixed",
		thread:      them("can't make any of those. separately — the invoice got paid today"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyReject, schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantSideNote: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "ambiguous_plus_ask", category: "mixed",
		thread:      them("let me check. meanwhile can you intro me to Rahul?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantSideNote: true, wantCounterDay: -1, wantCounterHour: -1},

	// ── Non-replies / unrelated ──
	unrelated("meme", "haha that meme was great 😂"),
	unrelated("logistics", "did you reach home ok?"),
	{name: "voice_call", category: "non_reply", thread: them("[Voice call]"),
		// A call right after a proposal may be them answering by phone: both
		// "unrelated" (keep watching) and "ambiguous" (surface it) are safe.
		wantIntents: []schedule.ReplyIntent{schedule.ReplyUnrelated, schedule.ReplyAmbiguous},
		wantSlot:    -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	unrelated("birthday", "happy birthday man!! 🎂"),

	// ── Soft yes (SAFETY CRITICAL: a hedge is not consent — never accept) ──
	softYesCase("confirm_tomorrow", "Tue works, let me just confirm tomorrow", 1),
	softYesCase("probably", "probably wed", 2),
	softYesCase("should_work", "wed should work", 2),
	softYesCase("check_with_team", "thursday 4 is likely fine, need to check with my team first", 3),
	softYesCase("pencil_in", "pencil me in for tue 3 for now", 1),
	softYesCase("conditional_flight", "tue 3 works I think, as long as my flight lands on time", 1),
	softYesCase("tentatively", "tentatively wed 11 — I'll confirm once I've spoken to Priya", 2),

	// ── Deference (they hand the pick back — we pick) ──
	deferCase("any_of_these", "any of these work", nil),
	deferCase("you_pick", "you pick", nil),
	deferCase("whenever_suits", "whenever suits you", nil),
	deferCase("im_flexible", "I'm flexible, whatever's easiest for you", nil),
	deferCase("all_work_your_call", "all three work for me, your call", nil),
	deferCase("subset_tue_wed", "tue or wed both fine, you choose", []int{1, 2}),

	// ── Scope change (the shape moved, not the time) ──
	{name: "make_it_an_hour", category: "scope_change", thread: them("can we make it an hour?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyScopeChange}, wantSlot: -1,
		wantDuration: 60, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "coffee_instead", category: "scope_change", thread: them("let's grab coffee instead"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyScopeChange}, wantSlot: -1,
		wantFormat: "in-person", wantNeedsVenue: true, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "just_call_me", category: "scope_change", thread: them("just call me, no need for a video thing"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyScopeChange}, wantSlot: -1,
		wantFormat: "call", forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "zoom_link", category: "scope_change", thread: them("can you send a zoom link?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyScopeChange}, wantSlot: -1,
		wantFormat: "video", wantPlatform: "zoom", forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "shorter_15", category: "scope_change", thread: them("15 mins is plenty tbh, no need for half an hour"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyScopeChange}, wantSlot: -1,
		wantDuration: 15, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "lunch_an_hour", category: "scope_change", thread: them("actually let's make it lunch — an hour?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyScopeChange}, wantSlot: -1,
		wantDuration: 60, wantFormat: "in-person", forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},

	// ── Directive (they instruct; sending three options back is a wrong move) ──
	{name: "call_me_at_5", category: "directive", thread: them("call me at 5"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyDirective}, wantSlot: -1,
		forbidAccept: true, wantCounterDay: -1, wantCounterHour: 17},
	{name: "come_by_at_2_tomorrow", category: "directive", thread: them("come by at 2 tomorrow"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyDirective}, wantSlot: -1,
		forbidAccept: true, wantCounterDay: time.Tuesday, wantCounterHour: 14},
	{name: "ring_me_thursday_11", category: "directive", thread: them("ring me thursday at 11"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyDirective}, wantSlot: -1,
		forbidAccept: true, wantCounterDay: time.Thursday, wantCounterHour: 11},
	{name: "call_wednesday_6pm", category: "directive", thread: them("call me wednesday at 6pm"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyDirective}, wantSlot: -1,
		forbidAccept: true, wantCounterDay: time.Wednesday, wantCounterHour: 18},
	{name: "ring_friday_9", category: "directive", thread: them("give me a ring friday 9am"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyDirective}, wantSlot: -1,
		forbidAccept: true, wantCounterDay: time.Friday, wantCounterHour: 9},

	// ── Not scheduling (stand down; wrong-number must be loud) ──
	notSchedulingCase("on_email", "let's just do this on email", -1),
	notSchedulingCase("assistant_will", "my assistant will set it up", -1),
	notSchedulingCase("ill_send_invite", "I'll send you an invite myself, don't worry about it", -1),
	notSchedulingCase("who_is_this", "who is this?", 1),
	notSchedulingCase("wrong_number", "wrong number", 1),
	notSchedulingCase("wrong_sam", "sorry, I think you have the wrong Sam", 1),
}

// Own-message cases (manual resolution detection, hardening req 9).
type ownCase struct {
	name          string
	thread        []schedule.ThreadMsg
	wantFinalized bool
}

func me(text string) schedule.ThreadMsg {
	return schedule.ThreadMsg{FromMe: true, Text: text, Time: evalNow.Add(3 * time.Hour)}
}
func themMsg(text string) schedule.ThreadMsg {
	return schedule.ThreadMsg{FromMe: false, Text: text, Time: evalNow.Add(3*time.Hour + 5*time.Minute)}
}

// ── Instruction vs draft (the foot-gun) ──
//
// The user types free text in their own self-chat while a draft is pending.
// Reading an instruction as a draft sends their private note to the contact —
// this happened in the field: "he asked for Tue or Wed, our entire proposal is
// wrong" came back as "Draft updated. It'll go out on your next 'propose'."
type selfTextCase struct {
	name string
	text string
	want schedule.SelfTextKind
	// For instructions: what must be extracted. Empty/zero = don't care.
	wantWindowContains string
	wantDuration       int
	wantFormat         string
	wantTone           bool // tone_note must be non-empty
}

var selfTextCases = []selfTextCase{
	// Instructions — must NEVER be armed as the outbound message.
	{name: "third_person_complaint", text: "he asked for Tue or Wed, our entire proposal is wrong",
		want: schedule.SelfTextInstruction, wantWindowContains: "tue"},
	{name: "only_next_week", text: "only next week please",
		want: schedule.SelfTextInstruction, wantWindowContains: "next week"},
	{name: "change_duration", text: "actually make it 45 mins",
		want: schedule.SelfTextInstruction, wantDuration: 45},
	{name: "change_format", text: "let's make this a call, not a video thing",
		want: schedule.SelfTextInstruction, wantFormat: "call"},
	{name: "tone_warmer", text: "make it warmer", want: schedule.SelfTextInstruction, wantTone: true},
	{name: "tone_shorter", text: "shorter please", want: schedule.SelfTextInstruction, wantTone: true},
	{name: "tone_less_formal", text: "way too formal, he's an old friend",
		want: schedule.SelfTextInstruction, wantTone: true},
	// Two changes in one instruction. The duration must DIFFER from the
	// session's current 30 min, or "0 = unchanged" is a correct answer and the
	// case tests nothing.
	{name: "combined_window_duration", text: "she can only do mornings, and let's make it 45 mins",
		want: schedule.SelfTextInstruction, wantWindowContains: "morning", wantDuration: 45},

	// Real drafts — a complete message addressed to the contact.
	{name: "draft_full_options", text: "hey Akshay! sorry for the slow reply — any of these work for a quick call?\n1. Tue 3pm\n2. Wed 11am\n3. Thu 4pm",
		want: schedule.SelfTextDraft},
	{name: "draft_short_question", text: "hi! would love to catch up properly. does thursday afternoon work for you?",
		want: schedule.SelfTextDraft},
	{name: "draft_apology_reschedule", text: "sorry mate, this week's a write-off on my end — can we do next week instead? I'm free tue or wed",
		want: schedule.SelfTextDraft},

	// Unclear — must ask, never arm.
	{name: "unclear_wrong", text: "wrong", want: schedule.SelfTextUnclear},
	{name: "unclear_bare_day", text: "tuesday", want: schedule.SelfTextUnclear},

	// Personal notes — the self-chat is also a notepad. These must fall
	// through in silence: not armed as a draft, and not asked about either.
	{name: "note_errands", text: "buy milk, renew passport", want: schedule.SelfTextNote},
	{name: "note_plumber", text: "call the plumber back tomorrow", want: schedule.SelfTextNote},
	{name: "note_idea", text: "idea: rewrite the pitch deck intro, it's way too long", want: schedule.SelfTextNote},
}

var ownCases = []ownCase{
	{name: "manual_lock_with_ack", thread: []schedule.ThreadMsg{me("ok let's just do tuesday 3pm, see you then"), themMsg("perfect")}, wantFinalized: true},
	{name: "manual_invite_sent", thread: []schedule.ThreadMsg{me("locked — sending you an invite for wed 11")}, wantFinalized: true},
	{name: "just_nudging", thread: []schedule.ThreadMsg{me("did you get a chance to look at the times?")}, wantFinalized: false},
	{name: "musing_not_agreeing", thread: []schedule.ThreadMsg{me("we could also just catch up at the offsite next month, whatever's easier")}, wantFinalized: false},
	{name: "unrelated_own_msg", thread: []schedule.ThreadMsg{me("btw sent you that article about the acquisition")}, wantFinalized: false},
}
