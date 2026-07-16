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
	case ReplyAccept, ReplyReject, ReplyCounter, ReplyAmbiguous, ReplyUnrelated:
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
{"intent": "accept|reject|counter_propose|ambiguous|unrelated", "slot_index": 0, "counter_time": "", "side_note": "", "confidence": "high|low"}

Rules, in priority order:
1. The LATEST position wins. If they accepted and then changed their mind ("actually Tuesday is bad"), report the later position, not the acceptance.
2. "accept": they clearly agreed to one of the OFFERED OPTIONS. slot_index = which one (1-based). Informal acceptance counts ("👍", "works for me", "the second one", "tue is good") as long as the referent is unmistakable. If exactly ONE option was offered, a plain agreement means slot_index 1.
3. If they agree but you cannot tell WHICH offered option they mean (e.g. "sounds good" over multiple options, "either works"), use "ambiguous". NEVER guess a slot.
4. "counter_propose": they suggested a specific alternative day+time that was NOT offered (e.g. "can we do Tue 5 instead?"). Set counter_time to your best RFC3339 timestamp in the user's timezone shown below (use the offered meeting duration; pick the NEXT future occurrence of that day). If their suggestion has no concrete day+time ("early next week better", "some morning?"), use "ambiguous" with counter_time "".
5. "reject": they declined and offered no alternative ("can't this week, sorry").
6. "ambiguous": unclear, conditional ("let me check and get back"), vague, or mixed signals.
7. "unrelated": their messages don't engage with the scheduling question at all.
8. side_note: any non-scheduling request or important info in their reply worth relaying to the user (e.g. "also send the deck"), else "". A side_note can coexist with any intent.
9. confidence: "high" only when a careful human assistant would act on this without double-checking. Anything uncertain: "low". When unsure between intents, prefer "ambiguous" with "low".
10. Ignore messages from ME when judging THEIR position; they are context only.

`)
	now := rc.Now
	if now.IsZero() {
		now = time.Now()
	}
	sb.WriteString(fmt.Sprintf("Current date/time: %s\nUser timezone: %s\nContact: %s\n\n", now.In(loc).Format("Monday, Jan 2 2006, 3:04 PM MST"), loc.String(), rc.ContactName))
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

Question: judging by ME's latest messages in the thread below, have ME and THEM already finalized a meeting time between themselves (e.g. "ok see you tuesday 3pm", "locked, sending an invite")? Merely discussing, asking, or proposing does NOT count — only a settled agreement.

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
