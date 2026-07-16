# Auto-Close Eval: Handoff-Only Auto-Close vs Single Bucket

Generated: 2026-07-16 17:29 IST
Scorer model (production sweep model): claude-haiku-4-5-20251001
Judge model: claude-sonnet-5
Schema version after opening DB copy: 8 (migration v8 applied cleanly)

## Policy under test (final)

- **Silent auto-close only for completed handoffs**: the chat shows BOTH the
  delivery by the committer AND the recipient acknowledging it
  (closure_type "recipient_acknowledgment", confidence >= 0.85).
- **Everything else detected at >= 0.60 becomes a suggestion** ("These look
  done") — one-sided "I sent it" claims, indirect references, moot-by-events,
  call-fulfills-commitment. The UI shows at most 5, highest confidence first;
  suggestions expire silently after 7 days (recorded as reason='expired').
- Below 0.60: ignored. The rule-based staleness sweep is unchanged.

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

**Retrospective replay.** From 2268 historically auto-closed commitments with
at least 4 messages of chat context in the 72h before their resolution
("replayable pool"), a seeded random sample of 60 was drawn. For each item:

1. **Scorer (new system):** the exact production resolution-sweep prompt
   (confidence + evidence + closure-type rubric, handoff-gated calibration)
   was re-run on the messages the sweep would have seen at resolution time,
   using the production sweep model. Detections map to the policy above.
   A commitment the scorer does not flag at all counts as "ignore".
2. **Judge (label):** an independent pass with a stronger model, given 48h of
   additional hindsight messages after the close, graded each historical
   auto-close as good (correct), bad (mistake), or unclear.

The old system auto-closed 100% of these items (that is how they entered the
sample), so the judge's verdicts directly measure the old single bucket's
precision.

## Sample

- Replayable auto-closed pool: 2268 (of all auto-closed commitments)
- Sampled and scored: 60 (scorer errors: 0, judge errors treated as unclear: 0)

## Scorer distribution (on items the old system closed)

| Bucket | Count | Share |
|---|---|---|
| not flagged | 14 | 23% (14/60) |
| <0.60 | 5 | 8% (5/60) |
| 0.60-0.84 | 37 | 62% (37/60) |
| 0.85+ one-sided | 1 | 2% (1/60) |
| 0.85+ handoff | 3 | 5% (3/60) |

Tier assignment under the final policy: auto 5% (3/60), suggest 63% (38/60), ignore 32% (19/60).

## Judge labels

good 6, bad 33, unclear 21 (of 60 scored).

## Headline metrics (old vs new)

| Metric | Value |
|---|---|
| **Judged-bad closes that would STILL close silently** (target ~0) | **0% (0/33)** |
| Judged-bad closes caught (suggested or left open instead) | 100% (33/33) |
| Old single-bucket precision (judge-labeled, unclear excluded) | 15% (6/39) |
| Two-sided-handoff auto-closes judged correct (new auto tier precision) | n/a |
| Estimated suggestions per week (raw rate) | ~286 |
| Displayed suggestion burden | at most 5 visible at a time; 7-day expiry |

Tier vs judge cross-tab:

| Tier | good | bad | unclear |
|---|---|---|---|
| auto (handoff, >=0.85) | 0 | 0 | 3 |
| suggest (>=0.60, non-handoff or <0.85) | 6 | 18 | 14 |
| ignore (<0.60) | 0 | 15 | 4 |

Suggestion burden basis: last 14 days had 1321 auto-closes total, of which 902 had
recent chat activity before close (proxy for the LLM-detection path this
change affects; the remainder are rule-based staleness closes, which keep
auto-closing directly). 902/2 per week x 63% suggest rate = ~286 raw
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
  headline numbers; "unclear" items (21) are excluded from precision.
- **The auto tier is small by design**, so its precision estimate rests on
  very few judge-labeled items — read it as directional, not exact.
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
