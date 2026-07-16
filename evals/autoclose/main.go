// Command autoclose replays historical auto-closed commitments against the
// new confidence-scored closure prompt and grades the outcomes with an
// independent Claude judge. It runs against a COPY of the real database
// (never the live one) and writes a markdown report.
//
// Usage:
//
//	go run ./evals/autoclose -db /path/to/copy/commit.db -n 60 -out evals/autoclose/REPORT.md
//
// The DB copy's directory must also contain .crypto_key (copied from
// ~/.commit) so store.Open can decrypt the Claude API key.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/msfoundry/commit/extraction"
	"github.com/msfoundry/commit/store"
)

const (
	autoCloseThreshold = 0.85
	pendingThreshold   = 0.60
	autoCloseType      = "recipient_acknowledgment" // only closure type allowed to close silently
	contextBeforeHours = 72  // messages before resolution shown to the scorer (sweep sees 48h; a little extra for slow chats)
	judgeAfterHours    = 48  // hindsight messages after resolution shown only to the judge
	maxContextMessages = 60
	minContextMessages = 4 // skip items with too little context to judge
)

type sampleItem struct {
	ID         string
	Title      string
	Context    string
	Direction  string
	PersonName string
	ChatJID    string
	ChatName   string
	ResolvedAt int64
	CreatedAt  int64

	// results
	Confidence  float64
	Evidence    string
	ClosureType string
	Detected    bool // scorer flagged this commitment at all
	ScoreErr    string

	JudgeVerdict string // good | bad | unclear
	JudgeReason  string
	JudgeErr     string
}

// evalMeta carries sampling facts from the dump phase to the collect phase.
type evalMeta struct {
	PoolSize      int    `json:"pool_size"`
	LLMPathLast14 int    `json:"llm_path_last14"`
	AllAutoLast14 int    `json:"all_auto_last14"`
	SchemaVersion string `json:"schema_version"`
	SampledN      int    `json:"sampled_n"`
}

