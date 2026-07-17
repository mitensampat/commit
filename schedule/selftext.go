package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ClassifySelfText reads the user's free text in their own self-chat while a
// draft is on the table and decides whether they are TELLING COMMIT something
// or WRITING THE MESSAGE.
//
// This exists because of a real failure: the user typed "he asked for Tue or
// Wed, our entire proposal is wrong" and Commit answered "Draft updated. It'll
// go out on your next 'propose'." — silently arming a note-to-self as the
// message to send. Anything short of a confident "this is the message" must
// not become the draft.
func (li *LLMInterpreter) ClassifySelfText(ctx context.Context, sc SelfTextContext) (*SelfTextClass, error) {
	prompt := buildSelfTextPrompt(sc)
	raw, err := callClaude(ctx, li.Creds.APIKey(), li.Creds.Model(), prompt, 512)
	if err != nil {
		return nil, err
	}
	var out SelfTextClass
	if err := json.Unmarshal([]byte(extractJSON(raw)), &out); err != nil {
		return nil, fmt.Errorf("parse self-text class: %w (raw: %s)", err, raw)
	}
	// Defensive normalization. Every failure mode lands on "unclear", which
	// asks — never on "draft", which arms.
	switch out.Kind {
	case SelfTextInstruction, SelfTextDraft, SelfTextNote, SelfTextUnclear:
	default:
		out.Kind = SelfTextUnclear
		out.Confidence = "low"
	}
	if out.Confidence != "high" {
		out.Confidence = "low"
	}
	// An "instruction" that names nothing to change is not actionable; asking
	// beats guessing at what they wanted.
	if out.Kind == SelfTextInstruction && !out.NeedsRecompute() && out.ToneNote == "" {
		out.Kind = SelfTextUnclear
		out.Confidence = "low"
	}
	if out.Kind != SelfTextInstruction {
		out.Window, out.DurationMin, out.Format, out.ToneNote = "", 0, "", ""
	}
	if out.DurationMin < 0 || out.DurationMin > 24*60 {
		out.DurationMin = 0
	}
	switch out.Format {
	case "call", "video", "in-person", "":
	default:
		out.Format = ""
	}
	return &out, nil
}

