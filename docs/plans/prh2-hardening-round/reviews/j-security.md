# Security review — PRH-2 J (PRH-I4-METRICS-1), 2026-09-28

**Reviewer:** `security`. The orchestrator recorded this review.

**Scope:** `6d8beea` against main `9c6c705`: code reading of all 6 files, plus unit tests.

## Verdict: ACCEPT (no conditions)

1. **Labels are bounded and not attacker-controlled: CONFIRMED.**
   - Every value is a compile-time constant: `decision` has 2 values, `reason` 9 and `provider_kind` 4 (domain enum). That is at most 72 counter series and 4 gauge series.
   - No tenant key, provider key, IP or id reaches a metric.
   - Exporters are stdout or none only; there is no `/metrics` endpoint.
2. **Metrics cannot change a decision: CONFIRMED.**
   - Recording happens after the decision. Every Add goes through `safelyRecord` (a recover); the mutant removing it is KILLED.
   - The release wrapper is panic-safe and invoked exactly once.
3. **No new side channel: CONFIRMED.** Responses, headers and ordering are unchanged. **Future constraint:** any scrape endpoint must be internal-only or authenticated.
4. **Narrowing to PRH-I4-METRICS-2: ACCEPTABLE.** It is stricter than the §8 draft. **Condition for METRICS-2:** its gauges stay tier- and domain-labelled only, and need their own security review.

## Low

- **J-L1:** ADR §21.12 says "closed" while the reviews are pending. It is accurate once both reviews are recorded.
- **J-L2:** a request that passes both stages counts "admitted" twice (preauth and verified). Add a closed `stage` label, or document the double count.
- **J-L3 (optional):** a static test keeping the label types closed by construction.