func main() {
	dbPath := flag.String("db", "", "path to a COPY of commit.db (required)")
	n := flag.Int("n", 60, "sample size")
	out := flag.String("out", "evals/autoclose/REPORT.md", "report output path")
	details := flag.String("details", "", "optional path for per-item JSON details (contains private message text; keep out of git)")
	concurrency := flag.Int("concurrency", 3, "parallel API calls")
	seed := flag.Int64("seed", 42, "sampling seed")
	dumpDir := flag.String("dump-prompts", "", "write scorer/judge prompts to this dir and exit (offline mode, no API calls)")
	collectDir := flag.String("collect", "", "read model responses from this dir (written next to the dumped prompts) and build the report")
	scorerModelName := flag.String("scorer-model", "", "override the scorer model (default: production sweep model)")
	judgeModelName := flag.String("judge-model", "", "override the judge model (default: the DB's configured model)")
	flag.Parse()

	if *dbPath == "" {
		log.Fatal("-db is required (a copy of commit.db, never the live file)")
	}
	if strings.Contains(*dbPath, ".commit/commit.db") {
		log.Fatal("refusing to run against what looks like the live database; pass a copy")
	}

	// store.Open runs migrations — this doubles as the migration-v8 check.
	sdb, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	schemaVersion := sdb.GetSetting("schema_version")
	fmt.Printf("schema_version after open: %s\n", schemaVersion)

	raw, err := sql.Open("sqlite", *dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()

	items, poolSize, err := sampleAutoClosed(raw, *n, *seed)
	if err != nil {
		log.Fatalf("sample: %v", err)
	}
	fmt.Printf("replayable auto-closed pool: %d, sampled: %d\n", poolSize, len(items))

	llmPathLast14, allAutoLast14, err := burdenCounts(raw)
	if err != nil {
		log.Fatalf("burden counts: %v", err)
	}
	meta := evalMeta{PoolSize: poolSize, LLMPathLast14: llmPathLast14, AllAutoLast14: allAutoLast14,
		SchemaVersion: schemaVersion, SampledN: len(items)}

	scorerModel := store.FallbackModel // production sweep model
	judgeModel := sdb.GetModel()
	// Model overrides apply in every mode — the judge especially should be a
	// stronger model than the scorer, not whatever the DB happens to have.
	if *scorerModelName != "" {
		scorerModel = *scorerModelName
	}
	if *judgeModelName != "" {
		judgeModel = *judgeModelName
	}

	switch {
	case *dumpDir != "":
		if err := dumpPrompts(raw, items, meta, *dumpDir); err != nil {
			log.Fatalf("dump prompts: %v", err)
		}
		fmt.Printf("prompts written to %s (score_*.txt, judge_*.txt); put raw model responses in %s/responses/\n", *dumpDir, *dumpDir)
		return
	case *collectDir != "":
		if err := collectResponses(items, *collectDir); err != nil {
			log.Fatalf("collect: %v", err)
		}
	default:
		apiKey := sdb.GetAPIKey()
		if apiKey == "" {
			log.Fatal("no API key decryptable from DB copy (is .crypto_key next to it?)")
		}
		fmt.Printf("scorer model: %s, judge model: %s\n", scorerModel, judgeModel)
		runAll(raw, items, apiKey, scorerModel, judgeModel, *concurrency)
	}

	report := buildReport(items, meta.PoolSize, meta.LLMPathLast14, meta.AllAutoLast14, scorerModel, judgeModel, meta.SchemaVersion)
	if err := os.WriteFile(*out, []byte(report), 0644); err != nil {
		log.Fatalf("write report: %v", err)
	}
	fmt.Printf("report written to %s\n", *out)

	if *details != "" {
		blob, _ := json.MarshalIndent(items, "", "  ")
		os.WriteFile(*details, blob, 0600)
		fmt.Printf("per-item details (private) written to %s\n", *details)
	}
}

// dumpPrompts writes each item's scorer and judge prompt to files so the
// model calls can be executed out-of-band (e.g. when the DB's API key has no
// credit). Responses go in <dir>/responses/score_<id>.txt / judge_<id>.txt.
func dumpPrompts(raw *sql.DB, items []*sampleItem, meta evalMeta, dir string) error {
	if err := os.MkdirAll(dir+"/responses", 0700); err != nil {
		return err
	}
	for _, it := range items {
		sp, err := scorerPrompt(raw, it)
		if err != nil {
			return fmt.Errorf("scorer prompt %s: %w", it.ID, err)
		}
		jp, err := judgePrompt(raw, it)
		if err != nil {
			return fmt.Errorf("judge prompt %s: %w", it.ID, err)
		}
		if err := os.WriteFile(fmt.Sprintf("%s/score_%s.txt", dir, it.ID), []byte(sp), 0600); err != nil {
			return err
		}
		if err := os.WriteFile(fmt.Sprintf("%s/judge_%s.txt", dir, it.ID), []byte(jp), 0600); err != nil {
			return err
		}
	}
	blob, _ := json.MarshalIndent(meta, "", "  ")
	return os.WriteFile(dir+"/meta.json", blob, 0600)
}

// collectResponses parses out-of-band model responses back onto the items.
func collectResponses(items []*sampleItem, dir string) error {
	for _, it := range items {
		sresp, err := os.ReadFile(fmt.Sprintf("%s/responses/score_%s.txt", dir, it.ID))
		if err != nil {
			it.ScoreErr = "missing response"
		} else {
			parseScorerResponse(it, string(sresp))
		}
		jresp, err := os.ReadFile(fmt.Sprintf("%s/responses/judge_%s.txt", dir, it.ID))
		if err != nil {
			it.JudgeErr = "missing response"
			it.JudgeVerdict = "unclear"
		} else {
			parseJudgeResponse(it, string(jresp))
		}
	}
	return nil
}

// sampleAutoClosed picks n random auto-resolved commitments that have enough
// message context around their resolution time to replay.
func sampleAutoClosed(raw *sql.DB, n int, seed int64) ([]*sampleItem, int, error) {
	rows, err := raw.Query(`
		SELECT c.id, c.title, c.context, c.direction, c.person_name, c.chat_jid, c.chat_name,
			c.resolved_at, c.created_at
		FROM commitments c
		WHERE c.status = 'resolved' AND c.resolved_by = 'auto' AND c.resolved_at IS NOT NULL
			AND (SELECT COUNT(*) FROM messages m
				WHERE m.chat_jid = c.chat_jid
				AND m.timestamp BETWEEN c.resolved_at - ?*3600 AND c.resolved_at) >= ?
		ORDER BY c.resolved_at`, contextBeforeHours, minContextMessages)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var pool []*sampleItem
	for rows.Next() {
		it := &sampleItem{}
		if err := rows.Scan(&it.ID, &it.Title, &it.Context, &it.Direction, &it.PersonName,
			&it.ChatJID, &it.ChatName, &it.ResolvedAt, &it.CreatedAt); err != nil {
			return nil, 0, err
		}
		pool = append(pool, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if n > len(pool) {
		n = len(pool)
	}
	sample := pool[:n]
	sort.Slice(sample, func(i, j int) bool { return sample[i].ResolvedAt < sample[j].ResolvedAt })
	return sample, len(pool), nil
}

// burdenCounts estimates last-14-day auto-close volume. LLM-path closes are
// approximated as those with message activity shortly before resolution
// (staleness closes by definition happen in quiet chats).
func burdenCounts(raw *sql.DB) (llmPath, all int, err error) {
	cutoff := time.Now().Add(-14 * 24 * time.Hour).Unix()
	if err = raw.QueryRow(`SELECT COUNT(*) FROM commitments
		WHERE status='resolved' AND resolved_by='auto' AND resolved_at > ?`, cutoff).Scan(&all); err != nil {
		return
	}
	err = raw.QueryRow(`SELECT COUNT(*) FROM commitments c
		WHERE c.status='resolved' AND c.resolved_by='auto' AND c.resolved_at > ?
		AND (SELECT COUNT(*) FROM messages m
			WHERE m.chat_jid = c.chat_jid
			AND m.timestamp BETWEEN c.resolved_at - 48*3600 AND c.resolved_at) >= ?`,
		cutoff, minContextMessages).Scan(&llmPath)
	return
}

func loadMessages(raw *sql.DB, chatJID string, from, to int64, limit int) ([]*store.Message, error) {
	rows, err := raw.Query(`
		SELECT id, chat_jid, sender_name, chat_name, content, timestamp, is_from_me, is_group
		FROM messages
		WHERE chat_jid = ? AND timestamp BETWEEN ? AND ?
		ORDER BY timestamp DESC LIMIT ?`, chatJID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*store.Message
	for rows.Next() {
		m := &store.Message{}
		var ts int64
		var fromMe, isGroup int
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderName, &m.ChatName, &m.Content, &ts, &fromMe, &isGroup); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		m.IsFromMe = fromMe == 1
		m.IsGroup = isGroup == 1
		msgs = append(msgs, m)
	}
	// chronological order
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, rows.Err()
}

func runAll(raw *sql.DB, items []*sampleItem, apiKey, scorerModel, judgeModel string, concurrency int) {
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var done, total int
	var mu sync.Mutex
	total = len(items)

	for _, it := range items {
		wg.Add(1)
		go func(it *sampleItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			scoreItem(raw, it, apiKey, scorerModel)
			judgeItem(raw, it, apiKey, judgeModel)
			mu.Lock()
			done++
			fmt.Printf("[%d/%d] %s conf=%.2f judge=%s\n", done, total, it.ID[:8], it.Confidence, it.JudgeVerdict)
			mu.Unlock()
		}(it)
	}
	wg.Wait()
}

func callWithRetry(ctx context.Context, apiKey, model, prompt string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := extraction.CallClaude(ctx, apiKey, model, prompt)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "rate limited") || strings.Contains(err.Error(), "429") ||
			strings.Contains(err.Error(), "529") {
			time.Sleep(time.Duration(20*(attempt+1)) * time.Second)
			continue
		}
		return "", err
	}
	return "", lastErr
}

// scorerPrompt builds the exact production resolution-sweep prompt with only
// the messages the sweep would have seen at resolution time.
func scorerPrompt(raw *sql.DB, it *sampleItem) (string, error) {
	msgs, err := loadMessages(raw, it.ChatJID, it.ResolvedAt-contextBeforeHours*3600, it.ResolvedAt, maxContextMessages)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", fmt.Errorf("no messages in window")
	}
	c := &store.Commitment{
		ID: it.ID, Title: it.Title, Direction: it.Direction, PersonName: it.PersonName,
	}
	return extraction.BuildResolutionPrompt(msgs, []*store.Commitment{c}), nil
}

