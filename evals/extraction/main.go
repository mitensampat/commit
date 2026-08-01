// Command extraction-eval measures and proves the quality of Commit's
// commitment-extraction pipeline.
//
// Usage (from repo root, against a COPY of the live DB — never the live DB):
//
//	go run ./evals/extraction -db /path/to/copy/commit.db -phase baseline
//	go run ./evals/extraction -db /path/to/copy/commit.db -phase dupes
//	go run ./evals/extraction -db /path/to/copy/commit.db -phase replay
//	go run ./evals/extraction -db /path/to/copy/commit.db -phase report
//
// Phases write JSON artifacts into -out (default evals/extraction) and
// "report" composes REPORT.md from them.
//
//   - baseline: stratified sample of ~80 commitments created in the last 30
//     days, source-message reconstruction, one structured judge call per item
//     (claude-sonnet-5).
//   - dupes: programmatic near-duplicate scan (Jaccard title similarity) over
//     the same 30-day window, per-chat and cross-chat.
//   - replay: re-runs the sampled items' source-message batches through the
//     CURRENT extraction prompt with the production extraction model (haiku),
//     applies the save-time dedupe the pipeline applies, then judges each
//     batch's output (precision) and coverage of baseline-real items (recall)
//     with claude-sonnet-5.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/msfoundry/commit/extraction"
	"github.com/msfoundry/commit/store"
)

const (
	judgeModel      = "claude-sonnet-5"
	sampleSize      = 80
	contextWindow   = 12  // messages on each side of the source message
	dedupeThreshold = 0.5 // save-time Jaccard threshold (mirrors pipeline)
	seed            = 20260716
)

// ---------- artifact types ----------

type SampledItem struct {
	ID           string   `json:"id"`
	ChatJID      string   `json:"chat_jid"`
	IsGroup      bool     `json:"is_group"`
	Direction    string   `json:"direction"`
	Significance string   `json:"significance"`
	Title        string   `json:"title"`
	Context      string   `json:"context"`
	PersonName   string   `json:"person_name"`
	SourceQuote  string   `json:"source_quote"`
	DueHint      string   `json:"due_hint"`
	MessageID    string   `json:"message_id"`
	CreatedAt    int64    `json:"created_at"`
	AnchorTS     int64    `json:"anchor_ts"`     // timestamp the window is centered on
	WindowMsgIDs []string `json:"window_msg_ids"`
	ContextFound bool     `json:"context_found"` // whether any source messages were reconstructed

	Judgment *Judgment `json:"judgment,omitempty"`
}

type Judgment struct {
	RealCommitment    bool   `json:"real_commitment"`
	NoiseCategory     string `json:"noise_category"`
	AttributionOK     bool   `json:"attribution_ok"`
	TitleQuality      string `json:"title_quality"` // good | acceptable | poor
	SignificanceOK    bool   `json:"significance_ok"`
	AnonymizedPattern string `json:"anonymized_pattern"`
	Notes             string `json:"notes"`
}

type BaselineArtifact struct {
	GeneratedAt string        `json:"generated_at"`
	JudgeModel  string        `json:"judge_model"`
	Items       []SampledItem `json:"items"`
}

type DupesArtifact struct {
	GeneratedAt        string   `json:"generated_at"`
	Threshold          float64  `json:"threshold"`
	WindowDays         int      `json:"window_days"`
	Total              int      `json:"total"`
	InChatDupItems     int      `json:"in_chat_dup_items"`
	CrossChatDupItems  int      `json:"cross_chat_dup_items"`
	InChatDupPairs     int      `json:"in_chat_dup_pairs"`
	CrossChatDupPairs  int      `json:"cross_chat_dup_pairs"`
	ExamplePatterns    []string `json:"example_patterns"` // redacted-ish: titles only, no phone JIDs
	OpenOnlyTotal      int      `json:"open_only_total"`
	OpenInChatDupItems int      `json:"open_in_chat_dup_items"`
}

type ReplayExtraction struct {
	Title          string    `json:"title"`
	Context        string    `json:"context"`
	Direction      string    `json:"direction"`
	PersonName     string    `json:"person_name"`
	SourceQuote    string    `json:"source_quote"`
	DueHint        string    `json:"due_hint"`
	Significance   string    `json:"significance"`
	DroppedLow     bool      `json:"dropped_low"`     // filtered by significance gate
	DroppedDupe    bool      `json:"dropped_dupe"`    // filtered by save-time dedupe
	Judgment       *Judgment `json:"judgment,omitempty"`
}

type ReplayBatch struct {
	Key           string             `json:"key"`
	ChatJID       string             `json:"chat_jid"`
	MsgIDs        []string           `json:"msg_ids"`
	BaselineIDs   []string           `json:"baseline_ids"` // sampled items whose window this batch covers
	Extractions   []ReplayExtraction `json:"extractions"`
	Coverage      map[string]bool    `json:"coverage"` // baseline item id -> still extracted
	ExtractionErr string             `json:"extraction_err,omitempty"`
	JudgeErr      string             `json:"judge_err,omitempty"`
}

