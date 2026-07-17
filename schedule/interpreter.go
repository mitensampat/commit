package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// LLMInterpreter implements ReplyInterpreter with a Claude call. Interpretation
// runs on a haiku-class model to match production cost; the prompt is tuned by
// the eval corpus in evals/schedule.
type LLMInterpreter struct {
	Creds Creds
}

func (li *LLMInterpreter) InterpretReply(ctx context.Context, rc ReplyContext) (*Interpretation, error) {
	prompt := buildReplyPrompt(rc)
	raw, err := callClaude(ctx, li.Creds.APIKey(), li.Creds.Model(), prompt, 512)
	if err != nil {
		return nil, err
	}
	var interp Interpretation
	if err := json.Unmarshal([]byte(extractJSON(raw)), &interp); err != nil {
		return nil, fmt.Errorf("parse interpretation: %w (raw: %s)", err, raw)
	}
	// Defensive normalization — anything malformed degrades to ambiguous/low,
	// never to a booking.
	switch interp.Intent {
	case ReplyAccept, ReplyReject, ReplyCounter, ReplyAmbiguous, ReplyUnrelated,
		ReplySoftYes, ReplyDeference, ReplyScopeChange, ReplyDirective, ReplyNotScheduling:
	default:
		interp.Intent = ReplyAmbiguous
		interp.Confidence = "low"
	}
	if interp.Confidence != "high" {
		interp.Confidence = "low"
	}
	if interp.SlotIndex < 0 || interp.SlotIndex > len(rc.Slots) {
		interp.Intent = ReplyAmbiguous
		interp.SlotIndex = 0
		interp.Confidence = "low"
	}
	// Normalize the timestamp to real RFC3339 before anything downstream tries
	// to parse it. Models intermittently drop the zone offset
	// ("2026-07-17T17:30:00"), and a strict RFC3339 parse then fails at BOOK
	// time — the session silently refuses to complete. A naive timestamp means
	// the user's timezone, which is the one the prompt asked for.
	if interp.CounterTime != "" {
		loc := rc.Location
		if loc == nil {
			loc = time.Local
		}
		if t, ok := parseFlexibleTime(interp.CounterTime, loc); ok {
			interp.CounterTime = t.Format(time.RFC3339)
		} else {
			// A time we cannot pin down is not something to act on.
			if interp.Intent == ReplyCounter || interp.Intent == ReplyDirective {
				interp.Intent = ReplyAmbiguous
				interp.Confidence = "low"
			}
			interp.CounterTime = ""
		}
	}
	// A scope_change that changed nothing we can act on is not a scope change.
	if interp.Intent == ReplyScopeChange && interp.NewDurationMin == 0 && interp.NewFormat == "" && !interp.NeedsVenue {
		interp.Intent = ReplyAmbiguous
		interp.Confidence = "low"
	}
	// A directive with no time to check is just ambiguity.
	if interp.Intent == ReplyDirective && interp.CounterTime == "" {
		interp.Intent = ReplyAmbiguous
		interp.Confidence = "low"
	}
	// A "counter" or "directive" naming a time we ALREADY offered is not a
	// counter at all — it's an acceptance of that option. The model reads the
	// clock right far more reliably than it reads the pragmatics, so settle
	// this on the timestamps rather than on the prose.
	if interp.Intent == ReplyCounter || interp.Intent == ReplyDirective {
		if t, err := time.Parse(time.RFC3339, interp.CounterTime); err == nil {
			for i, sl := range rc.Slots {
				if sl.Start.Equal(t) {
					interp.Intent = ReplyAccept
					interp.SlotIndex = i + 1
					interp.CounterTime = ""
					break
				}
			}
		}
	}
	// Per-intent field hygiene: an accept carries only a slot index, a counter
	// only a time. Stray extra fields would make two equivalent readings look
	// different to SameOutcome and wedge the correction-race gate.
	switch interp.Intent {
	case ReplyAccept, ReplySoftYes:
		interp.CounterTime = ""
	case ReplyCounter, ReplyDirective:
		interp.SlotIndex = 0
	case ReplyDeference:
		interp.SlotIndex = 0
		interp.CounterTime = ""
		// Drop out-of-range subset entries rather than letting them pick.
		var keep []int
		for _, i := range interp.DeferSlots {
			if i >= 1 && i <= len(rc.Slots) {
				keep = append(keep, i)
			}
		}
		interp.DeferSlots = keep
	case ReplyScopeChange:
		interp.SlotIndex = 0
		interp.CounterTime = ""
	default:
		interp.SlotIndex = 0
		interp.CounterTime = ""
	}
	// Scope fields only mean anything on a scope_change; stray values would
	// mutate the session from an unrelated intent.
	if interp.Intent != ReplyScopeChange {
		interp.NewDurationMin = 0
		interp.NewFormat = ""
		interp.NeedsVenue = false
		interp.RequestedPlatform = ""
	}
	if interp.Intent != ReplyNotScheduling {
		interp.WrongPerson = false
	}
	if interp.Intent != ReplyDeference {
		interp.DeferSlots = nil
	}
	return &interp, nil
}

