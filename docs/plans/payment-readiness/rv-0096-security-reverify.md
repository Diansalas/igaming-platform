# RV-0096 — Security re-verification of ADR 0096 (revised)

Reviewer: `security`, 2026-09-27. Reviewed at `HEAD 6c8731e`, where the revised ADR landed in
`d50327b`. Target: `docs/decisions/0096-kyc-enforcement-boundary.md`.

**Method:** I checked every §13 condition (C1–C10) against the ADR's **design text**: §2.2, §2.6,
§3.2, §3.5, §3.6, §5, §6, §7 and §8. I did not rely on the §14 revision record's own claims. I also
checked the text against the live schema and code it depends on:
- `migrations/0040_*` (`kyc_verifications`: `person_id`, `expires_at`, and the tenant RLS),
- `migrations/0075_*` lines 159–225 (the RLS/trigger precedent),
- `internal/db/tenant_rls.go` (how `WithTenant` commits and rolls back),
- `internal/kyc` (who writes `expires_at`).

**Out of scope:** no code exists yet, and no pen test or legal review was done. This does not
approve the PRH-I3 implementation. That still needs its own security review (ADR §8 items 3, 6, 10).

**Migration renumber 0101 → 0100:** this changes the allocation only, not the design. Nothing in this
verdict depends on the number. The ADR body still says `0101` in about 10 places (§0, §3.2, §3.6
heading and filename, §7, §8). All of them must be renumbered together, including the `.down.sql`
name. No `0100_*`/`0101_*` file exists in `migrations/` yet.

## Verdict

**APPROVE WITH CONDITIONS stands. The launch-blocking conditions C1, C2, C4(a–d, f) and C5 are
CLOSED at design level.** Two conditions remain OPEN:
- C4(e) is OPEN (N2 below).
- C3 is OPEN as an implementation gate. The mechanism deferral is accepted, with conditions (see the
  ruling below).

There are 3 new MEDIUM findings: N1, N2 and N3. They are design-text defects and must be fixed in the
ADR **before PRH-I3 implementation starts**. There are 4 new LOW findings.

## Per-condition status

| # | Sev | Status | Verified against | Notes |
|---|---|---|---|---|
| C1 | HIGH, launch-blocking | **CLOSED** (design) | §3.2 pt 1, §1 row #3, §5 performance bullet, §7.2 rows 3–5 | Every withdrawal request and payout dispatch requires `passed`. There is no history exemption. The `withdrawal_requests` history query is removed, `first_withdrawal` is not a `trigger_type`, and §7 asserts the opposite of the old exemption test. A new wallet or account with no verification row evaluates `failed`, so it cannot reset the gate. **But** the read key for "per Person" is inconsistent (N1). |
| C2 | HIGH, launch-blocking | **CLOSED** (design); DB tests are an impl gate | §3.6 SQL, §7.3 | The INSERT/UPDATE predicates match 0075 lines 207–225 verbatim. `FORCE` RLS is set on both tables. There is no DELETE or FOR ALL policy. The lifecycle trigger allows exactly `draft→active`, `draft→withdrawn` and `active→withdrawn`, and every non-`status` column is immutable. Both tables have TRUNCATE guards. `created_by_actor_id NOT NULL` is bound to the principal GUC in `WITH CHECK`. The decisions table has `FORCE`, a `NULLIF` tenant predicate and an append-only + TRUNCATE trigger. Residual LOW items are in N7. INSERT directly as `active` is possible; that bears on C3 (see the ruling). |
| C3 | MEDIUM | **OPEN** (impl gate) | §8 item 10, §14.5 | Audit of each write is specified. Four-eyes is required but its mechanism is not designed. See the ruling below. |
| C4(a) | HIGH, launch-blocking | **CLOSED** | §2.6(a), §5, §7.1 | Uses the latest row with `created_at DESC, id DESC`, and names `EXISTS(approved)` as the bug. The row key is subject to N1. |
| C4(b) | HIGH, launch-blocking | **CLOSED**, with a note | §2.6(b), §7.1 | `expires_at <= now()` evaluates `failed`. The column already exists (0040:61), but no code writes it (N6). |
| C4(c) | HIGH, launch-blocking | **CLOSED** | §2.2 `OutcomeUnavailable`, §2.6(c), §7.1, §7.5 | A query error and "no rows" are asserted as distinct code paths. |
| C4(d) | HIGH, launch-blocking | **CLOSED** | §2.2, §2.6(d), §7.1 | `JurisdictionCode` is removed from `EnforcementParams`. Policies are selected only by `LicensingJurisdictionID`. |
| C4(e) | HIGH (activates with HD-KYC-1) | **OPEN** | §2.6(e), §3.6 unique index | The §14.1 claim "across all assets/wallets" is **not** what the text says. The text is same-asset-only, and the unique index allows only one active `cumulative_deposit` row per jurisdiction, whatever the asset. See N2. |
| C4(f) | HIGH, launch-blocking | **CLOSED** | §2.6(f), §7.5 | The test uses a valid tenant-B token carrying tenant A's `player_account_id`. |
| C5 | HIGH, launch-blocking | **CLOSED** (design), with binding impl notes | §3.6 "Commit discipline", §5, §8 items 2–3, §7.2, §7.6 | Every enforcement point evaluates before its first state-changing statement, and denies commit decision + audit. Two implementation hazards and one text inconsistency remain: N4 and N5. |
| C6 | MEDIUM | **CLOSED** (design); raw-guard is an impl gate | §8 item 3, §7.5, §5 (ADR 0095 Phase A / sweeper) | |
| C7 | MEDIUM | **CLOSED** (design) | §6, §7.5 | Covers tenant from context, cross-tenant 404, roles, the audited cross-tenant path, keyset pagination with max and default page size, and exact response fields. |
| C8 | MEDIUM | **CLOSED** (design) | §6, §3.5 last bullet, §7.5 | Closed-enum codes; `unavailable` is generic; no policy row is exposed to players. |
| C9 | LOW | **CLOSED** (design) | §6 dormancy route | Must be extended to per-asset under N2, and must not report inert triggers as "configured" (N3). |
| C10 | LOW | **CLOSED** (design); checked again at impl | §3.7 | |
| Ruling (deposit allow-when-unconfigured) | — | **Stands** | §3.2 pt 2 | Its precondition, C1, is now met. Ruling (b)'s launch sign-off (HD-KYC-1 + legal) is still a human launch item. |

