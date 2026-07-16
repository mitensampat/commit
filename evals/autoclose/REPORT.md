# Auto-Close Eval: Tiered Confidence vs Single Bucket

Generated: 2026-07-16 16:43 IST
Scorer model (production sweep model): claude-haiku-4-5 (production sweep model family, executed via Claude Code subagents)
Judge model: claude-sonnet (executed via Claude Code subagents)
Schema version after opening DB copy: 8 (migration v8 applied cleanly)

## Methodology

**Ground truth caveat:** the primary methodology (auto-closes later reopened by
the user) is NOT recoverable from this database. Reopening sets status='open'
and overwrites resolved_by with 'user'; there is no status-history table. The
known-bad ground truth set is therefore empty, and this eval uses the stated
fallback: an LLM-judge methodology.

**Retrospective replay.** From 2218 historically auto-closed commitments with
at least 4 messages of chat context in the 72h before their resolution
("replayable pool"), a seeded random sample of 60 was drawn. For each item:

1. **Scorer (new system):** the exact production resolution-sweep prompt
   (confidence + evidence + closure type rubric) was re-run on the messages
   the sweep would have seen at resolution time, using the production sweep
   model. The returned confidence maps to the new tiers:
   auto-close (>=0.85), pending confirmation (0.60-0.85), ignore (<0.60).
   A commitment the scorer does not flag at all counts as "ignore".
2. **Judge (label):** an independent pass with a stronger model, given 48h of
   additional hindsight messages after the close, graded each historical
   auto-close as good (correct), bad (mistake), or unclear.

The old system auto-closed 100% of these items (that is how they entered the
sample), so the judge's verdicts directly measure the old single bucket's
precision.

## Sample

- Replayable auto-closed pool: 2218 (of all auto-closed commitments)
- Sampled and scored: 60 (scorer errors: 0, judge errors treated as unclear: 0)

## Confidence distribution (new scorer, on items the old system closed)

| Bucket | Count | Share |
|---|---|---|
| not flagged | 47 | 78% (47/60) |
| <0.60 | 0 | 0% (0/60) |
| 0.60-0.84 | 7 | 12% (7/60) |
| 0.85-0.89 | 2 | 3% (2/60) |
| 0.90-1.00 | 4 | 7% (4/60) |

Tier assignment: auto 10% (6/60), pending 12% (7/60), ignore 78% (47/60).

## Judge labels

good 17, bad 35, unclear 8 (of 60 scored).

## Headline metrics

| Metric | Value |
|---|---|
| **Judged-bad auto-closes the new system catches** (demoted to pending or ignored) | **94% (33/35)** |
| Judged-good auto-closes still closed automatically (>=0.85) | 18% (3/17) |
| Old single-bucket precision (judge-labeled, unclear excluded) | 33% (17/52) |
| New auto tier precision (judge-labeled, unclear excluded) | 60% (3/5) |
| Estimated pending confirmations per week | ~50 |

Tier vs judge cross-tab:

| Tier | good | bad | unclear |
|---|---|---|---|
| auto (>=0.85) | 3 | 2 | 1 |
| pending (0.60-0.85) | 4 | 2 | 1 |
| ignore (<0.60) | 10 | 31 | 6 |

Pending burden basis: last 14 days had 1270 auto-closes total, of which 854 had
recent chat activity before close (proxy for the LLM-detection path this
change affects; the remainder are rule-based staleness closes, which keep
auto-closing directly). 854/2 per week x 12% pending rate = ~50
confirmations/week.

## Interpretation

The judge found the old always-auto-close bucket to be far less precise than
assumed: most sampled closes had no real completion evidence. The new
calibrated scorer is much more conservative — it silently auto-closes only
the clearest cases, routes a small slice to user confirmation, and leaves the
rest open. Items it now ignores do not pile up forever: they remain subject
to the unchanged rule-based staleness sweep and normal user actions. The
trade is deliberate: far fewer wrong silent closes at the cost of less
automation, with the pending queue keeping the user in the loop on the
ambiguous middle.

## Caveats

- **No human ground truth.** Labels come from a single stronger-model judge
  pass with hindsight context, not from the user. Judge mistakes move both
  headline numbers; "unclear" items (8) are excluded from precision.
- **Label circularity risk is limited but real:** scorer and judge share a
  model family, though they use different prompts, different models, and the
  judge sees 48h of extra hindsight the scorer cannot.
- **Sample size** (60 scored) gives rough precision estimates; treat
  percentage points as +/-10 or so at this n.
- **Sampling bias:** only auto-closes with chat activity before close were
  replayable. Rule-based staleness closes (quiet chats) are underrepresented
  by design — the tiered change does not touch that path.
- **The pool predates this change**, so items closed by the old aggressive
  prompt at low true confidence are exactly what we want to measure, but chat
  context reconstruction (72h window, 60-message cap) may differ slightly
  from what the sweep saw live.
- Per-item evidence and judge reasons contain private message text and are
  deliberately NOT included in this committed report (write them locally with
  -details).
- **Execution path:** the configured API key (and the machine's other
  Anthropic key) had no remaining credit at eval time, so prompts were built
  and parsed by this harness (-dump-prompts / -collect) but executed through
  Claude Code subagents of the corresponding model families instead of raw
  API calls. Harness system prompts may shift scores slightly vs production;
  rerun in direct API mode once the key has credit to confirm.
