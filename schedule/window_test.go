package schedule

import (
	"testing"
	"time"
)

// The bug this guards against: "next week, Tues/Wed preferred" resolved the
// date range but silently dropped the day preference, so we proposed Mon/Thu/Sat
// to someone who had asked for Tuesday or Wednesday.
func TestPreferredDays(t *testing.T) {
	cases := []struct {
		window string
		want   []time.Weekday
	}{
		{"next week, Tues/Wed preferred", []time.Weekday{time.Tuesday, time.Wednesday}},
		{"Tue or Wed", []time.Weekday{time.Tuesday, time.Wednesday}},
		{"tuesday, wednesday", []time.Weekday{time.Tuesday, time.Wednesday}},
		{"monday next week", []time.Weekday{time.Monday}},
		{"thurs/fri", []time.Weekday{time.Thursday, time.Friday}},
		{"Tuesdays preferred", []time.Weekday{time.Tuesday}},
		{"this week", nil},
		{"next week", nil},
		{"", nil},
	}
	for _, tc := range cases {
		t.Run(tc.window, func(t *testing.T) {
			got := PreferredDays(tc.window)
			if len(got) != len(tc.want) {
				t.Fatalf("PreferredDays(%q) = %v, want %v", tc.window, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("PreferredDays(%q) = %v, want %v", tc.window, got, tc.want)
				}
			}
		})
	}
}

// A day preference must not widen the search window it appears in.
func TestPreferredDaysWithWindowRange(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, loc) // a Thursday
	from, to := WindowRange("next week, Tues/Wed preferred", now, loc)
	if from.Weekday() != time.Monday {
		t.Fatalf("next week should start on Monday, got %s", from.Weekday())
	}
	if d := to.Sub(from); d != 7*24*time.Hour {
		t.Fatalf("next week should span 7 days, got %v", d)
	}
	days := PreferredDays("next week, Tues/Wed preferred")
	if len(days) != 2 {
		t.Fatalf("expected Tue+Wed inside the next-week window, got %v", days)
	}
}

func TestFormatDays(t *testing.T) {
	got := FormatDays([]time.Weekday{time.Tuesday, time.Wednesday})
	if got != "Tue/Wed" {
		t.Fatalf("FormatDays = %q, want Tue/Wed", got)
	}
}
