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
	wantIntents    []schedule.ReplyIntent // any of these passes
	wantSlot       int                    // -1 = don't care
	wantSideNote   bool                   // side_note must be non-empty
	forbidAccept   bool                   // classifying as accept is an automatic fail
	wantCounterDay time.Weekday           // for counters: proposed time must land on this weekday (-1 = don't care)
	wantCounterHour int                   // local hour the counter must land on (-1 = don't care)
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
		wantIntents:  []schedule.ReplyIntent{schedule.ReplyAmbiguous},
		wantSlot:     -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1}
}

func reject(name, text string) llmCase {
	return llmCase{name: name, category: "rejection", thread: them(text),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyReject}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1}
}

func unrelated(name, text string) llmCase {
	return llmCase{name: name, category: "non_reply", thread: them(text),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyUnrelated}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1}
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
		slots: []schedule.Slot{slot(21, 15, 0)},
		draft: "hey! quick sync on the partnership — does Tue 3pm work?",
		thread: them("sounds good"), wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: 1, wantCounterDay: -1, wantCounterHour: -1},
	{name: "single_option_thumbs", category: "acceptance",
		slots: []schedule.Slot{slot(22, 11, 0)},
		draft: "hey! quick sync on the partnership — does Wed 11am work?",
		thread: them("👍"), wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: 1, wantCounterDay: -1, wantCounterHour: -1},

	// ── Corrections after acceptance (SAFETY CRITICAL: latest state wins;
	//    returning the stale acceptance is the correction-race bug) ──
	{name: "correct_to_other_offered_slot", category: "correction",
		thread: them("Tue works", "actually hold on — tue just got blocked. wed instead?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: 2, wantCounterDay: -1, wantCounterHour: -1},
	{name: "correct_to_new_time", category: "correction",
		thread: them("Tue 3 works!", "so sorry, something came up tuesday. could we do friday 2pm?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter}, wantSlot: 0, wantCounterDay: time.Friday, wantCounterHour: 14},
	{name: "correct_withdraw_no_alternative", category: "correction",
		thread: them("Wed 11 is great", "wait no, I'm traveling wed after all. hmm"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAmbiguous, schedule.ReplyReject}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "correct_to_unsure", category: "correction",
		thread: them("thursday!", "hmm actually let me check my calendar first, don't lock it yet"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "correct_to_full_reject", category: "correction",
		thread: them("Tue 3 works", "actually I can't do this week at all, really sorry"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyReject, schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},

	// ── Counter-proposals (req 10: verify + surface as pickable) ──
	{name: "counter_same_day_later", category: "counter",
		thread: them("can we do Tue 5 instead?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter}, wantSlot: 0, wantCounterDay: time.Tuesday, wantCounterHour: 17},
	{name: "counter_friday_morning", category: "counter",
		thread: them("how about friday 10am?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter}, wantSlot: 0, wantCounterDay: time.Friday, wantCounterHour: 10},
	{name: "counter_explicit_date", category: "counter",
		thread: them("none of those work — monday the 27th, 3pm?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter}, wantSlot: 0, wantCounterDay: time.Monday, wantCounterHour: 15},
	{name: "counter_shift_offered_slot", category: "counter",
		thread: them("could we push tue to 4:30?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter}, wantSlot: 0, wantCounterDay: time.Tuesday, wantCounterHour: 16},
	{name: "counter_same_time_other_day", category: "counter",
		thread: them("what about same time wednesday?"),
		// Hard case: "same time" = 3pm (slot 1's time) on Wed. Ambiguous is acceptable; accepting a slot is not.
		wantIntents: []schedule.ReplyIntent{schedule.ReplyCounter, schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},

	// ── Ambiguous (SAFETY CRITICAL: never classify as accept) ──
	ambiguous("vague_window", "early next week is better for me"),
	ambiguous("will_check", "let me check and get back to you"),
	ambiguous("sounds_good_multi", "sounds good!"),
	ambiguous("either_works", "either works"),
	ambiguous("maybe", "maybe, not sure yet"),
	ambiguous("two_candidates", "probably tue or thu"),

	// ── Rejections ──
	reject("slammed", "can't this week, totally slammed"),
	reject("pass", "sorry, gonna pass on this for now"),
	{name: "revisit_next_month", category: "rejection",
		thread: them("no bandwidth rn — let's revisit next month?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyReject, schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	reject("flat_no", "not going to work, sorry"),

	// ── Mixed-topic replies ──
	{name: "accept_plus_ask", category: "mixed",
		thread: them("Tue works. also can you send over the deck before?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: 1, wantSideNote: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "accept_plus_chatter", category: "mixed",
		thread: them("wed 11 👍 btw did you watch the game last night??"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAccept}, wantSlot: 2, wantCounterDay: -1, wantCounterHour: -1},
	{name: "reject_plus_info", category: "mixed",
		thread: them("can't make any of those. separately — the invoice got paid today"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyReject, schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantSideNote: true, wantCounterDay: -1, wantCounterHour: -1},
	{name: "ambiguous_plus_ask", category: "mixed",
		thread: them("let me check. meanwhile can you intro me to Rahul?"),
		wantIntents: []schedule.ReplyIntent{schedule.ReplyAmbiguous}, wantSlot: -1, forbidAccept: true, wantSideNote: true, wantCounterDay: -1, wantCounterHour: -1},

	// ── Non-replies / unrelated ──
	unrelated("meme", "haha that meme was great 😂"),
	unrelated("logistics", "did you reach home ok?"),
	{name: "voice_call", category: "non_reply", thread: them("[Voice call]"),
		// A call right after a proposal may be them answering by phone: both
		// "unrelated" (keep watching) and "ambiguous" (surface it) are safe.
		wantIntents: []schedule.ReplyIntent{schedule.ReplyUnrelated, schedule.ReplyAmbiguous},
		wantSlot: -1, forbidAccept: true, wantCounterDay: -1, wantCounterHour: -1},
	unrelated("birthday", "happy birthday man!! 🎂"),
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

var ownCases = []ownCase{
	{name: "manual_lock_with_ack", thread: []schedule.ThreadMsg{me("ok let's just do tuesday 3pm, see you then"), themMsg("perfect")}, wantFinalized: true},
	{name: "manual_invite_sent", thread: []schedule.ThreadMsg{me("locked — sending you an invite for wed 11")}, wantFinalized: true},
	{name: "just_nudging", thread: []schedule.ThreadMsg{me("did you get a chance to look at the times?")}, wantFinalized: false},
	{name: "musing_not_agreeing", thread: []schedule.ThreadMsg{me("we could also just catch up at the offsite next month, whatever's easier")}, wantFinalized: false},
	{name: "unrelated_own_msg", thread: []schedule.ThreadMsg{me("btw sent you that article about the acquisition")}, wantFinalized: false},
}
