package schedule

import (
	"testing"
	"time"
)

// The bug this guards against: the interpreter asks for RFC3339, but models
// intermittently drop the zone offset ("2026-07-17T17:30:00"). A strict
// time.Parse(time.RFC3339, ...) then fails at BOOK time, and the session
// refuses to complete with no visible reason — the counterpart's accepted time
// simply never becomes an event. A zone-less timestamp means the user's
// timezone, which is the one the prompt asked the model to use.
func TestParseFlexibleTime(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)

	cases := []struct {
		name string
		in   string
		want time.Time
		ok   bool
	}{
		{"rfc3339_with_offset", "2026-07-17T17:30:00+05:30",
			time.Date(2026, 7, 17, 17, 30, 0, 0, ist), true},
		{"rfc3339_utc", "2026-07-17T12:00:00Z",
			time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC), true},
		{"zoneless_seconds", "2026-07-17T17:30:00",
			time.Date(2026, 7, 17, 17, 30, 0, 0, ist), true},
		{"zoneless_no_seconds", "2026-07-17T17:30",
			time.Date(2026, 7, 17, 17, 30, 0, 0, ist), true},
		{"space_separated", "2026-07-17 17:30:00",
			time.Date(2026, 7, 17, 17, 30, 0, 0, ist), true},
		{"empty", "", time.Time{}, false},
		{"prose", "next tuesday at 5", time.Time{}, false},
		{"date_only", "2026-07-17", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := parseFlexibleTime(c.in, ist)
		if ok != c.ok {
			t.Errorf("%s: ok=%v want %v", c.name, ok, c.ok)
			continue
		}
		if ok && !got.Equal(c.want) {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

// A zone-less timestamp must land on the same instant as the explicit-offset
// form — otherwise "5:30pm" quietly becomes a UTC 5:30pm and books the wrong
// hour.
func TestParseFlexibleTime_ZonelessMatchesExplicitOffset(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	naive, ok := parseFlexibleTime("2026-07-17T17:30:00", ist)
	if !ok {
		t.Fatal("zone-less parse must succeed")
	}
	explicit, ok := parseFlexibleTime("2026-07-17T17:30:00+05:30", ist)
	if !ok {
		t.Fatal("rfc3339 parse must succeed")
	}
	if !naive.Equal(explicit) {
		t.Fatalf("zone-less %s must equal explicit %s", naive, explicit)
	}
}
