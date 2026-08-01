# Extraction Quality Evaluation

Generated 2026-08-01. Judge model: `claude-sonnet-5`. Replay extraction model: `claude-haiku-4-5-20251001` (production). Baseline = 71 historical commitments (OLD prompt), stratified over the last 30 days; 67 judged. Replay = the same source-message batches re-run through the NEW prompt, judged on the same rubric.

## Verdict — DO NOT SHIP (yet)

The tighter prompt is a real quality win on precision, titles, and the noise it was designed to kill. But it fails the one make-or-break test: **recall of genuine commitments is 91.7% (22 of 24 observable), below the 95% bar.** The two it dropped were both real, and one was HIGH-significance (a request to execute secondary-offering signature paperwork) — exactly the class of commitment that must never be silently lost. Precision you can tune later; a dropped board/financing commitment is the failure mode this product exists to prevent.

| Headline | Before (OLD prompt) | After (NEW prompt) |
|---|---|---|
| **Precision** (share judged a real commitment) | **43.3%** (29/67) | **59.4%** (41/69) |
| **Recall** of baseline-real items | 100% (by construction) | **91.7%** (22/24 covered) — **below 95% target** |
| Biggest noise-category reduction | — | **meeting-attendance 9 -> 1** |
| Title quality "good" | 8 / 67 | 38 / 69 |
| Attribution error rate | 38.8% | 31.9% |

**The one reason it doesn't ship:** recall < 95%, and the misses include a genuine HIGH-significance commitment. **What to fix:** the consequence test is over-firing on real deliverables that lack an explicit deadline in the immediate window. Add protected-class guidance / few-shot examples so HIGH-significance items (financing, board, legal, investor paperwork) are never dropped for "no stated consequence," then re-run the recall phase and clear >=95% before shipping.

### Recall, honestly

Of 29 baseline items judged REAL, **24 could be observed** in replay (their source window was reconstructable and their batch was judged): 22 re-extracted, **2 dropped -> 91.7%**. The 2 drops:

- **HIGH / you_owe / group** — "Request signature on secondary offering documents (excluding gap shares)." A genuine consequence-heavy financing task. This is the disqualifying miss.
- **MEDIUM / you_owe / 1:1** — "Post content to social media at the high-engagement time." Borderline, but judged real in baseline.

The remaining **5 real items had no coverage verdict and are excluded from the 91.7%** (not counted as drops): 3 because their source messages could not be reconstructed from the DB (a data/anchoring limitation, not a prompt decision), and 2 because the sonnet judge call returned a truncated/empty response for that batch (judge-side noise). None of these five is evidence the new prompt kept or dropped them — they are simply unobservable in this run.

With only 24 observations, 95% allows at most 1 miss (23/24 = 95.8%); we saw 2. The confidence interval is wide, so the *magnitude* is soft — but the rule is explicit and one miss is a HIGH item, so the direction is not in doubt: hold.

## Phase 1 — Baseline (71 sampled, 67 judged) — OLD prompt behavior

| Metric | Value |
|---|---|
| Precision (judged real) | **29/67 = 43.3%** |
| Attribution error rate | 26/67 = 38.8% |
| Significance mismatch rate | 35/67 = 52.2% |
| Title quality good / acceptable / poor | 8 / 41 / 18 |

### Precision by segment

| Segment | Real / Judged | Precision |
|---|---|---|
| they_owe / 1:1 / high | 1 / 5 | 20% |
| they_owe / 1:1 / medium | 2 / 9 | 22% |
| they_owe / group / high | 6 / 9 | 67% |
| they_owe / group / medium | 5 / 10 | 50% |
| you_owe / 1:1 / high | 3 / 7 | 43% |
| you_owe / 1:1 / medium | 5 / 12 | 42% |
| you_owe / group / high | 4 / 9 | 44% |
| you_owe / group / medium | 3 / 6 | 50% |

### Noise taxonomy (OLD)

| Category | Count | Share of noise |
|---|---|---|
| vague-agreement | 19 | 50% |
| meeting-attendance | 9 | 24% |
| duplicate | 4 | 11% |
| micro-coordination | 3 | 8% |
| opinion-not-promise | 2 | 5% |
| other | 1 | 3% |

Example patterns (redacted):

- *vague-agreement*: PERSON_A raises systemic cross-team compliance/legal gaps; [ME] replies 'I will drive prio on this' — a vague, undated intent to prioritize rather than a specific deliverable, and the direction is misattributed to PERSON_A instead of [ME].
- *vague-agreement*: PERSON_A says 'will get that to test end to end infra' — an ambiguous reference with no defined deliverable or deadline.
- *meeting-attendance*: PERSON_A asks CEO to schedule a meeting on a specific day to discuss progress toward a business milestone — a meeting request, not a deliverable commitment.
- *meeting-attendance*: PERSON_A, founder of a startup, requests a meeting with PERSON_B at PERSON_B's office on a specific weekday — a meeting scheduling request, not a consequence-driven commitment.
- *duplicate*: PERSON_A relays that a group of leaders decided to close out bonus payouts for a small set of ex-employees, closely overlapping an already-tracked commitment to calculate the same payouts.
- *micro-coordination*: PERSON_A says 'I'll send one in 10 mins' about a draft message — an ephemeral micro-coordination promise.
- *opinion-not-promise*: PERSON_A and PERSON_B brainstorm product strategy; PERSON_A says 'push CC full speed' as a strategic musing, not a specific deliverable owed to anyone.