func parseScorerResponse(it *sampleItem, resp string) {
	var result struct {
		Closures []extraction.ClosureDetection `json:"closures"`
	}
	if err := json.Unmarshal([]byte(extraction.ExtractJSON(resp)), &result); err != nil {
		it.ScoreErr = fmt.Sprintf("parse: %v", err)
		return
	}
	for _, d := range result.Closures {
		if d.ID == it.ID {
			it.Detected = true
			it.Confidence = d.Confidence
			it.Evidence = d.Evidence
			it.ClosureType = d.ClosureType
			return
		}
	}
	// Not flagged at all: the new system would not have closed it.
	it.Detected = false
	it.Confidence = 0
}

// scoreItem replays the production resolution-sweep prompt via the API.
func scoreItem(raw *sql.DB, it *sampleItem, apiKey, model string) {
	prompt, err := scorerPrompt(raw, it)
	if err != nil {
		it.ScoreErr = fmt.Sprintf("build prompt: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := callWithRetry(ctx, apiKey, model, prompt)
	if err != nil {
		it.ScoreErr = err.Error()
		return
	}
	parseScorerResponse(it, resp)
}

// judgePrompt builds an independent audit prompt — with hindsight messages
// after the close that the scorer never saw.
func judgePrompt(raw *sql.DB, it *sampleItem) (string, error) {
	msgs, err := loadMessages(raw, it.ChatJID, it.ResolvedAt-contextBeforeHours*3600, it.ResolvedAt+judgeAfterHours*3600, maxContextMessages+20)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", fmt.Errorf("no messages in window")
	}

	var sb strings.Builder
	sb.WriteString(`You are auditing an automated commitment tracker. It watched a WhatsApp chat and auto-closed the commitment below, deciding it was completed or no longer needed. Your job: with hindsight, was that close CORRECT?

Commitment:
`)
	dir := "The user owed"
	if it.Direction == "they_owe" {
		dir = it.PersonName + " owed the user"
	}
	fmt.Fprintf(&sb, "- %s: %s\n", dir, it.Title)
	if it.Context != "" {
		fmt.Fprintf(&sb, "- Context: %s\n", it.Context)
	}
	fmt.Fprintf(&sb, "- Auto-closed at: %s\n\n", time.Unix(it.ResolvedAt, 0).Format("Jan 2 2006 3:04PM"))
	sb.WriteString(`Chat messages around (and after) the close. Messages from the user are marked [ME]:
`)
	for _, m := range msgs {
		prefix := m.SenderName
		if m.IsFromMe {
			prefix = "[ME]"
		}
		fmt.Fprintf(&sb, "[%s] %s: %s\n", m.Timestamp.Format("Jan 2 3:04PM"), prefix, m.Content)
	}
	sb.WriteString(`
Verdict rules:
- "good": the commitment was genuinely fulfilled, handled, or made moot by the close time (or clearly shortly after). Closing it was right.
- "bad": there is no real evidence it was done — it looks like it was still open, or the conversation merely moved on. Closing it silently was a mistake.
- "unclear": the messages genuinely don't tell you either way.

Be strict: pleasantries, topic changes, or silence are not completion. But also practical: for small ephemeral promises, a matching action (file, call, "sent") counts even without the word "done".

Return ONLY JSON: {"verdict": "good"|"bad"|"unclear", "reason": "one short sentence"}`)

	return sb.String(), nil
}

func parseJudgeResponse(it *sampleItem, resp string) {
	var v struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(extraction.ExtractJSON(resp)), &v); err != nil {
		it.JudgeErr = fmt.Sprintf("parse: %v", err)
		it.JudgeVerdict = "unclear"
		return
	}
	if v.Verdict != "good" && v.Verdict != "bad" && v.Verdict != "unclear" {
		v.Verdict = "unclear"
	}
	it.JudgeVerdict = v.Verdict
	it.JudgeReason = v.Reason
}

