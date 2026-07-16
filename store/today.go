package store

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// TodayItem is a commitment selected for the "Act on today" list,
// with a human-readable reason and an urgency class for the UI stripe.
type TodayItem struct {
	*Commitment
	Reason  string  `json:"reason"`
	Urgency string  `json:"urgency"` // "deadline" | "stale" | "plain"
	Score   float64 `json:"-"`
}

// words in a due_hint that signal an imminent deadline
var urgentDueWords = []string{
	"today", "tonight", "tomorrow", "eod", "asap", "morning", "by end of day",
}

var weekdays = []string{
	"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday",
}

func dueSignal(hint string) (hasDeadline, urgent bool) {
	h := strings.ToLower(hint)
	if strings.TrimSpace(h) == "" {
		return false, false
	}
	for _, w := range urgentDueWords {
		if strings.Contains(h, w) {
			return true, true
		}
	}
	for _, d := range weekdays {
		if strings.Contains(h, d) {
			return true, false
		}
	}
	if strings.Contains(h, "week") || strings.Contains(h, "by ") {
		return true, false
	}
	return false, false
}

// RankToday scores candidates and returns at most max items worth acting on
// today. Signals: reminder passed, due-hint urgency, promise age on HIGH
// significance, conversation going cold, favorites boost. Items below the
// floor are dropped entirely — a quiet day returns an empty list.
func RankToday(cands []*TodayCandidate, now time.Time, max int) []*TodayItem {
	const scoreFloor = 40.0

	var items []*TodayItem
	for _, c := range cands {
		score := 0.0
		reason := ""
		urgency := "plain"
		ageDays := now.Sub(c.CreatedAt).Hours() / 24

		// Reminder the user set that has come due — strongest signal.
		if c.ReminderAt != nil && !c.ReminderAt.After(now.Add(48*time.Hour)) {
			score += 100
			urgency = "deadline"
			if c.ReminderAt.Before(now) {
				reason = "reminder passed"
			} else {
				reason = "reminder due soon"
			}
		}

		// Deadline language in the extracted due hint.
		if hasDeadline, urgent := dueSignal(c.DueHint); hasDeadline {
			if urgent {
				score += 90
				urgency = "deadline"
				if reason == "" {
					reason = "due " + shortHint(c.DueHint)
				}
			} else {
				// A stated deadline gets more pressing as the promise ages.
				score += 40 + minF(ageDays*8, 40)
				if ageDays >= 3 {
					urgency = "deadline"
					if reason == "" {
						reason = "promised " + humanDays(ageDays) + " ago, due " + shortHint(c.DueHint)
					}
				} else if reason == "" {
					reason = "due " + shortHint(c.DueHint)
				}
			}
		}

		// HIGH significance promises aging without resolution.
		if c.Significance == "high" {
			score += 20
			if ageDays >= 3 {
				score += minF(ageDays*4, 32)
				if reason == "" {
					reason = "promised " + humanDays(ageDays) + " ago"
				}
			}
		}

		// Conversation going cold on something that matters.
		if c.LastActivity != nil {
			coldDays := now.Sub(*c.LastActivity).Hours() / 24
			if coldDays >= 5 && c.Significance == "high" {
				score += 25
				if urgency == "plain" {
					urgency = "stale"
				}
				if reason == "" {
					reason = "no activity in " + humanDays(coldDays)
				}
			}
		}

		// People the user deliberately tracks rank higher.
		if c.Favorited || c.FavChat {
			score += 15
			if reason == "" && ageDays >= 2 {
				reason = "starred · open " + humanDays(ageDays)
			}
		}

		if score < scoreFloor {
			continue
		}
		if reason == "" {
			reason = "open " + humanDays(ageDays)
		}
		items = append(items, &TodayItem{Commitment: c.Commitment, Reason: reason, Urgency: urgency, Score: score})
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Score > items[j].Score })

	// Diversify: drop near-duplicate titles (extraction sometimes catches the
	// same promise from two chats) and cap one item per chat so one loud
	// thread can't fill the whole list — the rest stay in Open.
	var picked []*TodayItem
	perChat := map[string]int{}
	for _, it := range items {
		if perChat[it.ChatJID] >= 1 {
			continue
		}
		dup := false
		for _, p := range picked {
			if titleSimilar(it.Title, p.Title) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		picked = append(picked, it)
		perChat[it.ChatJID]++
		if len(picked) == max {
			break
		}
	}
	return picked
}

// titleSimilar reports whether two commitment titles describe the same
// promise, using word-overlap (Jaccard) on lowercased tokens.
// Display-time threshold is looser (0.4) than the save-time dedupe (0.5)
// because dropping a near-duplicate from a ranked view is cheap, while
// refusing to save one is not.
func titleSimilar(a, b string) bool {
	return TitlesSimilar(a, b, 0.4)
}

// shortHint trims a due hint to a badge-sized phrase — extraction sometimes
// captures a whole clause ("every tuesday for 6 weeks starting from this
// message") where the first few words carry the signal.
func shortHint(hint string) string {
	h := strings.ToLower(strings.TrimSpace(hint))
	const maxLen = 24
	if len(h) <= maxLen {
		return h
	}
	cut := strings.LastIndex(h[:maxLen], " ")
	if cut < 8 {
		cut = maxLen
	}
	return h[:cut] + "…"
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func humanDays(d float64) string {
	n := int(d)
	switch {
	case n <= 0:
		return "today"
	case n == 1:
		return "1 day"
	default:
		return strconv.Itoa(n) + " days"
	}
}
