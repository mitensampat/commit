package schedule

import (
	"strings"
	"time"
)

// WindowRange converts a freeform window phrase into a [from, to) search
// horizon. Unknown phrases fall back to the default horizon (tomorrow →
// +7 days), which keeps behavior safe: worst case we offer reasonable
// near-term slots.
func WindowRange(window string, now time.Time, loc *time.Location) (time.Time, time.Time) {
	if loc == nil {
		loc = time.Local
	}
	n := now.In(loc)
	startOfDay := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	}
	w := strings.ToLower(strings.TrimSpace(window))

	switch {
	case w == "today":
		return n, startOfDay(n).AddDate(0, 0, 1)
	case w == "tomorrow" || w == "tmrw":
		d := startOfDay(n).AddDate(0, 0, 1)
		return d, d.AddDate(0, 0, 1)
	case strings.Contains(w, "next week"):
		// Next Monday → following Monday.
		d := startOfDay(n)
		for d.Weekday() != time.Monday || !d.After(startOfDay(n)) {
			d = d.AddDate(0, 0, 1)
		}
		return d, d.AddDate(0, 0, 7)
	case strings.Contains(w, "this week") || w == "week":
		// Now → end of Sunday.
		d := startOfDay(n)
		for d.Weekday() != time.Sunday {
			d = d.AddDate(0, 0, 1)
		}
		return n, d.AddDate(0, 0, 1)
	case strings.Contains(w, "weekend"):
		d := startOfDay(n)
		for d.Weekday() != time.Saturday {
			d = d.AddDate(0, 0, 1)
		}
		return d, d.AddDate(0, 0, 2)
	case strings.Contains(w, "next month"):
		d := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
		return d, d.AddDate(0, 0, 14)
	}

	// Single weekday name → the next occurrence of that day.
	weekdays := map[string]time.Weekday{
		"mon": time.Monday, "monday": time.Monday,
		"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
		"wed": time.Wednesday, "wednesday": time.Wednesday,
		"thu": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
		"fri": time.Friday, "friday": time.Friday,
		"sat": time.Saturday, "saturday": time.Saturday,
		"sun": time.Sunday, "sunday": time.Sunday,
	}
	for word, wd := range weekdays {
		if w == word || strings.HasPrefix(w, word+" ") || strings.HasSuffix(w, " "+word) {
			d := startOfDay(n).AddDate(0, 0, 1)
			for d.Weekday() != wd {
				d = d.AddDate(0, 0, 1)
			}
			return d, d.AddDate(0, 0, 1)
		}
	}

	// Default: tomorrow through +7 days.
	return startOfDay(n).AddDate(0, 0, 1), startOfDay(n).AddDate(0, 0, 8)
}