## Ruling on the C3 deferral (§8 item 10 / §14.5)

**Accepted with conditions. C3 stays OPEN and blocks PRH-I3 from being marked complete.** Leaving the
mechanism's shape to implementation is acceptable because the requirement itself is binding in §8
item 10, and nothing is silently dropped. The conditions:

1. **No "first cut without four-eyes"** unless a new ADR decision records it and **all relax
   operations are unavailable until four-eyes exists**. In that interim, no API or repository path may
   withdraw an `active` row or supersede one. Failing closed means the operator cannot relax a policy
   at all.
2. **Do not classify "relaxing" changes.** Require four-eyes on **every** activation (UPDATE
   `draft→active` *and* INSERT directly as `active`) and on every `active→withdrawn`. Deciding whether
   a change is "higher threshold, therefore relaxing" is itself an attack surface: a row can also be
   relaxed by changing its asset (N2), its `play_operation`, or its `required_tier`. The schema
   currently allows a single principal to INSERT a row that is already `active`. That path must go
   through the same mechanism, or be rejected by the DB (e.g. INSERT `WITH CHECK (status = 'draft')`).
3. **Approver ≠ requester must be enforced by the database** (a CHECK or trigger on the approval
   record, keyed by distinct principal ids), not only in the handler. Today the RLS lets any single
   platform-admin connection UPDATE `active→withdrawn`, so an API-only control can be bypassed by any
   platform-admin-scoped connection.
4. **Supersession is atomic.** The unique active index forces "withdraw the old row, activate the
   new one". Both must happen in one transaction; otherwise the jurisdiction is dormant
   (`not_required`) in between.
5. **Launch linkage:** if any `kyc_enforcement_policies` row is `active` at launch, the four-eyes
   mechanism must be IMPLEMENTED and security-reviewed first. If no row is active, only ruling (b)
   applies.

## New findings

**N1 — MEDIUM — The read key contradicts itself ("per Person" vs. per account vs. HD-KYC-6).**
- §3.2 pt 1 and §7.2 say the withdrawal rule is scoped per `Person`, and §5 says the latest row is
  read "scoped by `PersonID`".
- §2.6(a) and §5 (same bullet) say the latest row is read by `(tenant_id, brand_id,
  player_account_id)`.
- HD-KYC-6 says the read stays tenant/brand-narrow.
- Migration 0040's header says `person_id` is "never … an enforcement or RLS key today".

*Failure scenario:* an implementer writes `WHERE person_id = $1 ORDER BY created_at DESC LIMIT 1`.
RLS keeps this inside the tenant, but now:
- (i) an approval at brand A satisfies a withdrawal at brand B. That silently decides HD-KYC-6.
- (ii) a newer `approved` row on account 2 masks a `rejected` latest row on account 1, so account 1
  withdraws against a known negative state. That is the exact class C1 closed.

*Fix (ADR text):* the passed-check keys on the gated account's own latest row,
`(tenant_id, brand_id, player_account_id)`. With no row the result is `failed`, which is what stops a
new account from resetting the gate. `PersonID` is used, if at all, only as an **additional
deny-only** check: `failed` if any other `PlayerAccount` of the same Person *in the same tenant* has
a latest row that is `rejected`. Rewrite §3.2 pt 1, §5 and the §7.2 "per Person" row to match. Or
drop `PersonID` from `EnforcementParams`.

**N2 — MEDIUM (activates with HD-KYC-1; reopens C4(e)) — `cumulative_deposit` covers one asset per
jurisdiction.** `kyc_enforcement_policies_one_active` is unique on `(licensing_jurisdiction_id,
trigger_type, COALESCE(play_operation,''))`. It does not include `asset_code`. §2.6(e) sums only the
row's own asset.

