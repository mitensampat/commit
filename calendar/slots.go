// Package calendar implements Google Calendar access (OAuth2 authorization-code
// flow with a loopback redirect, REST API client) and pure slot computation.
// The slot math has no network dependencies and is unit-tested with synthetic
// fixtures.
package calendar

import (
	"sort"
	"strings"
	"time"
)

// Interval is a half-open busy interval [Start, End).
type Interval struct {
	Start, End time.Time
}

// Event is the subset of a Google Calendar event that busy computation needs.
type Event struct {
	ID           string
	Summary      string
	Start, End   time.Time
	AllDay       bool
	Transparency string // "opaque" (busy, default) or "transparent" (free)
	EventType    string // "default", "outOfOffice", "focusTime", "workingLocation", "birthday", ...
	Status       string // "confirmed", "tentative", "cancelled"
}

// BusyIntervals converts events to busy intervals, honoring hardening req 4:
//   - cancelled events are never busy
//   - transparent ("free"-marked) events are never busy — this is how Google
//     marks default all-day events, so free all-day events are ignored
//   - opaque all-day events and outOfOffice events ARE busy
//   - workingLocation and birthday pseudo-events are never busy
//   - events whose title contains an ignore word ("block", "hold", per
//     settings) are never busy
func BusyIntervals(events []Event, ignoreTitles []string) []Interval {
	var out []Interval
	for _, e := range events {
		if e.Status == "cancelled" {
			continue
		}
		if e.EventType == "workingLocation" || e.EventType == "birthday" {
			continue
		}
		// outOfOffice is always busy regardless of transparency.
		if e.EventType != "outOfOffice" {
			if e.Transparency == "transparent" {
				continue
			}
			if titleIgnored(e.Summary, ignoreTitles) {
				continue
			}
		}
		if !e.End.After(e.Start) {
			continue
		}
		out = append(out, Interval{Start: e.Start, End: e.End})
	}
	return MergeIntervals(out)
}

func titleIgnored(summary string, ignoreTitles []string) bool {
	s := strings.ToLower(summary)
	for _, ig := range ignoreTitles {
		ig = strings.ToLower(strings.TrimSpace(ig))
		if ig != "" && strings.Contains(s, ig) {
			return true
		}
	}
	return false
}

// MergeIntervals sorts and merges overlapping/adjacent intervals.
func MergeIntervals(in []Interval) []Interval {
	if len(in) == 0 {
		return nil
	}
	sorted := make([]Interval, len(in))
	copy(sorted, in)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start.Before(sorted[j].Start) })
	out := []Interval{sorted[0]}
	for _, iv := range sorted[1:] {
		last := &out[len(out)-1]
		if !iv.Start.After(last.End) {
			if iv.End.After(last.End) {
				last.End = iv.End
			}
		} else {
			out = append(out, iv)
		}
	}
	return out
}

// Prefs are the user's scheduling preferences for slot computation.
type Prefs struct {
	DayStartMin int // meeting hours: minutes from midnight, local
	DayEndMin   int
	Workdays    map[time.Weekday]bool
	Buffer      time.Duration // travel buffer, applied around busy blocks (in-person)
	Location    *time.Location
}

// Slot is a proposable meeting time.
type Slot struct {
	Start, End time.Time
	// Adjacent marks a slot that touches an existing meeting (preferred: it
	// keeps large free blocks intact).
	Adjacent bool
}

// IsFree reports whether [start, start+dur) overlaps no busy interval.
func IsFree(busy []Interval, start time.Time, dur time.Duration) bool {
	end := start.Add(dur)
	for _, b := range busy {
		if start.Before(b.End) && b.Start.Before(end) {
			return false
		}
	}
	return true
}