type ReplayArtifact struct {
	GeneratedAt     string        `json:"generated_at"`
	ExtractionModel string        `json:"extraction_model"`
	JudgeModel      string        `json:"judge_model"`
	Batches         []ReplayBatch `json:"batches"`
}

// ---------- main ----------

func main() {
	dbPath := flag.String("db", "", "path to a COPY of commit.db (with .crypto_key beside it)")
	phase := flag.String("phase", "", "baseline | dupes | replay | report")
	out := flag.String("out", "evals/extraction", "artifact directory")
	n := flag.Int("n", sampleSize, "sample size for baseline")
	flag.Parse()

	if *phase == "" {
		log.Fatal("-phase required: baseline | dupes | replay | report")
	}
	if *phase != "report" && *dbPath == "" {
		log.Fatal("-db required")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}

	var db *store.DB
	if *dbPath != "" {
		if abs, err := filepath.Abs(*dbPath); err == nil {
			home, _ := os.UserHomeDir()
			if abs == filepath.Join(home, ".commit", "commit.db") {
				log.Fatal("refusing to open the live DB — pass a copy")
			}
		}
		var err error
		db, err = store.Open(*dbPath)
		if err != nil {
			log.Fatalf("open db: %v", err)
		}
		defer db.Close()
	}

	switch *phase {
	case "baseline":
		runBaseline(db, *out, *n)
	case "dupes":
		runDupes(db, *out)
	case "replay":
		runReplay(db, *out)
	case "report":
		runReport(*out)
	default:
		log.Fatalf("unknown phase %q", *phase)
	}
}

// ---------- phase: baseline ----------

func runBaseline(db *store.DB, out string, n int) {
	apiKey := db.GetAPIKey()
	if apiKey == "" {
		log.Fatal("no API key in DB copy")
	}
	path := filepath.Join(out, "baseline.json")

	// Resume: reuse an existing sample and its verdicts, judging only the gaps.
	var items []SampledItem
	var prior BaselineArtifact
	if readJSONOK(path, &prior) && len(prior.Items) > 0 {
		items = prior.Items
		done := 0
		for _, it := range items {
			if it.Judgment != nil {
				done++
			}
		}
		log.Printf("resuming existing sample: %d items, %d already judged", len(items), done)
	} else {
		items = sampleCommitments(db, n)
		log.Printf("sampled %d commitments", len(items))
	}

	ctx := context.Background()
	for i := range items {
		it := &items[i]
		if it.Judgment != nil {
			continue
		}
		msgs := reconstructWindow(db, it)
		prompt := buildJudgePrompt(it, msgs, siblingTitles(db, it))
		resp, err := callWithRetry(ctx, func() (string, error) {
			return callJudge(ctx, apiKey, prompt, 1500)
		})
		if err != nil {
			log.Printf("[%d/%d] judge error for %s: %v", i+1, len(items), it.ID, err)
			continue
		}
		var j Judgment
		if err := json.Unmarshal([]byte(extraction.ExtractJSON(resp)), &j); err != nil {
			log.Printf("[%d/%d] parse error for %s: %v", i+1, len(items), it.ID, err)
			continue
		}
		it.Judgment = &j
		log.Printf("[%d/%d] %s real=%v noise=%q title=%s", i+1, len(items), it.ID[:8], j.RealCommitment, j.NoiseCategory, j.TitleQuality)
		// Persist after every judgment — never lose completed API work.
		writeJSON(path, BaselineArtifact{
			GeneratedAt: time.Now().Format(time.RFC3339),
			JudgeModel:  judgeModel,
			Items:       items,
		})
	}

	writeJSON(path, BaselineArtifact{
		GeneratedAt: time.Now().Format(time.RFC3339),
		JudgeModel:  judgeModel,
		Items:       items,
	})
	log.Printf("wrote %s", path)
}

func sampleCommitments(db *store.DB, n int) []SampledItem {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	all, err := db.GetCommitments("") // all statuses
	if err != nil {
		log.Fatalf("load commitments: %v", err)
	}
	// bucket by (direction, is_group, significance) — high/medium only
	buckets := map[string][]*store.Commitment{}
	total := 0
	for _, c := range all {
		if c.CreatedAt.Before(cutoff) {
			continue
		}
		if c.Significance != "high" && c.Significance != "medium" {
			continue
		}
		key := fmt.Sprintf("%s|%v|%s", c.Direction, c.IsGroup, c.Significance)
		buckets[key] = append(buckets[key], c)
		total++
	}
	if total == 0 {
		log.Fatal("no commitments in window")
	}
	rng := rand.New(rand.NewSource(seed))
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var items []SampledItem
	// proportional allocation with a floor of 4 per non-empty cell
	remaining := n
	alloc := map[string]int{}
	for _, k := range keys {
		a := len(buckets[k]) * n / total
		if a < 4 {
			a = 4
		}
		if a > len(buckets[k]) {
			a = len(buckets[k])
		}
		alloc[k] = a
		remaining -= a
	}
	// trim proportionally if over
	for remaining < 0 {
		big := keys[0]
		for _, k := range keys {
			if alloc[k] > alloc[big] {
				big = k
			}
		}
		alloc[big]--
		remaining++
	}
	for _, k := range keys {
		b := buckets[k]
		rng.Shuffle(len(b), func(i, j int) { b[i], b[j] = b[j], b[i] })
		for i := 0; i < alloc[k] && i < len(b); i++ {
			c := b[i]
			items = append(items, SampledItem{
				ID: c.ID, ChatJID: c.ChatJID, IsGroup: c.IsGroup,
				Direction: c.Direction, Significance: c.Significance,
				Title: c.Title, Context: c.Context, PersonName: c.PersonName,
				SourceQuote: c.SourceQuote, DueHint: c.DueHint,
				MessageID: c.MessageID, CreatedAt: c.CreatedAt.Unix(),
			})
		}
	}
	return items
}