*Failure scenario:* a jurisdiction activates a EUR threshold. A player then deposits without limit
in USD or USDT: no row can exist for those assets, so those deposits evaluate `not_required`. This is
structuring by asset choice, which is what C4(e) was meant to prevent.

*Fix:*
- Add `asset_code` to the unique index.
- Report dormancy per (jurisdiction, trigger, asset enabled for a live tenant) (C9).
- Require HD-KYC-1 to decide explicitly between per-asset thresholds and cross-asset aggregation
  (an FX/`ConversionOperation` basis, per ledger-finance C6). Record cross-asset structuring as a
  named risk in that decision.

A related point to record in HD-KYC-1: the cumulative sum is per `player_account_id`, so a Person
with several accounts in the same tenant can split deposits across them.

**N3 — MEDIUM — Triggers that can be activated but are never evaluated.**
- §5 says `withdrawal_hold`/`withdrawal_payout` do no policy lookup, so an active `edd_amount` row is
  never read on withdrawal. §3.2 and §7.2 nonetheless say it "still applies independently,
  additively".
- The outcome model has no tier: `passed` is `passed`, so EDD cannot be expressed.
- `registration_tier` has no `EnforcementOperation` at all.

*Failure scenario:* legal review sets an EDD threshold, and an operator activates it with a
`legal_review_reference`. The dormancy report shows the jurisdiction as configured, but nothing is
enforced. This is false compliance assurance.

*Fix:* either specify where and how each of these triggers is evaluated, or narrow the
`trigger_type` CHECK to the triggers that are actually evaluated (`cumulative_deposit`, `play`) until
that is designed. Also correct the §3.2/§7.2 "additively" wording.

**N4 — LOW (binding implementation note for C5) — Any `Err*` returned from the tx closure rolls back
the denial record.** `internal/db/tenant_rls.go:57-59` rolls back whenever `fn` returns a non-nil
error. §8 item 3 names the deny results `ErrKYCRequired` and `(wr, ErrKYCDeniedCommitted)` and says
"the handler commits". That only holds if the closure returns **nil** and the deny is carried out of
the closure by a captured variable. The §7.2/§7.6 "committed, not rolled back" tests are what prove
this. They must run through the real `WithTenant` wrapper, not a test tx the test commits by hand.

**N5 — LOW — The payout-deny commit shape contradicts §3.6's rule, and the denial record is lost if
the release fails.**
- §3.6 says a deny transaction has "no domain effect", and C5's test said "zero ledger or hold
  effect".
- For `withdrawal_payout`, §8 item 3 / §7.2 instead commit decision + audit **with** the
  `DenyForCompliance` hold release and the `approved→rejected` transition. That is correct and
  accepted: releasing the hold is the effect the denial is meant to have.
- *Fix §3.6 and §7.6* to carve out this case explicitly.
- Also, if `DenyForCompliance` fails (`ErrStateConflict` after losing the race with `Reject`/`Cancel`,
  or any posting error), the whole transaction rolls back, taking the decision and audit rows with
  it. The system still fails closed, because `MarkSubmitted` is never reached. But the KYC denial
  goes unrecorded. Require the handler to commit the decision and audit rows in a fresh
  transaction on that path, and test it.

**N6 — LOW — `kyc_verifications.expires_at` exists (0040:61) but nothing in `internal/kyc` writes it.**
Every `approved` row today therefore has `NULL` expiry and never expires unless a provider sends
`expired`. §2.6(b) is correct but has no effect in practice. *Fix:* state explicitly what `NULL`
means (valid until a provider-reported change). Either populate `expires_at` on approval in PRH-I3,
or record that platform-side re-verification periods are an HD tied to HDR-J-6. Do not leave it
implicit.

**N7 — LOW — Text and schema tidy-ups (none are exploitable as written):**
- (a) §3.6's intro still says writes come from a "platform_admin/**compliance** principal". The RLS
  is authoritative and correct; remove "compliance".
- (b) §3.2 pt 1 cites "§3.6 point (b)"; it should be §2.6(b).
- (c) The policy lookup filters only `status='active'` and ignores `effective_from`. Either filter on
  `effective_from <= now()` or document that the column is informational. In either case, the
  "BEFORE INSERT forge-proofing trigger, omitted for brevity" must actually be written in the
  migration.
- (d) `created_by_actor_type` has no CHECK; constrain it to the platform-admin actor type.
- (e) Apply the 0101→0100 renumber consistently (see above).

## What this does not cover

This review covers the design text only. It covers none of the following:
- the PRH-I3 code, migration SQL as actually written, or the raw-guard test,
- the four-eyes mechanism (C3),
- ADR 0095's own text, including whether it has taken on the §8 item 9 constraint,
- any legal adequacy of dormancy or threshold posture (ruling (b), HD-KYC-1..8).

Launch still requires human sign-off on ruling (b) and closure of C3, N2 and N3 before any threshold
policy is activated.
