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

// weekdayWords maps every spelling we accept to a weekday.
var weekdayWords = map[string]time.Weekday{
	"mon": time.Monday, "monday": time.Monday, "mondays": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday, "tuesdays": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday, "wednesdays": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday, "thursdays": time.Thursday,
	"fri": time.Friday, "friday": time.Friday, "fridays": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday, "saturdays": time.Saturday,
	"sun": time.Sunday, "sunday": time.Sunday, "sundays": time.Sunday,
}

// PreferredDays pulls specific weekdays out of a window phrase — the counterpart
// saying "next week, Tues/Wed preferred" means the date range is next week AND
// only two days of it are wanted. Returns nil when no day is named.
func PreferredDays(window string) []time.Weekday {
	w := strings.ToLower(window)
	// Split on anything that isn't a letter so "tues/wed", "tue or wed" and
	// "tuesday, wednesday" all work.
	fields := strings.FieldsFunc(w, func(r rune) bool {
		return !(r >= 'a' && r <= 'z')
	})
	seen := map[time.Weekday]bool{}
	var out []time.Weekday
	for _, f := range fields {
		if wd, ok := weekdayWords[f]; ok && !seen[wd] {
			seen[wd] = true
			out = append(out, wd)
		}
	}
	return out
}

// FormatDays renders weekdays the way a person would say them: "Tue/Wed".
func FormatDays(days []time.Weekday) string {
	var parts []string
	for _, d := range days {
		parts = append(parts, d.String()[:3])
	}
	return strings.Join(parts, "/")
}
