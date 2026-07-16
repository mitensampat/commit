package schedule

import "testing"

// The bug this guards against: users type the name WhatsApp shows them (from
// their address book) while chat metadata carries only the push name.
// "Allish Jain" must match a contact known as "Allish".
func TestNameMatchScore(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		target  string
		wantHit bool
	}{
		{"exact", "allish", "Allish", true},
		{"address-book name vs push name", "allish jain", "Allish", true},
		{"push name vs address-book name", "allish", "Allish Jain", true},
		{"first name of full name", "harish", "Harish Sampat", true},
		{"full name typed, full name known", "harish sampat", "Harish Sampat", true},
		{"case and spacing", "  KUNAL   shah ", "kunal shah", true},
		{"prefix", "kun", "Kunal Shah", true},
		{"unrelated", "allish jain", "Priya Menon", false},
		{"empty query", "", "Allish", false},
		{"empty target", "allish", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nameMatchScore(normalizeQuery(tc.query), tc.target)
			if (got > 0) != tc.wantHit {
				t.Fatalf("nameMatchScore(%q, %q) = %d, want hit=%v", tc.query, tc.target, got, tc.wantHit)
			}
		})
	}
}

// A stronger match must outrank a weaker one so the caller can resolve
// outright instead of asking.
func TestNameMatchScoreOrdering(t *testing.T) {
	q := normalizeQuery("allish jain")
	full := nameMatchScore(q, "Allish")        // same person, shorter known name
	other := nameMatchScore(q, "Jain Traders") // incidental surname overlap
	if full <= other {
		t.Fatalf("expected 'Allish' (%d) to outrank 'Jain Traders' (%d)", full, other)
	}

	q2 := normalizeQuery("kunal")
	exact := nameMatchScore(q2, "Kunal")
	partial := nameMatchScore(q2, "Kunal Shah")
	if exact < partial {
		t.Fatalf("exact match (%d) should not rank below partial (%d)", exact, partial)
	}
}
