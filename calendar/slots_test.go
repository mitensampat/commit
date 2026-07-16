package calendar

import (
	"testing"
	"time"
)

var loc = time.FixedZone("IST", 5*3600+1800)

func d(day, hour, min int) time.Time {
	// Base week: Monday 2026-07-20.
	return time.Date(2026, 7, 20+day-1, hour, min, 0, 0, loc)
}

func defaultPrefs() Prefs {
	return Prefs{
		DayStartMin: 9 * 60,
		DayEndMin:   18 * 60,
		Workdays: map[time.Weekday]bool{
			time.Monday: true, time.Tuesday: true, time.Wednesday: true,
			time.Thursday: true, time.Friday: true,
		},
		Location: loc,
	}
}

func TestBusyIntervalsAllDayRules(t *testing.T) {
	events := []Event{
		// Free-marked all-day event (Google default) — must be ignored.
		{Summary: "Company anniversary", AllDay: true, Transparency: "transparent",
			Start: d(1, 0, 0), End: d(2, 0, 0)},
		// OOO all-day — must be busy even if transparency unset.
		{Summary: "OOO", AllDay: true, EventType: "outOfOffice",
			Start: d(2, 0, 0), End: d(3, 0, 0)},
		// Busy-marked all-day — must be busy.
		{Summary: "Offsite", AllDay: true, Transparency: "opaque",
			Start: d(3, 0, 0), End: d(4, 0, 0)},
		// workingLocation pseudo-event — never busy.
		{Summary: "Home", AllDay: true, EventType: "workingLocation", Transparency: "opaque",
			Start: d(4, 0, 0), End: d(5, 0, 0)},
		// Cancelled — never busy.
		{Summary: "Old sync", Status: "cancelled", Start: d(4, 10, 0), End: d(4, 11, 0)},
		// Title-ignored hold — never busy.
		{Summary: "HOLD: maybe travel", Start: d(4, 14, 0), End: d(4, 16, 0)},
		// Regular meeting — busy.
		{Summary: "1:1", Start: d(5, 10, 0), End: d(5, 11, 0)},
	}
	busy := BusyIntervals(events, []string{"block", "hold"})

	// OOO Tue and opaque-offsite Wed are adjacent, so they merge.
	if len(busy) != 2 {
		t.Fatalf("want 2 busy intervals (merged OOO+offsite days, 1:1), got %d: %+v", len(busy), busy)
	}
	if !busy[0].Start.Equal(d(2, 0, 0)) || !busy[0].End.Equal(d(4, 0, 0)) {
		t.Errorf("merged OOO+offsite days wrong: %+v", busy[0])
	}
	if !busy[1].Start.Equal(d(5, 10, 0)) {
		t.Errorf("1:1 wrong: %+v", busy[1])
	}
}

func TestComputeSlotsBasics(t *testing.T) {
	// Tue has meetings 10-11 and 14-15; Wed is free; Thu has 9-12.
	busy := MergeIntervals([]Interval{
		{d(2, 10, 0), d(2, 11, 0)},
		{d(2, 14, 0), d(2, 15, 0)},
		{d(4, 9, 0), d(4, 12, 0)},
	})
	slots := ComputeSlots(busy, d(2, 0, 0), d(6, 0, 0), 30*time.Minute, defaultPrefs(), 3)
	if len(slots) != 3 {
		t.Fatalf("want 3 slots, got %d: %+v", len(slots), slots)
	}
	days := map[string]bool{}
	for _, s := range slots {
		if s.End.Sub(s.Start) != 30*time.Minute {
			t.Errorf("slot duration wrong: %+v", s)
		}
		// Within meeting hours.
		h := s.Start.In(loc).Hour()
		if h < 9 || h >= 18 {
			t.Errorf("slot outside meeting hours: %+v", s)
		}
		// Never overlapping busy.
		if !IsFree(busy, s.Start, 30*time.Minute) {
			t.Errorf("slot overlaps busy: %+v", s)
		}
		days[s.Start.In(loc).Format("2006-01-02")] = true
	}
	if len(days) < 2 {
		t.Errorf("slots should spread across >=2 days, got %v", days)
	}
}