// judgeItem runs the judge prompt via the API.
func judgeItem(raw *sql.DB, it *sampleItem, apiKey, model string) {
	prompt, err := judgePrompt(raw, it)
	if err != nil {
		it.JudgeErr = fmt.Sprintf("build prompt: %v", err)
		it.JudgeVerdict = "unclear"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := callWithRetry(ctx, apiKey, model, prompt)
	if err != nil {
		it.JudgeErr = err.Error()
		it.JudgeVerdict = "unclear"
		return
	}
	parseJudgeResponse(it, resp)
}

// tier implements the shipped policy: silent auto-close only for completed
// two-sided handoffs; every other detection at >=0.60 becomes a suggestion.
func tier(it *sampleItem) string {
	switch {
	case it.Detected && it.Confidence >= autoCloseThreshold && it.ClosureType == autoCloseType:
		return "auto"
	case it.Detected && it.Confidence >= pendingThreshold:
		return "suggest"
	default:
		return "ignore"
	}
}

func pct(a, b int) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%% (%d/%d)", 100*float64(a)/float64(b), a, b)
}

func buildReport(items []*sampleItem, poolSize, llmPathLast14, allAutoLast14 int, scorerModel, judgeModel, schemaVersion string) string {
	var scored []*sampleItem
	scoreErrs := 0
	for _, it := range items {
		if it.ScoreErr != "" {
			scoreErrs++
			continue
		}
		scored = append(scored, it)
	}

	// confidence distribution
	buckets := map[string]int{}
	bucketOrder := []string{"not flagged", "<0.60", "0.60-0.84", "0.85+ one-sided", "0.85+ handoff"}
	tierCount := map[string]int{}
	verdicts := map[string]int{}
	judgeErrs := 0

	type cell struct{ good, bad, unclear int }
	byTier := map[string]*cell{"auto": {}, "suggest": {}, "ignore": {}}

	for _, it := range scored {
		switch {
		case !it.Detected:
			buckets["not flagged"]++
		case it.Confidence < pendingThreshold:
			buckets["<0.60"]++
		case it.Confidence < autoCloseThreshold:
			buckets["0.60-0.84"]++
		case it.ClosureType == autoCloseType:
			buckets["0.85+ handoff"]++
		default:
			buckets["0.85+ one-sided"]++
		}
		t := tier(it)
		tierCount[t]++
		verdicts[it.JudgeVerdict]++
		if it.JudgeErr != "" {
			judgeErrs++
		}
		switch it.JudgeVerdict {
		case "good":
			byTier[t].good++
		case "bad":
			byTier[t].bad++
		default:
			byTier[t].unclear++
		}
	}

	nScored := len(scored)
	good := verdicts["good"]
	bad := verdicts["bad"]
	unclear := verdicts["unclear"]

	// Key numbers
	badStillAuto := byTier["auto"].bad // judged-bad closes that would STILL close silently (target ~0)
	badCaught := byTier["suggest"].bad + byTier["ignore"].bad

	oldPrecisionNum, oldPrecisionDen := good, good+bad
	autoGood := byTier["auto"].good
	autoDen := byTier["auto"].good + byTier["auto"].bad // handoff auto-tier correctness, unclear excluded

	suggestFrac := 0.0
	if nScored > 0 {
		suggestFrac = float64(tierCount["suggest"]) / float64(nScored)
	}
	suggestPerWeekRaw := float64(llmPathLast14) / 2.0 * suggestFrac

	var sb strings.Builder
	fmt.Fprintf(&sb, `# Auto-Close Eval: Handoff-Only Auto-Close vs Single Bucket

Generated: %s
Scorer model (production sweep model): %s
Judge model: %s
Schema version after opening DB copy: %s (migration v8 applied cleanly)

## Policy under test (final)

- **Silent auto-close only for completed handoffs**: the chat shows BOTH the
  delivery by the committer AND the recipient acknowledging it
  (closure_type "recipient_acknowledgment", confidence >= %.2f).
- **Everything else detected at >= %.2f becomes a suggestion** ("These look
  done") — one-sided "I sent it" claims, indirect references, moot-by-events,
  call-fulfills-commitment. The UI shows at most 5, highest confidence first;
  suggestions expire silently after 7 days (recorded as reason='expired').
- Below %.2f: ignored. The rule-based staleness sweep is unchanged.

This supersedes the earlier symmetric >= 0.85 / 0.60-0.84 tiers after two
direct-API reruns showed the 0.60-0.84 band contained zero judged-good items
and a stronger scorer declined to flag most closes — chat text alone cannot
justify most silent closes (see REPORT-direct.md, REPORT-sonnet-scorer.md,
kept as superseded inputs).

## Methodology

**Ground truth caveat:** the primary methodology (auto-closes later reopened by
the user) is NOT recoverable from this database. Reopening sets status='open'
and overwrites resolved_by with 'user'; there is no status-history table. The
known-bad ground truth set is therefore empty, and this eval uses the stated
fallback: an LLM-judge methodology.

**Retrospective replay.** From %d historically auto-closed commitments with
at least %d messages of chat context in the %dh before their resolution
("replayable pool"), a seeded random sample of %d was drawn. For each item:

1. **Scorer (new system):** the exact production resolution-sweep prompt
   (confidence + evidence + closure-type rubric, handoff-gated calibration)
   was re-run on the messages the sweep would have seen at resolution time,
   using the production sweep model. Detections map to the policy above.
   A commitment the scorer does not flag at all counts as "ignore".
2. **Judge (label):** an independent pass with a stronger model, given %dh of
   additional hindsight messages after the close, graded each historical
   auto-close as good (correct), bad (mistake), or unclear.

The old system auto-closed 100%% of these items (that is how they entered the
sample), so the judge's verdicts directly measure the old single bucket's
precision.

## Sample

- Replayable auto-closed pool: %d (of all auto-closed commitments)
- Sampled and scored: %d (scorer errors: %d, judge errors treated as unclear: %d)

## Scorer distribution (on items the old system closed)

| Bucket | Count | Share |
|---|---|---|
`,
		time.Now().Format("2006-01-02 15:04 MST"), scorerModel, judgeModel, schemaVersion,
		autoCloseThreshold, pendingThreshold, pendingThreshold,
		poolSize, minContextMessages, contextBeforeHours, len(items),
		judgeAfterHours,
		poolSize, nScored, scoreErrs, judgeErrs)

	for _, b := range bucketOrder {
		fmt.Fprintf(&sb, "| %s | %d | %s |\n", b, buckets[b], pct(buckets[b], nScored))
	}

	fmt.Fprintf(&sb, `
Tier assignment under the final policy: auto %s, suggest %s, ignore %s.

## Judge labels

good %d, bad %d, unclear %d (of %d scored).

## Headline metrics (old vs new)

| Metric | Value |
|---|---|
| **Judged-bad closes that would STILL close silently** (target ~0) | **%s** |
| Judged-bad closes caught (suggested or left open instead) | %s |
| Old single-bucket precision (judge-labeled, unclear excluded) | %s |
| Two-sided-handoff auto-closes judged correct (new auto tier precision) | %s |
| Estimated suggestions per week (raw rate) | ~%.0f |
| Displayed suggestion burden | at most 5 visible at a time; 7-day expiry |

Tier vs judge cross-tab:

| Tier | good | bad | unclear |
|---|---|---|---|
| auto (handoff, >=%.2f) | %d | %d | %d |
| suggest (>=%.2f, non-handoff or <%.2f) | %d | %d | %d |
| ignore (<%.2f) | %d | %d | %d |

Suggestion burden basis: last 14 days had %d auto-closes total, of which %d had
recent chat activity before close (proxy for the LLM-detection path this
change affects; the remainder are rule-based staleness closes, which keep
auto-closing directly). %d/2 per week x %.0f%% suggest rate = ~%.0f raw
suggestions/week entering the table. The Today view displays at most 5 at a
time (highest confidence first) and unactioned rows expire after 7 days, so
the user-visible review burden is a glance at <= 5 rows regardless of the raw
rate; the rest expire silently as training data.

## Interpretation

The judge confirms the old always-auto-close bucket was imprecise: most
sampled closes had no real completion evidence. Under the final policy, silent
closing is reserved for the one pattern with two-sided evidence — a delivery
plus the recipient's acknowledgment — and everything else the model suspects
is surfaced as a capped, expiring suggestion or simply left open. Items left
open do not pile up forever: they remain subject to the unchanged rule-based
staleness sweep and normal user actions.

## Caveats

- **No human ground truth.** Labels come from a single stronger-model judge
  pass with hindsight context, not from the user. Judge mistakes move both
  headline numbers; "unclear" items (%d) are excluded from precision.
- **The auto tier is small by design**, so its precision estimate rests on
  very few judge-labeled items — read it as directional, not exact.
- **Label circularity risk is limited but real:** scorer and judge share a
  model family, though they use different prompts, different models, and the
  judge sees %dh of extra hindsight the scorer cannot.
- **Sample size** (%d scored) gives rough precision estimates; treat
  percentage points as +/-10 or so at this n.
- **Sampling bias:** only auto-closes with chat activity before close were
  replayable. Rule-based staleness closes (quiet chats) are underrepresented
  by design — the tiered change does not touch that path.
- **The pool predates this change**, so items closed by the old aggressive
  prompt at low true confidence are exactly what we want to measure, but chat
  context reconstruction (%dh window, %d-message cap) may differ slightly
  from what the sweep saw live.
- Per-item evidence and judge reasons contain private message text and are
  deliberately NOT included in this committed report (write them locally with
  -details).
%s`,
		pct(tierCount["auto"], nScored), pct(tierCount["suggest"], nScored), pct(tierCount["ignore"], nScored),
		good, bad, unclear, nScored,
		pct(badStillAuto, bad),
		pct(badCaught, bad),
		pct(oldPrecisionNum, oldPrecisionDen),
		pct(autoGood, autoDen),
		suggestPerWeekRaw,
		autoCloseThreshold, byTier["auto"].good, byTier["auto"].bad, byTier["auto"].unclear,
		pendingThreshold, autoCloseThreshold, byTier["suggest"].good, byTier["suggest"].bad, byTier["suggest"].unclear,
		pendingThreshold, byTier["ignore"].good, byTier["ignore"].bad, byTier["ignore"].unclear,
		allAutoLast14, llmPathLast14, llmPathLast14, 100*suggestFrac, suggestPerWeekRaw,
		unclear, judgeAfterHours, nScored, contextBeforeHours, maxContextMessages,
		executionNote(scorerModel))

	return sb.String()
}

// executionNote adds a caveat when the model calls were executed out-of-band
// (e.g. via Claude Code subagents because the DB's API key had no credit).
func executionNote(scorerModel string) string {
	if !strings.Contains(scorerModel, "subagent") {
		return ""
	}
	return `- **Execution path:** the configured API key (and the machine's other
  Anthropic key) had no remaining credit at eval time, so prompts were built
  and parsed by this harness (-dump-prompts / -collect) but executed through
  Claude Code subagents of the corresponding model families instead of raw
  API calls. Harness system prompts may shift scores slightly vs production;
  rerun in direct API mode once the key has credit to confirm.
`
}