func (li *LLMInterpreter) InterpretOwnMessage(ctx context.Context, rc ReplyContext) (bool, error) {
	prompt := buildOwnMessagePrompt(rc)
	raw, err := callClaude(ctx, li.Creds.APIKey(), li.Creds.Model(), prompt, 256)
	if err != nil {
		return false, err
	}
	var out struct {
		Finalized bool `json:"finalized"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &out); err != nil {
		return false, fmt.Errorf("parse own-message result: %w", err)
	}
	return out.Finalized, nil
}

// parseFlexibleTime reads a model-emitted timestamp. RFC3339 is what we ask
// for, but a zone-less timestamp is a common and harmless slip: it means the
// user's timezone. Anything else is a genuine failure to pin down a time.
func parseFlexibleTime(s string, loc *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	// Zone-less layouts, interpreted in the user's timezone.
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func formatSlotLine(i int, s Slot, loc *time.Location) string {
	st := s.Start.In(loc)
	en := s.End.In(loc)
	return fmt.Sprintf("%d. %s, %s – %s", i+1,
		st.Format("Mon Jan 2"), st.Format("3:04 PM"), en.Format("3:04 PM"))
}

func buildReplyPrompt(rc ReplyContext) string {
	loc := rc.Location
	if loc == nil {
		loc = time.Local
	}
	var sb strings.Builder
	sb.WriteString(`You are the reply-interpretation module of a careful scheduling assistant. The user (ME) sent a message to a contact (THEM) proposing meeting times. Your ONLY job: classify the CURRENT scheduling position of THEM from the thread below.

Return STRICT JSON, nothing else:
{"intent": "accept|soft_yes|deference|reject|counter_propose|scope_change|directive|not_scheduling|ambiguous|unrelated", "slot_index": 0, "counter_time": "", "side_note": "", "confidence": "high|low", "defer_slots": [], "new_duration_min": 0, "new_format": "", "needs_venue": false, "requested_platform": "", "wrong_person": false}

Governing principle: never guess what they meant, but don't be fussy about what's obvious.

Rules, in priority order:
1. The LATEST position wins. If they accepted and then changed their mind ("actually Tuesday is bad"), report the later position, not the acceptance.

2. "accept": they FIRMLY agreed to one of the OFFERED OPTIONS, with no hedge. slot_index = which one (1-based). Informal acceptance counts ("👍", "works for me", "the second one", "tue is good") as long as the referent is unmistakable. If exactly ONE option was offered, a plain agreement means slot_index 1.

3. "soft_yes": they pointed at ONE specific option but HEDGED — the commitment isn't firm yet. slot_index = the option they pointed at. THIS IS NOT ACCEPT — the difference is the hedge, not the topic. Booking a soft yes is a serious error: they have not committed.
   Hedge markers, ANY of which forces soft_yes over accept:
     - modal/probabilistic verbs: "should work", "should be fine", "would work", "could work", "might work", "probably", "likely", "I think", "pretty sure", "I'd say"
     - provisional language: "tentatively", "pencil me in", "provisionally", "for now", "let's say"
     - a pending confirmation: "let me confirm", "I'll confirm", "need to check with X first", "don't lock it yet", "subject to X", "as long as X", "if nothing comes up"
   Note carefully: "wed works" is ACCEPT (firm). "wed should work" is SOFT_YES ("should" is a hedge). "wed works, let me just confirm tomorrow" is SOFT_YES. The single word is the whole difference — read it precisely.
   - A hedge that points at NO single option ("maybe", "probably tue or thu", "let me check and get back") is "ambiguous", NOT soft_yes. soft_yes REQUIRES one unmistakable slot.
   - If they named ONE option and LATER hedged it ("thursday!" … "actually let me check first, don't lock it yet"), that is soft_yes at that option — the hedge is the latest position and it still points somewhere.

4. "deference": they explicitly hand the choice to ME, or say every option works. Markers: "you pick", "any of these work", "whatever suits you", "whenever's easiest", "I'm flexible", "all of them work", "dealer's choice". Set defer_slots to the 1-based options they limited it to when they deferred WITHIN a subset ("Tue or Wed both fine, you choose" → [1,2]); leave defer_slots empty when any offered option is fine.
   - "sounds good" / "ok" / "👍" over MULTIPLE options is NOT deference — that's vague agreement to the idea, not a statement that every option works, nor a handover of the choice. Use "ambiguous".

5. THE COUNT OF OFFERED OPTIONS DECIDES WHAT "sounds good" MEANS. Check how many options are listed under OFFERED OPTIONS before you classify any plain agreement:
   - Exactly ONE option offered → a plain, unhedged agreement ("sounds good", "👍", "ok", "works", "perfect", "great") is "accept", slot_index 1, confidence "high". There is only one thing they could mean, so there is NOTHING TO GUESS and "ambiguous" is WRONG. Being coy here is a failure, not caution.
   - MORE THAN ONE option offered → the same words are "ambiguous" (which of them?). NEVER guess a slot.
   This rule is about ambiguity of REFERENT only. A hedge is still soft_yes under rule 3 regardless of the option count ("should work" with one option offered → soft_yes, slot_index 1).

6. If they suggest an alternative that actually MATCHES one of the OFFERED OPTIONS — the day matches AND the clock time is either the option's time or unspecified (e.g. "wed instead?" when Wednesday 11am was offered) — that is "accept" of that option, not a counter. But if they name a time that DIFFERS from the offered option on that day (e.g. "same time wednesday?" meaning another slot's time, or "wed at 2?" when Wed 11am was offered), that is "counter_propose", NOT an accept.

7. "counter_propose": they NEGOTIATE a specific alternative day+time that was NOT offered — they are asking, not telling ("can we do Tue 5 instead?", "how about friday 10am?", "could we push tue to 4:30?"). Set counter_time to your best RFC3339 timestamp in the user's timezone shown below (use the offered meeting duration; pick the NEXT future occurrence of that day). If their suggestion has no concrete day+time ("early next week better", "some morning?"), use "ambiguous" with counter_time "".

8. "directive": they INSTRUCT rather than negotiate — an IMPERATIVE naming a time, with no asking. Set counter_time the same way as counter_propose. If the instruction names no concrete day+time ("call me later"), use "ambiguous".
   The tell is grammatical MOOD, not seniority or politeness. A directive is a command: the sentence starts with a bare verb aimed at ME and contains no request frame.
     - Imperative openers that signal a directive: "call me…", "ring me…", "give me a ring…", "give me a call…", "come by…", "swing by…", "dial me…", "phone me…", "meet me…", "join me…", "be there…". These are directives even when they end with a friendly word, and even when they name a weekday ("ring me thursday at 11", "call me wednesday at 6pm" → BOTH directive).
     - Request frames that signal counter_propose instead: "can we…", "could we…", "how about…", "what about…", "would … work", "does … work", "shall we…", "any chance…", "…instead?", or any yes/no question about the time.
   Contrast, memorize this pair: "can we do 5?" ASKS → counter_propose. "call me at 5" TELLS → directive. Same time, different mood.
   A trailing "?" does NOT by itself make something a request if the sentence is an imperative ("call me at 5?" is still a directive).

9. "scope_change": they change the SHAPE of the meeting rather than the time — duration, format, or venue. A scope change does NOT need a request frame: a bare statement of a different shape ("15 mins is plenty", "an hour would be better", "coffee works better than a call") IS a scope_change, not an ambiguous remark. If you can name the new duration or format, classify it as scope_change with high confidence rather than parking it in side_note.
   - new_duration_min: minutes if they indicated a different length, however phrased — "can we make it an hour?" → 60; "15 mins is plenty tbh, no need for half an hour" → 15; "let's keep it to 20" → 20. Else 0.
   - new_format: "call" | "video" | "in-person" if they changed it, else "". "just call me, no need for a video thing" → "call". "let's grab coffee instead" → "in-person". "send a zoom link" → "video".
   - needs_venue: true when the new shape needs a place and nobody has named one ("let's grab coffee instead" → true).
   - requested_platform: a video tool they named — "zoom", "teams", "meet" — else "".
   - A scope change may also carry a time preference; still classify as scope_change (the shape has to be settled first).
   - "just call me, no need for a meeting" is a scope_change to a call — NOT not_scheduling. They still want to talk.

10. "not_scheduling": this has stopped being a conversation we can schedule.
   - They move it elsewhere: "let's just do this on email", "my assistant will set it up", "I'll send an invite myself".
   - Or we have the wrong person: "who is this?", "wrong number", "I think you have the wrong Sam". Set wrong_person true for these ONLY.
   - Do NOT use this for banter or silence — that's "unrelated".

11. "reject": they declined and offered no alternative ("can't this week, sorry").

12. "ambiguous": unclear, conditional, vague, or mixed signals with no single readable position.

13. "unrelated": system placeholders like "[Voice call]" or media markers with no text are "unrelated" or "ambiguous" depending on context; otherwise "unrelated" means their messages don't engage with the scheduling question at all — banter, reactions, social chatter. They simply haven't answered yet, so we keep waiting.

14. side_note: any non-scheduling request or important info in their reply worth relaying to the user (e.g. "also send the deck"), else "". A side_note can coexist with any intent.

15. confidence: "high" only when a careful human assistant would act on this without double-checking. Anything uncertain: "low". When unsure between intents, prefer "ambiguous" with "low".
   - EXCEPTION A: when torn between "accept" and "soft_yes", always choose "soft_yes" — the cost of holding a firm yes is a question; the cost of booking a soft yes is a meeting they never agreed to.
   - EXCEPTION B: this caution does NOT apply when exactly ONE option was offered and they plainly agreed to it. That is an unhedged "accept" at high confidence (rule 5). Retreating to "ambiguous" there is not caution — it is fussiness, and it is wrong.

16. Ignore messages from ME when judging THEIR position; they are context only.

Worked examples (assume Tue 3pm / Wed 11am / Thu 4pm offered unless noted):
- Only ONE option (Tue 3pm) was offered; THEM: "sounds good" → {"intent":"accept","slot_index":1,"confidence":"high"} — one option, plain agreement, nothing to guess.
- Three options offered; THEM: "sounds good" → {"intent":"ambiguous","slot_index":0,"confidence":"low"} — which of the three?
- THEM: "Tue works" → {"intent":"accept","slot_index":1,"confidence":"high"} — firm, one referent.
- THEM: "Tue works, let me just confirm tomorrow" → {"intent":"soft_yes","slot_index":1,"confidence":"high"} — same referent, but "let me confirm" is a hedge. Never accept.
- THEM: "wed should work" → {"intent":"soft_yes","slot_index":2,"confidence":"high"} — "should work" is a hedge, NOT "works". This one is easy to get wrong: compare "wed works" (accept) with "wed should work" (soft_yes).
- THEM: "probably wed" → {"intent":"soft_yes","slot_index":2,"confidence":"high"} — "probably" hedges a single option.
- THEM: "thursday!" then later "hmm actually let me check my calendar first, don't lock it yet" → {"intent":"soft_yes","slot_index":3,"confidence":"high"} — latest position is a hedge aimed at Thursday.
- THEM: "probably tue or thu" → {"intent":"ambiguous","slot_index":0,"confidence":"low"} — hedged AND two referents.
- THEM: "any of these work, you pick" → {"intent":"deference","defer_slots":[],"confidence":"high"}.
- THEM: "tue or wed both fine, you choose" → {"intent":"deference","defer_slots":[1,2],"confidence":"high"}.
- THEM: "can we make it an hour?" → {"intent":"scope_change","new_duration_min":60,"confidence":"high"}.
- THEM: "15 mins is plenty tbh, no need for half an hour" → {"intent":"scope_change","new_duration_min":15,"confidence":"high"} — a bare statement of a new length is still a scope_change, not "ambiguous".
- THEM: "let's grab coffee instead" → {"intent":"scope_change","new_format":"in-person","needs_venue":true,"confidence":"high"}.
- THEM: "send a zoom link" → {"intent":"scope_change","new_format":"video","requested_platform":"zoom","confidence":"high"}.
- THEM: "call me at 5" → {"intent":"directive","counter_time":"<today or next day 17:00 RFC3339>","confidence":"high"} — an instruction, not a request.
- THEM: "ring me thursday at 11" → {"intent":"directive","counter_time":"<Thursday 11:00 RFC3339>","confidence":"high"} — imperative "ring me"; naming a weekday does NOT make it a request.
- THEM: "give me a ring friday 9am" → {"intent":"directive","counter_time":"<Friday 09:00 RFC3339>","confidence":"high"}.
- THEM: "can we do tue 5 instead?" → {"intent":"counter_propose","counter_time":"<next Tuesday 17:00 RFC3339>","confidence":"high"} — asking, not telling.
- THEM: "my assistant will set it up" → {"intent":"not_scheduling","wrong_person":false,"confidence":"high"}.
- THEM: "who is this?" → {"intent":"not_scheduling","wrong_person":true,"confidence":"high"}.
- THEM: "haha that meme was great 😂" → {"intent":"unrelated","slot_index":0,"confidence":"high"} — banter/reactions/social chatter that ignores the scheduling question is "unrelated", not "ambiguous".

`)
	now := rc.Now
	if now.IsZero() {
		now = time.Now()
	}
	sb.WriteString(fmt.Sprintf("Current date/time: %s\nUser timezone: %s\nContact: %s\n", now.In(loc).Format("Monday, Jan 2 2006, 3:04 PM MST"), loc.String(), rc.ContactName))
	// Calendar hint: models are bad at weekday→date arithmetic; spell out the
	// next two weeks so counter_time dates land on the right day.
	sb.WriteString("Upcoming days: ")
	for i := 0; i < 14; i++ {
		d := now.In(loc).AddDate(0, 0, i)
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(d.Format("Mon=Jan 2"))
	}
	sb.WriteString("\n\n")
	sb.WriteString("OFFERED OPTIONS:\n")
	for i, s := range rc.Slots {
		sb.WriteString(formatSlotLine(i, s, loc) + "\n")
	}
	sb.WriteString("\nMESSAGE ME SENT:\n" + rc.Draft + "\n\nTHREAD SINCE THEN (oldest first):\n")
	for _, m := range rc.Thread {
		who := "THEM"
		if m.FromMe {
			who = "ME"
		}
		ts := ""
		if !m.Time.IsZero() {
			ts = " [" + m.Time.In(loc).Format("Jan 2 3:04 PM") + "]"
		}
		sb.WriteString(fmt.Sprintf("%s%s: %s\n", who, ts, m.Text))
	}
	sb.WriteString("\nJSON:")
	return sb.String()
}

func buildOwnMessagePrompt(rc ReplyContext) string {
	loc := rc.Location
	if loc == nil {
		loc = time.Local
	}
	var sb strings.Builder
	sb.WriteString(`You watch a chat thread for a scheduling assistant. The user (ME) had asked the assistant to schedule a meeting with a contact (THEM), but may have just settled it MANUALLY by texting the contact directly.

Question: judging by ME's latest messages in the thread below, have ME and THEM already finalized a meeting time between themselves (e.g. "ok see you tuesday 3pm", "locked, sending an invite")? Merely discussing, asking, or proposing options does NOT count. But if ME unilaterally DECLARES a specific settled time and commits to it ("locking tomorrow 5pm", "sending you an invite for wed 11", "see you tuesday at 3"), that DOES count as finalized even without an explicit acknowledgment from THEM.

Return STRICT JSON: {"finalized": true|false}

`)
	sb.WriteString(fmt.Sprintf("Contact: %s\n\nTHREAD (oldest first):\n", rc.ContactName))
	for _, m := range rc.Thread {
		who := "THEM"
		if m.FromMe {
			who = "ME"
		}
		sb.WriteString(fmt.Sprintf("%s: %s\n", who, m.Text))
	}
	sb.WriteString("\nJSON:")
	return sb.String()
}