## Duplicate rate (programmatic, Jaccard >= 0.5, 30-day window)

| Metric | Value |
|---|---|
| Commitments scanned (high+medium) | 1085 |
| Items in >=1 in-chat near-duplicate pair | 25 (2.3%) — 14 pairs |
| Items in >=1 cross-chat same-person duplicate pair | 30 (2.8%) — 20 pairs |
| Open-only in-chat duplicate items | 11 of 361 open (3.0%) |

Example near-duplicate title pairs (names redacted):

- Complete assigned board deck slides (infra & PERSON section)  ~  Complete assigned board deck slides (full section)
- Complete assigned board deck slides (section 7, 27, 28)  ~  Complete assigned board deck slides (full section)
- Discuss PERSON PERSON PERSON compliance and next steps  ~  Connect to discuss PERSON PERSON PERSON compliance and next steps
- Present PERSON growth plan in person  ~  Present PERSON growth plan
- Send calendar invite for event with address details  ~  Send calendar invite for PERSON meeting with office address
- Sign PERSON order form  ~  Sign order form

The programmatic in-chat duplicate rate is low (2.3%), and in the replay the save-time dedupe removed 1 near-duplicate, leaving **0 residual near-duplicates among 77 kept extractions**. Duplication is a minor problem in the current corpus; the save-time dedupe is cheap insurance rather than a headline fix.

## Phase 3 — Replay through the new pipeline (68 batches, 0 extraction errors)

New pipeline = updated prompt + save-time dedupe (Jaccard >= 0.5), extraction on `claude-haiku-4-5-20251001` (matches production).

| Metric | Before (baseline) | After (replay) |
|---|---|---|
| Precision (judged real) | 43.3% (29/67) | **59.4%** (41/69) |
| Attribution error rate | 38.8% (26/67) | 31.9% (22/69) |
| Title quality good/acceptable/poor | 8 / 41 / 18 | 38 / 24 / 7 |
| Save-time dedupe drops | n/a | 1 (plus 0 low-significance drops) |
| Residual near-duplicates among kept | n/a | 0 of 77 (0.0%) |
| **Recall of baseline-real items** | 100% (by construction) | **91.7%** (22/24 covered) |

### Noise categories, before -> after

| Category | Before | After |
|---|---|---|
| callback | 0 | 6 |
| duplicate | 4 | 1 |
| meeting-attendance | 9 | 1 |
| micro-coordination | 3 | 1 |
| opinion-not-promise | 2 | 0 |
| other | 1 | 2 |
| vague-agreement | 19 | 17 |

**Reading the noise table.** Before/after sets are different extractions (OLD historical vs NEW replay), so counts show composition, not item-for-item deltas. What clearly improved: **meeting-attendance 9 -> 1** (the biggest reduction — the explicit "attendance is the calendar's job" rule works), duplicate 4 -> 1, micro-coordination 3 -> 1, opinion-not-promise 2 -> 0. What did NOT improve: **vague-agreement barely moved (19 -> 17) and is now ~60% of all remaining noise** — the consequence test is not reliably catching undated "I'll drive/look into/revert" intents. A new **callback bucket (0 -> 6)** appeared: the NEW haiku extraction surfaces "I'll call X" promises the OLD historical set did not, so the callback rule is not fully holding on the extraction model. Net precision still rose 16 points because the categories it does kill (meeting-attendance, duplicates, opinions) were large.

### What the changes clearly bought

- **Self-contained titles**: "good" titles jumped 8 -> 38 and "poor" fell 18 -> 7. This is the strongest, least ambiguous win — titles now name the deliverable and counterparty and would read correctly two weeks later.
- **Meeting/attendance and scheduling noise** largely eliminated.
- **Precision +16 points** (43% -> 59%).

### What blocks shipping

- **Recall 91.7% < 95%**, including one dropped HIGH-significance financing commitment.
- **vague-agreement noise essentially unchanged** and now dominant — the consequence test is too lenient on undated intents while, paradoxically, too aggressive on real-but-undated deliverables (the recall drops). Both point at the same lever: the consequence test needs deadline-independent handling of significance.

## Caveats

- **Recall denominator is small (24 observable of 29 real).** At n=24, one item is ~4 points, so 91.7% vs the 95% line is a one-item difference. The direction (hold) is driven by the rule plus the HIGH-significance miss, not by a large margin — a larger recall sample is warranted before a final call.
- The judge is a single LLM call per item (no self-consistency voting); borderline labels carry noise. 4 baseline items and 2 replay batches failed to return a parseable judgment and are excluded.
- Source-message reconstruction is a +/-12-message window around the anchor; items whose source message was not stored were judged/omitted from a creation-time window that may miss true context. 3 real items were unreconstructable and excluded from recall.
- Replay reconstructs windows, not the exact original poll-time batches; batch boundaries differ from production, changing what context the model and dedupe see.
- Before/after noise counts come from different extraction sets and are compositional, not paired deltas.
- The duplicate scan uses Jaccard title similarity (threshold 0.5); it misses semantic duplicates with disjoint wording and can flag distinct-but-similar tasks as duplicates.
- Sample is stratified but small; subgroup percentages (esp. per-segment precision) have wide confidence intervals.
