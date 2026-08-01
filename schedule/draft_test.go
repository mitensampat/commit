package schedule

import (
	"strings"
	"testing"
	"time"
)

func tzSlots() []Slot {
	base := time.Date(2026, 8, 4, 20, 30, 0, 0, time.UTC) // arbitrary fixed day
	return []Slot{
		{Start: base, End: base.Add(30 * time.Minute)},
		{Start: base.Add(24 * time.Hour), End: base.Add(24*time.Hour + 30*time.Minute)},
	}
}

// The bug: with a cross-timezone contact, the model was told to give their
// time "too" and shown an example with only their clock — so it sometimes
// dropped the user's own time entirely. The prompt must now demand BOTH clocks
// and provide both labelled lists.
func TestDraftPromptDemandsBothTimezones(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	p := buildDraftPrompt(DraftRequest{
		ContactName:   "Daniel",
		Topic:         "partnership",
		Slots:         tzSlots(),
		Location:      ist,
		ContactTZ:     "America/Los_Angeles",
		ContactTZNote: "assuming SF from the +1 number",
	})

	// The instruction must forbid showing only one clock.
	for _, want := range []string{"BOTH clocks", "never instead", "USER's timezone", "CONTACT's timezone"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing the both-timezones guarantee %q", want)
		}
	}
	// Both timezone lists must actually be present (labelled), so the model
	// has real values to pair rather than doing arithmetic in its head.
	if strings.Count(p, "the SAME times") == 0 && !strings.Contains(p, "SAME times") {
		t.Fatal("prompt does not include the contact-timezone slot list")
	}
	// The user's own timezone list must lead.
	userIdx := strings.Index(p, "USER's timezone")
	contactIdx := strings.Index(p, "CONTACT's timezone")
	if userIdx == -1 || contactIdx == -1 || userIdx > contactIdx {
		t.Fatal("user's timezone list must be presented before the contact's")
	}
}

// Same-timezone contact: no cross-timezone scaffolding should appear at all,
// so ordinary drafts stay clean.
func TestDraftPromptSingleTimezone(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	p := buildDraftPrompt(DraftRequest{
		ContactName: "Priya",
		Topic:       "sync",
		Slots:       tzSlots(),
		Location:    ist,
		// no ContactTZ
	})
	if strings.Contains(p, "BOTH clocks") || strings.Contains(p, "CONTACT's timezone") {
		t.Fatal("single-timezone draft should carry no cross-timezone instructions")
	}
}