func buildSelfTextPrompt(sc SelfTextContext) string {
	loc := sc.Location
	if loc == nil {
		loc = time.Local
	}
	var sb strings.Builder
	sb.WriteString(`You are part of a scheduling assistant. The user has a DRAFT message on the table, waiting to be sent to a contact. The user just typed something in their OWN private self-chat (a notes-to-self chat only they can see).

Decide what that text IS:
- "instruction": they are telling YOU (the assistant) what to change — about the days, the window, the duration, the format, or how the message should read. Instructions usually talk ABOUT the contact in the third person ("he", "she", "they", or their name), or address you directly ("make it", "change it", "actually...").
- "draft": they have written the actual message they want SENT to the contact. Drafts talk TO the contact in the second person ("you"), or read as a complete WhatsApp message someone would send.
- "note": a personal note to themselves with NOTHING to do with this meeting ("buy milk, renew passport", "call the plumber", "idea: pitch deck rewrite"). This self-chat is also their notepad, so this is common and we simply ignore it in silence.
- "unclear": it IS about this meeting, but you cannot tell whether it's an instruction or the message text. This is a perfectly good answer — we will ask them.

Return STRICT JSON, nothing else:
{"kind": "instruction|draft|note|unclear", "window": "", "duration_min": 0, "format": "", "tone_note": "", "confidence": "high|low"}

For "instruction", fill ONLY the fields they actually changed:
- window: a new day/date window phrase, copied close to their words ("Tue or Wed", "next week", "this week", "Thursday"). Empty if they didn't change which days.
- duration_min: a new length in minutes ("actually 45 mins" → 45). 0 if unchanged.
- format: "call" | "video" | "in-person" if they changed it, else "".
- tone_note: how the message should READ differently ("warmer", "shorter", "less formal", "more direct"). Empty if they didn't ask for a wording change.
An instruction that changes nothing nameable is "unclear".

DECISION PROCEDURE — this is the whole task. Follow the steps IN ORDER. Do not extract any fields until you have answered step 1.

STEP 0 — IS THIS ABOUT THIS MEETING AT ALL? If the text has nothing to do with scheduling this meeting with this contact — errands, reminders, shopping lists, stray thoughts — answer "note" at high confidence and STOP. Do not stretch to find an instruction in it. The user is just using their notepad.

STEP 1 — WHO IS BEING SPOKEN TO? Read the text and ask: is this addressed to the CONTACT, or to ME (the assistant)?
  Signals it is addressed to the CONTACT → answer "draft" and STOP:
    - a greeting or vocative aimed at a person: "hey", "hi", "hi <name>", "mate", "man", "dude"
    - an apology or social softener aimed at a person: "sorry mate", "sorry for the delay", "apologies", "no worries", "hope you're well"
    - second person aimed at them: "you", "your", "does ... work for you", "let me know"
    - a first-person statement of the USER's own availability offered TO someone: "I'm free tue or wed", "I'm around thursday"
    - it reads end-to-end as a complete WhatsApp message you could send unedited
  Signals it is addressed to ME → go to step 2:
    - the contact referred to in the third person: "he", "she", "they", "him", "her", or their name used as a subject ("Akshay asked for...")
    - a command aimed at the assistant: "make it", "change it", "use", "only", "actually..."
    - a bare fragment that is not a sentence anyone would send ("45 mins", "warmer", "next week only")

STEP 2 — if it is addressed to ME and names something concrete to change → "instruction". Extract the fields.

STEP 3 — if the addressee is genuinely unclear, or nothing concrete is named to change → "unclear". We will simply ask them; that costs one question and is always safe.

THE TRAP TO AVOID: a message addressed to the contact will often MENTION days, durations, or formats — those are the very things being discussed. Extractable-looking details do NOT make it an instruction, and they do NOT make it "unclear" either. "sorry mate, can we do next week instead? I'm free tue or wed" is addressed to the CONTACT (apology + "can we" + "I'm free"), so it is a DRAFT with high confidence, even though "next week" and "tue or wed" look extractable. Step 1 outranks step 2 — always.
Mirror image: "he asked for Tue or Wed" mentions the same days but talks ABOUT him, so it is an instruction. The days are identical; the ADDRESSEE is what differs.

WHY THIS MATTERS, IN BOTH DIRECTIONS:
- Calling an instruction a "draft" SENDS the user's private note to the contact. That is the worst outcome; never do it on a guess.
- But calling a plainly-addressed message "unclear" is also a failure — it makes us look like we can't read. When step 1 gives a clear answer, COMMIT to it at high confidence. Hedging to "unclear" is only correct when the addressee is genuinely undecidable, not merely when the message happens to mention scheduling details.
- Rule of thumb: if you would be comfortable sending the text, exactly as written, to the contact right now, it is a draft. If sending it verbatim would embarrass the user, it is not.

Worked examples:
- "he asked for Tue or Wed, our entire proposal is wrong" → {"kind":"instruction","window":"Tue or Wed","confidence":"high"} — third person about the contact, and a complaint. Never a draft.
- "only next week please" → {"kind":"instruction","window":"next week","confidence":"high"}.
- "actually make it 45 mins" → {"kind":"instruction","duration_min":45,"confidence":"high"}.
- "let's make this a call not a video thing" → {"kind":"instruction","format":"call","confidence":"high"}.
- "make it warmer" → {"kind":"instruction","tone_note":"warmer","confidence":"high"}.
- "shorter please" → {"kind":"instruction","tone_note":"shorter","confidence":"high"}.
- "too formal, he's an old friend" → {"kind":"instruction","tone_note":"less formal, they're an old friend","confidence":"high"}.
- "she can only do mornings, and 30 mins is plenty" → {"kind":"instruction","window":"mornings","duration_min":30,"confidence":"high"}.
- "hey! sorry for the delay — any of these work for a quick call? 1. Tue 3pm 2. Wed 11am" → {"kind":"draft","confidence":"high"} — addressed to the contact, sendable as-is.
- "hi Sam, would love to catch up. does Thursday afternoon suit you?" → {"kind":"draft","confidence":"high"}.
- "sorry mate, this week's a write-off on my end — can we do next week instead? I'm free tue or wed" → {"kind":"draft","confidence":"high"} — "sorry mate" and "can we" are aimed at the CONTACT. It names days, but it is a message, not an instruction. Contrast with "he asked for Tue or Wed" above, which talks ABOUT him.
- "wrong" → {"kind":"unclear","confidence":"low"} — what's wrong? Ask.
- "tuesday" → {"kind":"unclear","confidence":"low"} — a new window, or the whole message? Ask.
- "buy milk, renew passport" → {"kind":"note","confidence":"high"} — a to-do list, nothing to do with the meeting. Ignore it silently.
- "call the plumber back" → {"kind":"note","confidence":"high"} — mentions "call", but it's an errand about a plumber, not this meeting's format.

`)
	now := sc.Now
	if now.IsZero() {
		now = time.Now()
	}
	sb.WriteString(fmt.Sprintf("Current date/time: %s\nContact the draft is addressed to: %s\n",
		now.In(loc).Format("Monday, Jan 2 2006, 3:04 PM MST"), sc.ContactName))
	sb.WriteString(fmt.Sprintf("Meeting so far: %s, %d min", sc.Topic, sc.DurationMin))
	if sc.Format != "" {
		sb.WriteString(", " + sc.Format)
	}
	sb.WriteString("\n\nOPTIONS CURRENTLY ON THE TABLE:\n")
	for i, s := range sc.Slots {
		sb.WriteString(formatSlotLine(i, s, loc) + "\n")
	}
	sb.WriteString("\nDRAFT CURRENTLY ON THE TABLE (this is what would be sent):\n" + sc.Draft + "\n")
	sb.WriteString("\nWHAT THE USER JUST TYPED IN THEIR SELF-CHAT:\n" + sc.Text + "\n")
	sb.WriteString("\nJSON:")
	return sb.String()
}
