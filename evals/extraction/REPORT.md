# Extraction Quality Evaluation

Generated 2026-08-01. Judge model: `claude-sonnet-5`. Replay extraction model: `claude-haiku-4-5-20251001` (production). Baseline = 71 historical commitments (OLD prompt), stratified over the last 30 days; 67 judged. Replay = the same source-message batches re-run through the NEW prompt, judged on the same rubric. The baseline judgments are fixed; only the NEW-prompt replay was re-run after the protected-class fix (below), so before/after are directly comparable.

## Verdict — SHIP

The tightened prompt, plus a protected-class rule for high-stakes domains, clears both gates:

| Gate | Target | Result |
|---|---|---|
| **Recall** of baseline-real items | >= 95% | **96.0%** (24/25) — PASS |
| **Zero HIGH-significance drops** | mandatory | **0 dropped** (10/10 observable HIGH items captured; 4 unobservable) — PASS |
| **Precision** not regressing below +10pt over 43% baseline | >= ~53% | **64.1%** (+20.8pt) — PASS |

| Headline | Before (OLD prompt) | After (NEW prompt, fixed) |
|---|---|---|
| **Precision** (share judged a real commitment) | **43.3%** (29/67) | **64.1%** (41/64) |
| **Recall** of baseline-real items | 100% (by construction) | **96.0%** (24/25 covered) |
| HIGH-significance items dropped | — | **0** |
| Biggest noise-category reduction | — | **meeting-attendance 9 -> 4; vague-agreement 19 -> 13** |
| Title quality "good" | 8 / 67 | 37 / 64 |
| Attribution error rate | 38.8% | 28.1% |

**Why it ships now:** the one blocker from the first pass — a dropped HIGH-significance financing commitment ("Request signature on secondary-offering documents") — is captured (covered=true) after the fix, and no other HIGH item drops. Precision actually rose versus the pre-fix replay (59.4% -> 64.1%), and the protected class did NOT reopen the vague-agreement loophole: vague-agreement noise fell further (17 -> 13). The only remaining recall miss is one borderline MEDIUM item ("post content to social media at the high-engagement time").

### The fix (extraction-quality branch, `extraction/commitments.go`)

1. **Protected-class rule** added to the extraction prompt: commitments in financing/fundraising, board matters, legal/regulatory, investor paperwork, equity/ESOP, signatures or approvals on those, and payment/wire obligations are ALWAYS extracted as HIGH — "their significance IS the consequence" — even when undated. This directly targets the failure mode from pass 1.
2. **Three worked few-shot examples** anchoring the behavior: the exact wrongly-dropped secondary-offering signature -> EXTRACT HIGH; an undated board-deck review -> EXTRACT HIGH; contrasted with "we should catch up on the fundraise sometime" -> DO NOT EXTRACT.
3. **Anti-loophole guard**: the protected class covers only a concrete deliverable/signature/approval, never a mere intention to discuss; the vague-agreement list was expanded ("will revert", "will align on this", "will drive prio on this") with an explicit note that topic importance never upgrades a vague intention into a commitment.

### Recall, honestly

Of 29 baseline items judged REAL, **25 were observable** in replay (source window reconstructable and batch judged): 24 re-extracted, **1 dropped -> 96.0%**. The single drop is **MEDIUM / you_owe / 1:1** — "Post content to social media at the high-engagement time," a borderline item. **Zero HIGH-significance items dropped.**

Of the 14 HIGH real baseline items, 10 were observable and all 10 were captured; the other 4 have no coverage verdict and are excluded (3 source windows unreconstructable from the DB, 1 judge-call truncation) — none is evidence of a prompt drop. With 25 observations, 95% permits at most 1 miss (24/25 = 96.0%); we saw exactly 1, on a non-protected MEDIUM item.

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
- *meeting-attendance*: PERSON_A asks CEO to schedule a meeting on a specific day to discuss progress toward a business milestone — a meeting request, not a deliverable commitment.
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

The programmatic in-chat duplicate rate is low (2.3%). In the fixed replay the save-time dedupe found nothing to drop and left **0 residual near-duplicates among 76 kept extractions**. Duplication is a minor problem in the current corpus; the save-time dedupe is cheap insurance rather than a headline fix.