func TestComputeSlotsPrefersAdjacency(t *testing.T) {
	// One meeting Tue 10-11 in an otherwise free week: the Tuesday slot
	// should hug the meeting (11:00 or 9:30 edge), not sit mid-afternoon
	// splitting the free block.
	busy := []Interval{{d(2, 10, 0), d(2, 11, 0)}}
	slots := ComputeSlots(busy, d(2, 0, 0), d(4, 0, 0), 30*time.Minute, defaultPrefs(), 3)
	if len(slots) == 0 {
		t.Fatal("no slots")
	}
	foundAdjacent := false
	for _, s := range slots {
		if s.Start.Equal(d(2, 11, 0)) || s.End.Equal(d(2, 10, 0)) {
			foundAdjacent = true
		}
	}
	if !foundAdjacent {
		t.Errorf("expected a slot adjacent to the 10-11 meeting, got %+v", slots)
	}
}

func TestComputeSlotsRespectsWorkdaysAndFullDays(t *testing.T) {
	// Whole Tue blocked by opaque all-day event; want no Tue slots, no
	// weekend slots.
	busy := []Interval{{d(2, 0, 0), d(3, 0, 0)}}
	slots := ComputeSlots(busy, d(1, 0, 0), d(8, 0, 0), 60*time.Minute, defaultPrefs(), 3)
	for _, s := range slots {
		wd := s.Start.In(loc).Weekday()
		if wd == time.Saturday || wd == time.Sunday {
			t.Errorf("slot on weekend: %+v", s)
		}
		if s.Start.In(loc).Day() == 21 {
			t.Errorf("slot on fully-blocked Tuesday: %+v", s)
		}
	}
}

func TestComputeSlotsTravelBuffer(t *testing.T) {
	// In-person with 30m buffer: meeting Tue 10-11 means 9:30-11:30 is
	// effectively blocked.
	prefs := defaultPrefs()
	prefs.Buffer = 30 * time.Minute
	busy := []Interval{{d(2, 10, 0), d(2, 11, 0)}}
	slots := ComputeSlots(busy, d(2, 0, 0), d(3, 0, 0), 30*time.Minute, prefs, 3)
	for _, s := range slots {
		if s.Start.Before(d(2, 11, 30)) && s.End.After(d(2, 9, 30)) {
			t.Errorf("slot violates travel buffer: %+v", s)
		}
	}
}

func TestComputeSlotsNoRoom(t *testing.T) {
	// Day packed solid: no slots.
	busy := []Interval{{d(2, 9, 0), d(2, 18, 0)}}
	slots := ComputeSlots(busy, d(2, 0, 0), d(3, 0, 0), 30*time.Minute, defaultPrefs(), 3)
	if len(slots) != 0 {
		t.Errorf("expected no slots on a packed day, got %+v", slots)
	}
}

func TestIsFree(t *testing.T) {
	busy := []Interval{{d(2, 10, 0), d(2, 11, 0)}}
	if IsFree(busy, d(2, 10, 30), 30*time.Minute) {
		t.Error("overlapping start should not be free")
	}
	if IsFree(busy, d(2, 9, 45), 30*time.Minute) {
		t.Error("overlapping end should not be free")
	}
	if !IsFree(busy, d(2, 11, 0), 30*time.Minute) {
		t.Error("back-to-back after busy should be free")
	}
	if !IsFree(busy, d(2, 9, 30), 30*time.Minute) {
		t.Error("back-to-back before busy should be free")
	}
}

func TestMergeIntervals(t *testing.T) {
	merged := MergeIntervals([]Interval{
		{d(1, 10, 0), d(1, 11, 0)},
		{d(1, 10, 30), d(1, 12, 0)},
		{d(1, 14, 0), d(1, 15, 0)},
	})
	if len(merged) != 2 {
		t.Fatalf("want 2 merged intervals, got %d", len(merged))
	}
	if !merged[0].End.Equal(d(1, 12, 0)) {
		t.Errorf("merge wrong: %+v", merged[0])
	}
}
