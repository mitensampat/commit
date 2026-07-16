package store

import "strings"

// TitleSimilarity returns the Jaccard word-overlap similarity of two
// commitment titles on lowercased tokens (0 = disjoint, 1 = identical).
// Shared by display-time dedupe (today view) and save-time dedupe
// (extraction pipeline), so both paths agree on what "the same promise" means.
func TitleSimilarity(a, b string) float64 {
	ta := simTokenSet(a)
	tb := simTokenSet(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for w := range ta {
		if tb[w] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	return float64(inter) / float64(union)
}

// TitlesSimilar reports whether two titles describe the same promise at the
// given Jaccard threshold.
func TitlesSimilar(a, b string, threshold float64) bool {
	return TitleSimilarity(a, b) >= threshold
}

func simTokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(s)) {
		w = strings.Trim(w, ".,()!?:;\"'")
		if len(w) > 2 {
			out[w] = true
		}
	}
	return out
}