func reconstructWindow(db *store.DB, it *SampledItem) []*store.Message {
	anchor := it.CreatedAt
	if it.MessageID != "" {
		if m, err := db.GetMessageByID(it.MessageID); err == nil && m != nil {
			anchor = m.Timestamp.Unix()
		}
	}
	it.AnchorTS = anchor
	msgs, err := db.GetMessagesAround(it.ChatJID, anchor, contextWindow)
	if err != nil {
		return nil
	}
	it.ContextFound = len(msgs) > 0
	it.WindowMsgIDs = it.WindowMsgIDs[:0]
	for _, m := range msgs {
		it.WindowMsgIDs = append(it.WindowMsgIDs, m.ID)
	}
	return msgs
}

// siblingTitles returns titles of other commitments in the same chat created
// within ±3 days, so the judge can flag near-duplicate extractions.
func siblingTitles(db *store.DB, it *SampledItem) []string {
	cs, err := db.GetCommitments("")
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range cs {
		if c.ID == it.ID || c.ChatJID != it.ChatJID {
			continue
		}
		d := c.CreatedAt.Unix() - it.CreatedAt
		if d < -3*86400 || d > 3*86400 {
			continue
		}
		out = append(out, fmt.Sprintf("[%s] %s", c.Direction, c.Title))
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func renderMessages(msgs []*store.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		prefix := m.SenderName
		if m.IsFromMe {
			prefix = "[ME]"
		}
		content := m.Content
		if len(content) > 500 {
			content = content[:500] + "…"
		}
		fmt.Fprintf(&sb, "[%s] %s: %s\n", m.Timestamp.Format("Jan 2 3:04PM"), prefix, content)
	}
	return sb.String()
}

func buildJudgePrompt(it *SampledItem, msgs []*store.Message, siblings []string) string {
	var sb strings.Builder
	sb.WriteString(`You are auditing a WhatsApp commitment tracker used by a CEO. The extraction pipeline turned chat messages into "commitments". Judge ONE extracted commitment against the source messages.

The user's quality bar (tenets): a commitment is REAL only if it is consequence-driven — a specific person is waiting on it, there is a real deadline, or dropping it carries relationship/business cost. NOT commitments: callback promises ("I'll call you back", "will call when I land"), vague agreements ("will look into it", "will revert"), meeting/event attendance (calendar's job, e.g. "attend leadership meeting every Tuesday"), ephemeral micro-coordination ("on my way", "reaching in 10"), opinions or FYIs, and near-duplicate re-extractions of an already-tracked promise.

Judge these dimensions:
1. real_commitment (bool): consequence-driven promise vs noise per the tenets above.
2. noise_category (string): only when real_commitment=false, one of: "callback", "vague-agreement", "meeting-attendance", "micro-coordination", "duplicate", "opinion-not-promise", "other". Empty string when real.
3. attribution_ok (bool): direction is correct ("you_owe" = [ME] made the promise; "they_owe" = the other person did) AND person_name identifies the right counterparty. If the source messages are missing/ambiguous, judge from what's available; default true only if nothing contradicts it.
4. title_quality (string): "good" = specific, self-contained, actionable — would make sense two weeks later with no chat context (names the deliverable and counterparty); "acceptable" = understandable but missing specificity; "poor" = vague, context-dependent, or misleading.
5. significance_ok (bool): the assigned significance matches — HIGH = deadlines/financial/legal/board/investor-level; MEDIUM = professional follow-ups, documents, intros.
6. anonymized_pattern (string): a one-line description of the underlying message pattern with ALL real names, companies, and identifying details replaced by placeholders like PERSON_A / COMPANY_X. Example: "PERSON_A says 'will call you after landing' — a callback promise".
7. notes (string): one short sentence of justification, no real names.

Return ONLY JSON:
{"real_commitment": bool, "noise_category": "...", "attribution_ok": bool, "title_quality": "...", "significance_ok": bool, "anonymized_pattern": "...", "notes": "..."}

EXTRACTED COMMITMENT UNDER REVIEW:
`)
	fmt.Fprintf(&sb, "- title: %s\n- context: %s\n- direction: %s\n- person_name: %s\n- significance: %s\n- due_hint: %s\n- source_quote: %s\n- chat type: %s\n",
		it.Title, it.Context, it.Direction, it.PersonName, it.Significance, it.DueHint, it.SourceQuote, chatType(it.IsGroup))
	if len(siblings) > 0 {
		sb.WriteString("\nOTHER COMMITMENTS EXTRACTED FROM THE SAME CHAT WITHIN ±3 DAYS (for duplicate detection):\n")
		for _, s := range siblings {
			sb.WriteString("- " + s + "\n")
		}
	}
	sb.WriteString("\nSOURCE MESSAGES (window around the extraction; [ME] = the user):\n")
	if len(msgs) == 0 {
		sb.WriteString("(no source messages could be reconstructed — judge from the commitment fields alone and note this)\n")
	} else {
		sb.WriteString(renderMessages(msgs))
	}
	return sb.String()
}

func chatType(group bool) string {
	if group {
		return "group"
	}
	return "1:1"
}

// ---------- phase: dupes ----------

func runDupes(db *store.DB, out string) {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	all, err := db.GetCommitments("")
	if err != nil {
		log.Fatal(err)
	}
	var cs []*store.Commitment
	for _, c := range all {
		if c.CreatedAt.After(cutoff) && (c.Significance == "high" || c.Significance == "medium") {
			cs = append(cs, c)
		}
	}
	art := DupesArtifact{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Threshold:   dedupeThreshold,
		WindowDays:  30,
		Total:       len(cs),
	}
	inChatDup := map[string]bool{}
	crossDup := map[string]bool{}
	patterns := map[string]bool{}

	for i := 0; i < len(cs); i++ {
		for j := i + 1; j < len(cs); j++ {
			a, b := cs[i], cs[j]
			if a.Direction != b.Direction {
				continue
			}
			if !store.TitlesSimilar(a.Title, b.Title, dedupeThreshold) {
				continue
			}
			if a.ChatJID == b.ChatJID {
				art.InChatDupPairs++
				inChatDup[a.ID], inChatDup[b.ID] = true, true
				if len(patterns) < 8 {
					patterns[redactTitle(a.Title)+"  ≈  "+redactTitle(b.Title)] = true
				}
			} else if a.PersonName != "" && a.PersonName == b.PersonName {
				art.CrossChatDupPairs++
				crossDup[a.ID], crossDup[b.ID] = true, true
			}
		}
	}
	art.InChatDupItems = len(inChatDup)
	art.CrossChatDupItems = len(crossDup)
	for p := range patterns {
		art.ExamplePatterns = append(art.ExamplePatterns, p)
	}
	sort.Strings(art.ExamplePatterns)

	// open-only view
	openDup := map[string]bool{}
	var open []*store.Commitment
	for _, c := range cs {
		if c.Status == "open" {
			open = append(open, c)
		}
	}
	art.OpenOnlyTotal = len(open)
	for i := 0; i < len(open); i++ {
		for j := i + 1; j < len(open); j++ {
			if open[i].ChatJID == open[j].ChatJID && open[i].Direction == open[j].Direction &&
				store.TitlesSimilar(open[i].Title, open[j].Title, dedupeThreshold) {
				openDup[open[i].ID], openDup[open[j].ID] = true, true
			}
		}
	}
	art.OpenInChatDupItems = len(openDup)

	writeJSON(filepath.Join(out, "dupes.json"), art)
	log.Printf("dupes: total=%d in-chat dup items=%d (%.1f%%) cross-chat=%d open-only=%d/%d",
		art.Total, art.InChatDupItems, pct(art.InChatDupItems, art.Total), art.CrossChatDupItems,
		art.OpenInChatDupItems, art.OpenOnlyTotal)
}

// redactTitle masks capitalized words (likely names) with placeholders.
func redactTitle(t string) string {
	words := strings.Fields(t)
	for i, w := range words {
		if i == 0 {
			continue
		}
		r := []rune(w)
		if len(r) > 0 && r[0] >= 'A' && r[0] <= 'Z' {
			words[i] = "PERSON"
		}
	}
	return strings.Join(words, " ")
}

// ---------- phase: replay ----------

func runReplay(db *store.DB, out string) {
	apiKey := db.GetAPIKey()
	if apiKey == "" {
		log.Fatal("no API key in DB copy")
	}
	var base BaselineArtifact
	readJSON(filepath.Join(out, "baseline.json"), &base)
	if len(base.Items) == 0 {
		log.Fatal("baseline.json missing or empty — run -phase baseline first")
	}

	// Group sampled items into batches by chat + overlapping windows.
	type batchAgg struct {
		chatJID string
		items   []*SampledItem
		msgIDs  map[string]bool
		msgs    []*store.Message
	}
	var batches []*batchAgg
	for i := range base.Items {
		it := &base.Items[i]
		if !it.ContextFound {
			continue
		}
		msgs, err := db.GetMessagesAround(it.ChatJID, it.AnchorTS, contextWindow)
		if err != nil || len(msgs) == 0 {
			continue
		}
		var target *batchAgg
		for _, b := range batches {
			if b.chatJID != it.ChatJID {
				continue
			}
			for _, m := range msgs {
				if b.msgIDs[m.ID] {
					target = b
					break
				}
			}
			if target != nil {
				break
			}
		}
		if target == nil {
			target = &batchAgg{chatJID: it.ChatJID, msgIDs: map[string]bool{}}
			batches = append(batches, target)
		}
		target.items = append(target.items, it)
		for _, m := range msgs {
			if !target.msgIDs[m.ID] {
				target.msgIDs[m.ID] = true
				target.msgs = append(target.msgs, m)
			}
		}
	}
	log.Printf("replay: %d batches from %d baseline items", len(batches), len(base.Items))

	ctx := context.Background()
	extractionModel := store.FallbackModel // haiku — matches production extraction
	art := ReplayArtifact{
		GeneratedAt:     time.Now().Format(time.RFC3339),
		ExtractionModel: extractionModel,
		JudgeModel:      judgeModel,
	}

	// track saved titles per chat to simulate cross-batch save-time dedupe
	savedPerChat := map[string][]string{}

	for bi, b := range batches {
		sort.Slice(b.msgs, func(i, j int) bool { return b.msgs[i].Timestamp.Before(b.msgs[j].Timestamp) })
		rb := ReplayBatch{
			Key:     fmt.Sprintf("batch-%02d", bi),
			ChatJID: b.chatJID,
		}
		for _, m := range b.msgs {
			rb.MsgIDs = append(rb.MsgIDs, m.ID)
		}
		for _, it := range b.items {
			rb.BaselineIDs = append(rb.BaselineIDs, it.ID)
		}

		prompt := extraction.BuildExtractionPrompt(b.msgs, nil, "")
		resp, err := callWithRetry(ctx, func() (string, error) {
			return extraction.CallClaude(ctx, apiKey, extractionModel, prompt)
		})
		if err != nil {
			rb.ExtractionErr = err.Error()
			art.Batches = append(art.Batches, rb)
			log.Printf("batch %d/%d extraction error: %v", bi+1, len(batches), err)
			continue
		}
		var result struct {
			Commitments []ReplayExtraction `json:"commitments"`
		}
		if err := json.Unmarshal([]byte(extraction.ExtractJSON(resp)), &result); err != nil {
			rb.ExtractionErr = "parse: " + err.Error()
			art.Batches = append(art.Batches, rb)
			continue
		}

		// simulate the pipeline's significance gate + save-time dedupe
		for i := range result.Commitments {
			ec := &result.Commitments[i]
			sig := ec.Significance
			if sig != "high" && sig != "medium" && sig != "low" {
				sig = "medium"
				ec.Significance = sig
			}
			if sig == "low" {
				ec.DroppedLow = true
				continue
			}
			for _, prev := range savedPerChat[b.chatJID] {
				if store.TitlesSimilar(ec.Title, prev, dedupeThreshold) {
					ec.DroppedDupe = true
					break
				}
			}
			if !ec.DroppedDupe {
				savedPerChat[b.chatJID] = append(savedPerChat[b.chatJID], ec.Title)
			}
		}
		rb.Extractions = result.Commitments

		// judge: precision of surviving extractions + coverage of baseline-real items
		var kept []ReplayExtraction
		for _, e := range rb.Extractions {
			if !e.DroppedLow && !e.DroppedDupe {
				kept = append(kept, e)
			}
		}
		var realBaseline []*SampledItem
		for _, it := range b.items {
			if it.Judgment != nil && it.Judgment.RealCommitment {
				realBaseline = append(realBaseline, it)
			}
		}
		if len(kept) > 0 || len(realBaseline) > 0 {
			jp := buildBatchJudgePrompt(b.msgs, kept, realBaseline)
			jresp, err := callWithRetry(ctx, func() (string, error) {
				return callJudge(ctx, apiKey, jp, 3000)
			})
			if err != nil {
				rb.JudgeErr = err.Error()
			} else if err := applyBatchJudgment(&rb, kept, realBaseline, jresp); err != nil {
				rb.JudgeErr = err.Error()
			}
		}
		art.Batches = append(art.Batches, rb)
		log.Printf("batch %d/%d: %d extracted, %d kept, %d baseline-real", bi+1, len(batches), len(rb.Extractions), len(kept), len(realBaseline))
		// Persist after every batch — a long run must not lose completed work.
		writeJSON(filepath.Join(out, "replay.json"), art)
	}

	writeJSON(filepath.Join(out, "replay.json"), art)
	log.Printf("wrote %s", filepath.Join(out, "replay.json"))
}

func buildBatchJudgePrompt(msgs []*store.Message, kept []ReplayExtraction, realBaseline []*SampledItem) string {
	var sb strings.Builder
	sb.WriteString(`You are auditing a WhatsApp commitment tracker used by a CEO. A NEW extraction pipeline was run over the source messages below. Do two things.

TASK 1 — judge each NEW extraction (list A) on the tenets: a commitment is REAL only if consequence-driven (someone waiting, real deadline, relationship cost). Noise: callback promises, vague agreements, meeting/event attendance, micro-coordination, opinions/FYIs, duplicates. For each item in list A return: real_commitment (bool), noise_category ("" or one of callback|vague-agreement|meeting-attendance|micro-coordination|duplicate|opinion-not-promise|other), attribution_ok (bool: direction "you_owe" means [ME] promised), title_quality (good|acceptable|poor: good = self-contained, would make sense 2 weeks later), significance_ok (bool), anonymized_pattern (one line, real names replaced with PERSON_A etc), notes (short, no real names).

TASK 2 — for each OLD commitment in list B (all judged real in a prior audit), say whether the new pipeline still captures it: covered=true if some item in list A represents the same promise (paraphrase is fine), else false.

Return ONLY JSON:
{"a": [{"index": 0, "real_commitment": true, "noise_category": "", "attribution_ok": true, "title_quality": "good", "significance_ok": true, "anonymized_pattern": "...", "notes": "..."}, ...],
 "b": [{"id": "...", "covered": true}, ...]}

LIST A — NEW EXTRACTIONS:
`)
	for i, e := range kept {
		fmt.Fprintf(&sb, "%d. title=%q direction=%s person=%q significance=%s due_hint=%q quote=%q\n",
			i, e.Title, e.Direction, e.PersonName, e.Significance, e.DueHint, e.SourceQuote)
	}
	if len(kept) == 0 {
		sb.WriteString("(none — the new pipeline extracted nothing from this batch)\n")
	}
	sb.WriteString("\nLIST B — OLD COMMITMENTS (judged real; check coverage):\n")
	for _, it := range realBaseline {
		fmt.Fprintf(&sb, "- id=%s title=%q direction=%s person=%q\n", it.ID, it.Title, it.Direction, it.PersonName)
	}
	if len(realBaseline) == 0 {
		sb.WriteString("(none)\n")
	}
	sb.WriteString("\nSOURCE MESSAGES ([ME] = the user):\n")
	sb.WriteString(renderMessages(msgs))
	return sb.String()
}

func applyBatchJudgment(rb *ReplayBatch, kept []ReplayExtraction, realBaseline []*SampledItem, resp string) error {
	var parsed struct {
		A []struct {
			Index int `json:"index"`
			Judgment
		} `json:"a"`
		B []struct {
			ID      string `json:"id"`
			Covered bool   `json:"covered"`
		} `json:"b"`
	}
	if err := json.Unmarshal([]byte(extraction.ExtractJSON(resp)), &parsed); err != nil {
		return fmt.Errorf("parse judge: %w", err)
	}
	// map kept indices back to rb.Extractions positions
	keptPos := []int{}
	for i, e := range rb.Extractions {
		if !e.DroppedLow && !e.DroppedDupe {
			keptPos = append(keptPos, i)
		}
	}
	for _, a := range parsed.A {
		if a.Index >= 0 && a.Index < len(keptPos) {
			j := a.Judgment
			rb.Extractions[keptPos[a.Index]].Judgment = &j
		}
	}
	rb.Coverage = map[string]bool{}
	for _, b := range parsed.B {
		rb.Coverage[b.ID] = b.Covered
	}
	// any baseline-real item the judge omitted counts as uncovered
	for _, it := range realBaseline {
		if _, ok := rb.Coverage[it.ID]; !ok {
			rb.Coverage[it.ID] = false
		}
	}
	return nil
}

// ---------- phase: report ----------

func runReport(out string) {
	var base BaselineArtifact
	var dupes DupesArtifact
	var replay ReplayArtifact
	haveBase := readJSONOK(filepath.Join(out, "baseline.json"), &base)
	haveDupes := readJSONOK(filepath.Join(out, "dupes.json"), &dupes)
	haveReplay := readJSONOK(filepath.Join(out, "replay.json"), &replay)

	var sb strings.Builder
	sb.WriteString("# Extraction Quality Evaluation\n\n")
	fmt.Fprintf(&sb, "Generated %s. Judge model: `%s`. Replay extraction model: `%s` (production).\n\n",
		time.Now().Format("2006-01-02 15:04"), judgeModel, store.FallbackModel)

	if haveBase {
		writeBaselineSection(&sb, &base)
	}
	if haveDupes {
		writeDupesSection(&sb, &dupes)
	}
	if haveReplay && haveBase {
		writeReplaySection(&sb, &base, &replay)
	}

	sb.WriteString(`## Caveats

- The judge is an LLM (single call per item, no self-consistency voting); judgments on borderline items carry noise. Spot checks suggest directionally reliable classification, but individual labels should not be treated as ground truth.
- Source-message reconstruction is a ±window around the anchor message; for items whose source message was not stored (or where the quote match failed at extraction time), the judge saw a window centered on creation time, which may miss the true source context.
- Phase 3 replays reconstructed windows, not the exact original unprocessed-message batches; batch boundaries differ from production (production batches are "whatever arrived since the last poll"), which can change what context the model sees.
- Recall coverage in Phase 3 is judged per batch by the LLM; a "covered" verdict accepts paraphrases, so before/after title wording is not compared literally.
- The duplicate scan uses Jaccard title similarity (threshold ` + fmt.Sprintf("%.1f", dedupeThreshold) + `); it misses semantic duplicates with disjoint wording and can flag distinct-but-similar tasks as duplicates.
- Sample is stratified but small (~80); subgroup percentages have wide confidence intervals.
`)

	path := filepath.Join(out, "REPORT.md")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", path)
}

func writeBaselineSection(sb *strings.Builder, base *BaselineArtifact) {
	judged := 0
	real := 0
	attrBad := 0
	sigBad := 0
	noise := map[string]int{}
	titleQ := map[string]int{}
	patterns := map[string][]string{}
	byCell := map[string][2]int{} // cell -> [real, judged]
	for _, it := range base.Items {
		if it.Judgment == nil {
			continue
		}
		judged++
		j := it.Judgment
		cell := fmt.Sprintf("%s / %s / %s", it.Direction, chatType(it.IsGroup), it.Significance)
		c := byCell[cell]
		c[1]++
		if j.RealCommitment {
			real++
			c[0]++
		} else {
			cat := j.NoiseCategory
			if cat == "" {
				cat = "other"
			}
			noise[cat]++
			if len(patterns[cat]) < 3 && j.AnonymizedPattern != "" {
				patterns[cat] = append(patterns[cat], j.AnonymizedPattern)
			}
		}
		byCell[cell] = c
		if !j.AttributionOK {
			attrBad++
		}
		if !j.SignificanceOK {
			sigBad++
		}
		q := j.TitleQuality
		if q == "" {
			q = "unrated"
		}
		titleQ[q]++
	}
	fmt.Fprintf(sb, "## Phase 1 — Baseline (%d sampled, %d judged)\n\n", len(base.Items), judged)
	fmt.Fprintf(sb, "| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(sb, "| Precision (judged real) | **%d/%d = %.1f%%** |\n", real, judged, pct(real, judged))
	fmt.Fprintf(sb, "| Attribution error rate | %d/%d = %.1f%% |\n", attrBad, judged, pct(attrBad, judged))
	fmt.Fprintf(sb, "| Significance mismatch rate | %d/%d = %.1f%% |\n", sigBad, judged, pct(sigBad, judged))
	fmt.Fprintf(sb, "| Title quality good / acceptable / poor | %d / %d / %d |\n\n", titleQ["good"], titleQ["acceptable"], titleQ["poor"])

	sb.WriteString("### Precision by segment\n\n| Segment | Real / Judged | Precision |\n|---|---|---|\n")
	var cells []string
	for k := range byCell {
		cells = append(cells, k)
	}
	sort.Strings(cells)
	for _, k := range cells {
		c := byCell[k]
		fmt.Fprintf(sb, "| %s | %d / %d | %.0f%% |\n", k, c[0], c[1], pct(c[0], c[1]))
	}

	sb.WriteString("\n### Noise taxonomy\n\n| Category | Count | Share of noise |\n|---|---|---|\n")
	totalNoise := judged - real
	var cats []string
	for k := range noise {
		cats = append(cats, k)
	}
	sort.Slice(cats, func(i, j int) bool { return noise[cats[i]] > noise[cats[j]] })
	for _, k := range cats {
		fmt.Fprintf(sb, "| %s | %d | %.0f%% |\n", k, noise[k], pct(noise[k], totalNoise))
	}
	sb.WriteString("\nExample patterns (redacted):\n\n")
	for _, k := range cats {
		for _, p := range patterns[k] {
			fmt.Fprintf(sb, "- *%s*: %s\n", k, p)
		}
	}
	sb.WriteString("\n")
}

func writeDupesSection(sb *strings.Builder, d *DupesArtifact) {
	fmt.Fprintf(sb, "## Duplicate rate (programmatic, Jaccard ≥ %.1f, %d-day window)\n\n", d.Threshold, d.WindowDays)
	fmt.Fprintf(sb, "| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(sb, "| Commitments scanned (high+medium) | %d |\n", d.Total)
	fmt.Fprintf(sb, "| Items in ≥1 in-chat near-duplicate pair | %d (%.1f%%) — %d pairs |\n", d.InChatDupItems, pct(d.InChatDupItems, d.Total), d.InChatDupPairs)
	fmt.Fprintf(sb, "| Items in ≥1 cross-chat same-person duplicate pair | %d (%.1f%%) — %d pairs |\n", d.CrossChatDupItems, pct(d.CrossChatDupItems, d.Total), d.CrossChatDupPairs)
	fmt.Fprintf(sb, "| Open-only in-chat duplicate items | %d of %d open (%.1f%%) |\n\n", d.OpenInChatDupItems, d.OpenOnlyTotal, pct(d.OpenInChatDupItems, d.OpenOnlyTotal))
	if len(d.ExamplePatterns) > 0 {
		sb.WriteString("Example near-duplicate title pairs (names redacted):\n\n")
		for _, p := range d.ExamplePatterns {
			fmt.Fprintf(sb, "- %s\n", p)
		}
		sb.WriteString("\n")
	}
}

func writeReplaySection(sb *strings.Builder, base *BaselineArtifact, r *ReplayArtifact) {
	// before numbers
	bJudged, bReal := 0, 0
	bNoise := map[string]int{}
	for _, it := range base.Items {
		if it.Judgment == nil {
			continue
		}
		bJudged++
		if it.Judgment.RealCommitment {
			bReal++
		} else {
			cat := it.Judgment.NoiseCategory
			if cat == "" {
				cat = "other"
			}
			bNoise[cat]++
		}
	}

	// after numbers
	kept, judgedKept, realKept := 0, 0, 0
	droppedLow, droppedDupe := 0, 0
	aNoise := map[string]int{}
	titleQ := map[string]int{}
	attrBad := 0
	covered, coverTotal := 0, 0
	extractErrs := 0
	dupAmongKept := 0
	var keptTitlesByChat = map[string][]string{}
	for _, b := range r.Batches {
		if b.ExtractionErr != "" {
			extractErrs++
		}
		for _, e := range b.Extractions {
			if e.DroppedLow {
				droppedLow++
				continue
			}
			if e.DroppedDupe {
				droppedDupe++
				continue
			}
			kept++
			for _, prev := range keptTitlesByChat[b.ChatJID] {
				if store.TitlesSimilar(e.Title, prev, dedupeThreshold) {
					dupAmongKept++
					break
				}
			}
			keptTitlesByChat[b.ChatJID] = append(keptTitlesByChat[b.ChatJID], e.Title)
			if e.Judgment != nil {
				judgedKept++
				if e.Judgment.RealCommitment {
					realKept++
				} else {
					cat := e.Judgment.NoiseCategory
					if cat == "" {
						cat = "other"
					}
					aNoise[cat]++
				}
				if !e.Judgment.AttributionOK {
					attrBad++
				}
				q := e.Judgment.TitleQuality
				if q == "" {
					q = "unrated"
				}
				titleQ[q]++
			}
		}
		for _, ok := range b.Coverage {
			coverTotal++
			if ok {
				covered++
			}
		}
	}

	fmt.Fprintf(sb, "## Phase 3 — Replay through the new pipeline (%d batches, %d extraction errors)\n\n", len(r.Batches), extractErrs)
	fmt.Fprintf(sb, "New pipeline = updated prompt + save-time dedupe (Jaccard ≥ %.1f), extraction on `%s`.\n\n", dedupeThreshold, r.ExtractionModel)
	fmt.Fprintf(sb, "| Metric | Before (baseline) | After (replay) |\n|---|---|---|\n")
	fmt.Fprintf(sb, "| Precision (judged real) | %.1f%% (%d/%d) | **%.1f%%** (%d/%d) |\n", pct(bReal, bJudged), bReal, bJudged, pct(realKept, judgedKept), realKept, judgedKept)
	fmt.Fprintf(sb, "| Attribution error rate | — see Phase 1 | %.1f%% (%d/%d) |\n", pct(attrBad, judgedKept), attrBad, judgedKept)
	fmt.Fprintf(sb, "| Title quality good/acceptable/poor | — see Phase 1 | %d / %d / %d |\n", titleQ["good"], titleQ["acceptable"], titleQ["poor"])
	fmt.Fprintf(sb, "| Save-time dedupe drops | n/a | %d (plus %d low-significance drops) |\n", droppedDupe, droppedLow)
	fmt.Fprintf(sb, "| Residual near-duplicates among kept | n/a | %d of %d (%.1f%%) |\n", dupAmongKept, kept, pct(dupAmongKept, kept))
	fmt.Fprintf(sb, "| **Recall of baseline-real items** | 100%% (by construction) | **%.1f%%** (%d/%d covered) |\n\n", pct(covered, coverTotal), covered, coverTotal)

	sb.WriteString("### Noise categories, before → after\n\n| Category | Before | After |\n|---|---|---|\n")
	catSet := map[string]bool{}
	for k := range bNoise {
		catSet[k] = true
	}
	for k := range aNoise {
		catSet[k] = true
	}
	var cats []string
	for k := range catSet {
		cats = append(cats, k)
	}
	sort.Strings(cats)
	for _, k := range cats {
		fmt.Fprintf(sb, "| %s | %d | %d |\n", k, bNoise[k], aNoise[k])
	}
	sb.WriteString("\n")
}

// ---------- API plumbing ----------

// callJudge uses its own HTTP path (rather than extraction.CallClaude) so the
// judge can have a larger max_tokens than the production pipeline's 2048.
func callJudge(ctx context.Context, apiKey, prompt string, maxTokens int) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      judgeModel,
		"max_tokens": maxTokens,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 429 {
		return "", fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("api error %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", err
	}
	for _, c := range parsed.Content {
		if c.Type == "text" && c.Text != "" {
			return c.Text, nil
		}
	}
	return "", fmt.Errorf("no text content in response")
}

func callWithRetry(ctx context.Context, fn func() (string, error)) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		out, err := fn()
		if err == nil {
			return out, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "rate limit") || strings.Contains(err.Error(), "overloaded") {
			wait := time.Duration(20*(attempt+1)) * time.Second
			log.Printf("retryable error, waiting %v: %v", wait, err)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return "", err
	}
	return "", lastErr
}

// ---------- helpers ----------

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func writeJSON(path string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
}

func readJSON(path string, v any) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		log.Fatalf("parse %s: %v", path, err)
	}
}

func readJSONOK(path string, v any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}
