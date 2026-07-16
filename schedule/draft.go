package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// InferredContext is what we read out of the recent thread with the contact
// to fill fields the command left unspecified.
type InferredContext struct {
	Topic       string `json:"topic"`
	DurationMin int    `json:"duration_min"`
	Format      string `json:"format"` // call | video | in-person | ""
	Window      string `json:"window"`
}

// InferContext reads the last few messages with the contact and infers
// topic / duration / format / window for anything the command didn't specify.
func InferContext(ctx context.Context, creds Creds, contactName string, thread []ThreadMsg, cmd *Command) (*InferredContext, error) {
	var sb strings.Builder
	sb.WriteString(`You help a scheduling assistant infer meeting context from a WhatsApp thread. The user wants to schedule a meeting with the contact below. From the recent messages, infer:
- topic: a short phrase for what the meeting is about (e.g. "CRED partnership follow-up"). If the thread gives no clue, use "catch-up".
- duration_min: sensible duration in minutes (30 default; 60 if it's clearly a deep work session or meal).
- format: "call", "video", or "in-person" if the thread suggests one, else "".
- window: a time window if the thread suggests one (e.g. "this week", "after the 20th"), else "".

Return STRICT JSON: {"topic": "", "duration_min": 30, "format": "", "window": ""}

`)
	sb.WriteString("Contact: " + contactName + "\n\nRECENT MESSAGES (oldest first):\n")
	for _, m := range thread {
		who := "THEM"
		if m.FromMe {
			who = "ME"
		}
		sb.WriteString(fmt.Sprintf("%s: %s\n", who, m.Text))
	}
	sb.WriteString("\nJSON:")

	raw, err := callClaude(ctx, creds.APIKey(), creds.Model(), sb.String(), 256)
	if err != nil {
		return nil, err
	}
	var ic InferredContext
	if err := json.Unmarshal([]byte(extractJSON(raw)), &ic); err != nil {
		return nil, fmt.Errorf("parse context: %w", err)
	}
	// Command-specified fields always win over inference.
	if cmd != nil {
		if cmd.DurationMin > 0 {
			ic.DurationMin = cmd.DurationMin
		}
		if cmd.Format != "" {
			ic.Format = cmd.Format
		}
		if cmd.Window != "" {
			ic.Window = cmd.Window
		}
	}
	if ic.DurationMin <= 0 {
		ic.DurationMin = 30
	}
	return &ic, nil
}

// DraftRequest carries everything needed to write the proposal message.
type DraftRequest struct {
	ContactName   string
	Topic         string
	Format        string
	Slots         []Slot
	Indices       []int // subset to include; empty = all
	MyStyle       string
	Location      *time.Location // user's TZ
	ContactTZ     string         // IANA or ""
	ContactTZNote string         // e.g. "assuming SF from the +1 number"
	StyleSamples  []string       // recent outbound messages, texting-style reference
	Cancel        bool           // graceful cancellation note instead of proposal
	BookedWhen    string         // for cancel/move notes
}

// SelectedSlots resolves the Indices subset.
func (dr *DraftRequest) SelectedSlots() []Slot {
	if len(dr.Indices) == 0 {
		return dr.Slots
	}
	var out []Slot
	for _, i := range dr.Indices {
		if i >= 1 && i <= len(dr.Slots) {
			out = append(out, dr.Slots[i-1])
		}
	}
	return out
}

// GenerateDraft writes the message to the counterpart in the user's texting
// style. Timezone assumptions must be stated, not hidden (hardening req 5).
func GenerateDraft(ctx context.Context, creds Creds, dr DraftRequest) (string, error) {
	loc := dr.Location
	if loc == nil {
		loc = time.Local
	}
	var sb strings.Builder
	sb.WriteString(`Write a short WhatsApp message from the user to the contact below. Match the user's texting style (see style notes and samples). No signatures, no "Dear", no bullet-point formality unless the samples do that. Keep it natural and brief.

`)
	if dr.Cancel {
		sb.WriteString("The message must gracefully cancel their planned meeting")
		if dr.BookedWhen != "" {
			sb.WriteString(" (" + dr.BookedWhen + ")")
		}
		sb.WriteString(", apologize briefly, and offer to rebook.\n")
	} else {
		sb.WriteString("The message must propose the meeting times below as a numbered list (1., 2., ...) and ask which works. Mention what the meeting is about in passing.\n")
		if dr.ContactTZ != "" && dr.ContactTZNote != "" {
			sb.WriteString("IMPORTANT: The contact may be in a different timezone (" + dr.ContactTZ + ", " + dr.ContactTZNote + "). Give the times in their timezone too, and explicitly state the assumption so they can correct it, e.g. \"that's 8am your side — assuming you're in SF?\".\n")
		}
	}
	sb.WriteString("\nContact: " + dr.ContactName + "\nTopic: " + dr.Topic + "\n")
	if dr.Format != "" {
		sb.WriteString("Format: " + dr.Format + "\n")
	}
	if !dr.Cancel {
		sb.WriteString("Times (user's timezone, " + loc.String() + "):\n")
		for i, s := range dr.SelectedSlots() {
			sb.WriteString(formatSlotLine(i, s, loc) + "\n")
		}
		if dr.ContactTZ != "" {
			if cloc, err := time.LoadLocation(dr.ContactTZ); err == nil {
				sb.WriteString("Same times in the contact's assumed timezone (" + dr.ContactTZ + "):\n")
				for i, s := range dr.SelectedSlots() {
					sb.WriteString(formatSlotLine(i, s, cloc) + "\n")
				}
			}
		}
	}
	if dr.MyStyle != "" {
		sb.WriteString("\nUser's style notes: " + dr.MyStyle + "\n")
	}
	if len(dr.StyleSamples) > 0 {
		sb.WriteString("\nRecent messages the user sent (style reference):\n")
		for _, s := range dr.StyleSamples {
			sb.WriteString("- " + s + "\n")
		}
	}
	sb.WriteString("\nReturn ONLY the message text, nothing else.")

	out, err := callClaude(ctx, creds.APIKey(), creds.Model(), sb.String(), 512)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// FormatSlotList renders the numbered options for the self-chat.
func FormatSlotList(slots []Slot, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	var sb strings.Builder
	for i, s := range slots {
		sb.WriteString(formatSlotLine(i, s, loc) + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// FormatSlotShort renders one slot compactly ("Tue 8, 3:00 PM").
func FormatSlotShort(s Slot, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	st := s.Start.In(loc)
	return st.Format("Mon 2, 3:04 PM")
}