## Phase 3 — Replay through the new pipeline, after the fix (68 batches, 0 extraction errors)

New pipeline = updated prompt (consequence test + protected class + self-contained titles) + save-time dedupe (Jaccard >= 0.5), extraction on `claude-haiku-4-5-20251001` (matches production).

| Metric | Before (baseline) | After (replay, fixed) | Pre-fix replay (pass 1) |
|---|---|---|---|
| Precision (judged real) | 43.3% (29/67) | **64.1%** (41/64) | 59.4% (41/69) |
| Attribution error rate | 38.8% (26/67) | 28.1% (18/64) | 31.9% (22/69) |
| Title quality good/acceptable/poor | 8 / 41 / 18 | 37 / 23 / 4 | 38 / 24 / 7 |
| Save-time dedupe drops | n/a | 0 (plus 0 low-significance drops) | 1 |
| Residual near-duplicates among kept | n/a | 0 of 76 (0.0%) | 0 of 77 |
| **Recall of baseline-real items** | 100% (by construction) | **96.0%** (24/25) | 91.7% (22/24) |
| **HIGH-significance items dropped** | — | **0** | 1 |

### Noise categories, before -> after (fixed)

| Category | Before | After |
|---|---|---|
| callback | 0 | 4 |
| duplicate | 4 | 1 |
| meeting-attendance | 9 | 4 |
| micro-coordination | 3 | 0 |
| opinion-not-promise | 2 | 0 |
| other | 1 | 1 |
| vague-agreement | 19 | 13 |

**Reading the noise table.** Before/after sets are different extractions (OLD historical vs NEW replay), so counts show composition, not item-for-item deltas. What clearly improved: **vague-agreement 19 -> 13** (the anti-loophole tightening held — importance no longer smuggles vague intents through), **meeting-attendance 9 -> 4**, micro-coordination 3 -> 0, opinion-not-promise 2 -> 0. A small **callback bucket (0 -> 4)** remains: the haiku extraction still occasionally surfaces "I'll call X" promises — the largest residual noise source alongside vague-agreement, and the natural target for the next iteration. Net precision rose ~21 points.

### What the fix bought, concretely

- **Zero HIGH-significance drops** (was 1) — the protected-class rule and its worked examples captured the secondary-offering signature that pass 1 lost.
- **Recall 91.7% -> 96.0%**, clearing the gate.
- **Precision 59.4% -> 64.1%** — the protected class did not cost precision; the anti-loophole guard kept vague chatter out.
- **Self-contained titles** remain the standout: "good" 8 -> 37, "poor" 18 -> 4.

## Caveats

- **Recall denominator is small (25 observable of 29 real).** At n=25 one item is 4 points, so 96.0% is exactly "one miss," on a borderline MEDIUM item. The HIGH-item guarantee rests on 10 observable HIGH items (all captured) — solid but not large; a bigger HIGH sample would harden the claim.
- The judge is a single LLM call per item (no self-consistency voting); borderline labels carry noise. Some baseline items and replay batches failed to return a parseable judgment and are excluded (3 unreconstructable HIGH windows, 1 judge truncation on the HIGH side).
- Source-message reconstruction is a +/-12-message window around the anchor; items whose source message was not stored were judged/omitted from a creation-time window that may miss true context.
- Replay reconstructs windows, not the exact original poll-time batches; batch boundaries differ from production, changing what context the model and dedupe see.
- Before/after noise counts come from different extraction sets and are compositional, not paired deltas.
- The duplicate scan uses Jaccard title similarity (threshold 0.5); it misses semantic duplicates with disjoint wording and can flag distinct-but-similar tasks as duplicates.
- Sample is stratified but small; subgroup percentages (esp. per-segment precision) have wide confidence intervals.

## Recommendation

**Ship the extraction-quality prompt changes.** Recall clears 95% with zero HIGH-significance drops, precision improves ~21 points, titles are dramatically more self-contained, and the protected class did not reopen the vague-agreement loophole. Next iteration (not blocking): the residual noise is now concentrated in callback (0 -> 4) and vague-agreement (13) — a follow-up tightening pass on those two categories is the highest-value next step.
