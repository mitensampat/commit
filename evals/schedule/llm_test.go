// LLM interpreter evals — run against the real Claude API.
//
// Usage:
//
//	COMMIT_EVAL_DB=/path/to/dir-with-commit.db \
//	COMMIT_EVAL_REPORT=1 \
//	go test ./evals/schedule/ -run TestLLM -v -timeout 30m
//
// The DB directory must contain commit.db and .crypto_key (a COPY of
// ~/.commit — never point this at the live DB). Interpretation runs on the
// haiku-class model to match production cost; set COMMIT_EVAL_MODEL to
// spot-check hard cases on sonnet. Without COMMIT_EVAL_DB the tests skip, so
// CI stays hermetic.
package scheduleevals

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/msfoundry/commit/schedule"
	"github.com/msfoundry/commit/store"
)

const evalModel = "claude-haiku-4-5-20251001"

func evalCreds(t *testing.T) schedule.Creds {
	dir := os.Getenv("COMMIT_EVAL_DB")
	if dir == "" {
		t.Skip("COMMIT_EVAL_DB not set — skipping real-API evals")
	}
	db, err := store.Open(filepath.Join(dir, "commit.db"))
	if err != nil {
		t.Fatalf("open eval db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	key := db.GetAPIKey()
	if key == "" {
		t.Fatal("eval db has no API key")
	}
	model := os.Getenv("COMMIT_EVAL_MODEL")
	if model == "" {
		model = evalModel
	}
	return schedule.Creds{APIKey: func() string { return key }, Model: func() string { return model }}
}

type result struct {
	category string
	name     string
	pass     bool
	detail   string
}

func gradeCase(c llmCase, interp *schedule.Interpretation) (bool, string) {
	if c.forbidAccept && interp.Intent == schedule.ReplyAccept {
		return false, fmt.Sprintf("SAFETY: classified as accept (slot %d)", interp.SlotIndex)
	}
	intentOK := false
	for _, w := range c.wantIntents {
		if interp.Intent == w {
			intentOK = true
			break
		}
	}
	if !intentOK {
		return false, fmt.Sprintf("intent=%s want one of %v", interp.Intent, c.wantIntents)
	}
	if c.wantSlot >= 0 && interp.Intent == schedule.ReplyAccept && interp.SlotIndex != c.wantSlot {
		return false, fmt.Sprintf("slot=%d want %d", interp.SlotIndex, c.wantSlot)
	}
	if c.wantSideNote && strings.TrimSpace(interp.SideNote) == "" {
		return false, "missing side_note"
	}
	if interp.Intent == schedule.ReplyCounter && (c.wantCounterDay >= 0 || c.wantCounterHour >= 0) {
		ct, err := time.Parse(time.RFC3339, interp.CounterTime)
		if err != nil {
			return false, fmt.Sprintf("unparseable counter_time %q", interp.CounterTime)
		}
		local := ct.In(evalLoc)
		if c.wantCounterDay >= 0 && local.Weekday() != c.wantCounterDay {
			return false, fmt.Sprintf("counter day=%s want %s (%s)", local.Weekday(), c.wantCounterDay, interp.CounterTime)
		}
		if c.wantCounterHour >= 0 && local.Hour() != c.wantCounterHour {
			return false, fmt.Sprintf("counter hour=%d want %d (%s)", local.Hour(), c.wantCounterHour, interp.CounterTime)
		}
	}
	return true, ""
}

func TestLLMInterpretReply(t *testing.T) {
	creds := evalCreds(t)
	li := &schedule.LLMInterpreter{Creds: creds}
	ctx := context.Background()

	caseFilter := os.Getenv("COMMIT_EVAL_CASE")
	var results []result
	for _, c := range llmCases {
		if caseFilter != "" && !strings.Contains(c.name, caseFilter) {
			continue
		}
		slots := c.slots
		if slots == nil {
			slots = stdSlots
		}
		draft := c.draft
		if draft == "" {
			draft = stdDraft
		}
		rc := schedule.ReplyContext{
			ContactName: "Akshay Shah",
			Slots:       slots,
			Draft:       draft,
			Thread:      c.thread,
			Now:         evalNow,
			Location:    evalLoc,
		}
		interp, err := li.InterpretReply(ctx, rc)
		var pass bool
		var detail string
		if err != nil {
			pass, detail = false, "error: "+err.Error()
		} else {
			pass, detail = gradeCase(c, interp)
			if !pass {
				detail += fmt.Sprintf(" | got %+v", *interp)
			}
		}
		results = append(results, result{c.category, c.name, pass, detail})
		status := "PASS"
		if !pass {
			status = "FAIL"
		}
		t.Logf("[%s] %-12s %-32s %s", status, c.category, c.name, detail)
	}

	// Own-message (manual resolution) cases.
	for _, c := range ownCases {
		if caseFilter != "" && !strings.Contains(c.name, caseFilter) {
			continue
		}
		rc := schedule.ReplyContext{
			ContactName: "Akshay Shah",
			Slots:       stdSlots,
			Draft:       stdDraft,
			Thread:      c.thread,
			Now:         evalNow,
			Location:    evalLoc,
		}
		finalized, err := li.InterpretOwnMessage(ctx, rc)
		pass := err == nil && finalized == c.wantFinalized
		detail := ""
		if err != nil {
			detail = "error: " + err.Error()
		} else if !pass {
			detail = fmt.Sprintf("finalized=%v want %v", finalized, c.wantFinalized)
		}
		results = append(results, result{"manual_resolution", c.name, pass, detail})
		status := "PASS"
		if !pass {
			status = "FAIL"
		}
		t.Logf("[%s] %-12s %-32s %s", status, "manual_res", c.name, detail)
	}

	summarize(t, results)
}

// safetyCritical categories must be perfect; the rest must clear 95%.
var safetyCritical = map[string]bool{
	"ambiguous":         true, // never-book-on-ambiguity
	"correction":        true, // correction race: latest state wins
	"manual_resolution": true, // watcher stand-down
}

func summarize(t *testing.T, results []result) {
	type agg struct{ pass, total int }
	byCat := map[string]*agg{}
	var cats []string
	totalPass, total := 0, 0
	for _, r := range results {
		if byCat[r.category] == nil {
			byCat[r.category] = &agg{}
			cats = append(cats, r.category)
		}
		byCat[r.category].total++
		total++
		if r.pass {
			byCat[r.category].pass++
			totalPass++
		}
	}
	sort.Strings(cats)

	var sb strings.Builder
	sb.WriteString("# @schedule interpreter eval report\n\n")
	sb.WriteString(fmt.Sprintf("Model: `%s` · run at %s · %d/%d overall (%.1f%%)\n\n",
		getModelName(), time.Now().Format("2006-01-02 15:04 MST"), totalPass, total, 100*float64(totalPass)/float64(total)))
	sb.WriteString("| Category | Pass | Total | Rate | Bar |\n|---|---|---|---|---|\n")
	for _, cat := range cats {
		a := byCat[cat]
		rate := 100 * float64(a.pass) / float64(a.total)
		bar := "95%"
		if safetyCritical[cat] {
			bar = "100% (safety-critical)"
		}
		sb.WriteString(fmt.Sprintf("| %s | %d | %d | %.0f%% | %s |\n", cat, a.pass, a.total, rate, bar))
	}
	sb.WriteString("\n## Failures\n\n")
	anyFail := false
	for _, r := range results {
		if !r.pass {
			anyFail = true
			sb.WriteString(fmt.Sprintf("- **%s/%s** — %s\n", r.category, r.name, r.detail))
		}
	}
	if !anyFail {
		sb.WriteString("None.\n")
	}
	t.Log("\n" + sb.String())

	if os.Getenv("COMMIT_EVAL_REPORT") != "" {
		writeReport(t, sb.String())
	}

	for _, cat := range cats {
		a := byCat[cat]
		rate := float64(a.pass) / float64(a.total)
		if safetyCritical[cat] && a.pass != a.total {
			t.Errorf("SAFETY-CRITICAL category %s below 100%%: %d/%d", cat, a.pass, a.total)
		} else if !safetyCritical[cat] && rate < 0.95 {
			t.Errorf("category %s below 95%%: %d/%d", cat, a.pass, a.total)
		}
	}
}

func getModelName() string {
	if m := os.Getenv("COMMIT_EVAL_MODEL"); m != "" {
		return m
	}
	return evalModel
}

func writeReport(t *testing.T, body string) {
	_, thisFile, _, _ := runtimeCaller()
	path := filepath.Join(filepath.Dir(thisFile), "REPORT.md")
	header := "<!-- Generated by: COMMIT_EVAL_DB=<db-copy-dir> COMMIT_EVAL_REPORT=1 go test ./evals/schedule/ -run TestLLM -->\n\n"
	if err := os.WriteFile(path, []byte(header+body), 0644); err != nil {
		t.Logf("could not write REPORT.md: %v", err)
	} else {
		t.Logf("wrote %s", path)
	}
}

func runtimeCaller() (uintptr, string, int, bool) {
	return runtime.Caller(1)
}