// ComputeSlots proposes up to `want` meeting slots between from and to,
// honoring meeting hours, workdays, travel buffer, and slot aesthetics:
// slots adjacent to existing meetings beat slots that split a large free
// block, and the picks are spread across at least two days when possible.
func ComputeSlots(busy []Interval, from, to time.Time, dur time.Duration, prefs Prefs, want int) []Slot {
	if want <= 0 {
		want = 3
	}
	loc := prefs.Location
	if loc == nil {
		loc = time.Local
	}
	if prefs.Buffer > 0 {
		var buffered []Interval
		for _, b := range busy {
			buffered = append(buffered, Interval{Start: b.Start.Add(-prefs.Buffer), End: b.End.Add(prefs.Buffer)})
		}
		busy = MergeIntervals(buffered)
	}

	type candidate struct {
		slot  Slot
		score float64
		day   string
	}
	var cands []candidate

	day := time.Date(from.In(loc).Year(), from.In(loc).Month(), from.In(loc).Day(), 0, 0, 0, 0, loc)
	for ; day.Before(to); day = day.AddDate(0, 0, 1) {
		if prefs.Workdays != nil && !prefs.Workdays[day.Weekday()] {
			continue
		}
		dayStart := day.Add(time.Duration(prefs.DayStartMin) * time.Minute)
		dayEnd := day.Add(time.Duration(prefs.DayEndMin) * time.Minute)
		if dayStart.Before(from) {
			dayStart = from
		}
		if dayEnd.After(to) {
			dayEnd = to
		}
		if !dayEnd.After(dayStart) {
			continue
		}

		// Free blocks within the day window.
		blocks := freeBlocks(busy, dayStart, dayEnd)
		for _, blk := range blocks {
			blockLen := blk.End.Sub(blk.Start)
			if blockLen < dur {
				continue
			}
			// Candidate at the block start (adjacent to the meeting that ends
			// there, or to the start of the meeting-hours window).
			startAdj := blockBoundedByBusy(busy, blk.Start, true)
			endAdj := blockBoundedByBusy(busy, blk.End, false)

			addCand := func(s time.Time, adjacent bool) {
				s = roundUpQuarter(s)
				if s.Add(dur).After(blk.End) || s.Before(blk.Start) {
					return
				}
				score := 0.0
				if adjacent {
					score += 10 // adjacency beats splitting a large free block
				}
				// Earlier in the horizon is mildly better.
				score -= s.Sub(from).Hours() * 0.1
				// Splitting penalty: a mid-block slot fragments free time.
				cands = append(cands, candidate{
					slot:  Slot{Start: s, End: s.Add(dur), Adjacent: adjacent},
					score: score,
					day:   day.Format("2006-01-02"),
				})
			}
			addCand(blk.Start, startAdj)
			if blockLen >= 2*dur {
				addCand(blk.End.Add(-dur), endAdj)
			}
		}
	}

	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })

	// Pick greedily with a spread constraint: at most 2 per day until we have
	// options on ≥2 days (when the horizon allows it).
	var picked []Slot
	perDay := map[string]int{}
	days := map[string]bool{}
	overlaps := func(s Slot) bool {
		for _, p := range picked {
			if s.Start.Before(p.End) && p.Start.Before(s.End) {
				return true
			}
		}
		return false
	}
	// First pass: max 1 per day to force spread.
	for _, c := range cands {
		if len(picked) >= want {
			break
		}
		if perDay[c.day] >= 1 || overlaps(c.slot) {
			continue
		}
		picked = append(picked, c.slot)
		perDay[c.day]++
		days[c.day] = true
	}
	// Second pass: fill remaining from best candidates regardless of day.
	for _, c := range cands {
		if len(picked) >= want {
			break
		}
		if overlaps(c.slot) {
			continue
		}
		picked = append(picked, c.slot)
		perDay[c.day]++
	}

	sort.Slice(picked, func(i, j int) bool { return picked[i].Start.Before(picked[j].Start) })
	return picked
}

// freeBlocks returns the gaps between busy intervals inside [start, end).
func freeBlocks(busy []Interval, start, end time.Time) []Interval {
	var out []Interval
	cur := start
	for _, b := range busy {
		if !b.End.After(cur) || !b.Start.Before(end) {
			continue
		}
		if b.Start.After(cur) {
			out = append(out, Interval{Start: cur, End: minTime(b.Start, end)})
		}
		if b.End.After(cur) {
			cur = b.End
		}
		if !cur.Before(end) {
			return out
		}
	}
	if cur.Before(end) {
		out = append(out, Interval{Start: cur, End: end})
	}
	return out
}

// blockBoundedByBusy reports whether the block edge at t touches a busy
// interval (true adjacency) rather than the day boundary.
func blockBoundedByBusy(busy []Interval, t time.Time, isStart bool) bool {
	for _, b := range busy {
		if isStart && b.End.Equal(t) {
			return true
		}
		if !isStart && b.Start.Equal(t) {
			return true
		}
	}
	return false
}

func roundUpQuarter(t time.Time) time.Time {
	r := t.Truncate(15 * time.Minute)
	if r.Before(t) {
		r = r.Add(15 * time.Minute)
	}
	return r
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
