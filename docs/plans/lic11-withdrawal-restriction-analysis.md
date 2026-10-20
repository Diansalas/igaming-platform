# lic11 STEP 1: what happens to a withdrawal today, per restriction cause (from CODE, not docs)

- **Author:** `ledger-finance` specialist (lic11). **Branch:** `lic11-withdrawal-restriction-policy` from
  `claude/licensing-jurisdiction-completion` at `ea3c379` (migration head 0179). Written BEFORE any code change.
- **Method:** code and SQL reading of the tree at `ea3c379`. Line numbers are of that tree. No database was run for this
  analysis (the private harness was refused to this agent; see the record).
- **Not a legal opinion.** Nothing here says what a regulator requires.

## 0. The premise, checked

The statement under review (Q12 in `docs/plans/licensing-owner-decision-requests.md`, recommended "(a) yes always (as built)")
is: *while a licence or a tenant/brand is suspended or expired, withdrawals and payouts of existing balances continue*.

The code shows the statement is **true for the licence and false for the tenant/brand**, and the two are governed by
different things:

1. **Licence suspended / expired / not yet issued / missing: withdrawals and payouts continue, by ABSENCE of any check.**
   No withdrawal path reads a licence. `jurisdiction.EvaluateLicenceValidity` (`internal/jurisdiction/licence_validity.go:68`)
   is consumed only by `internal/operatingmarket` (market gate: registration, deposit, launch, bets). Licence lifecycle
   writes (`internal/jurisdiction/licence_lifecycle.go`) and the expiry sweep touch only `licences` / `licence_versions` /
   alerts; nothing derives `tenants.status` or `brands.status` from a licence. So "withdrawals continue under licence
   suspension" is a **hard-coded universal rule by omission**, with no recorded policy decision, no per-tenant
   configuration and no audit trail saying that a payout went out while the licence was invalid. This is exactly the
   owner's concern, and it is the part this slice makes policy-driven.
2. **Tenant / brand suspended or closed: new withdrawal requests and normal payout submission STOP**, by an **approved
   owner decision**: ADR 0095 section 44 decisions 13-18 (HSEC-APPROVED-HOLD-RELEASE-1 / HD-CTF-6, owner instruction
   `CONTINUOUS-DEVELOPMENT-EXECUTION-CYCLE-2026-10-08`): "13. A suspended/closed tenant or brand MUST NOT automatically
   release player funds. 14. The existing hold remains in place. 15. A controlled staff resolution path is allowed.
   16. ... four-eyes ... 18. ... inactive tenant/brand means no normal payout submission." It is enforced in Go AND in the
   database (section 2). Q12's "(a) yes always (as built)" is therefore **inaccurate for tenant/brand**; ledger-finance
   review condition C2 (`docs/plans/licensing-ledger-finance-review-record.md`, F-2) already said so.

Consequence for the design (adapted, as the brief allows): the tenant/brand behaviour is NOT re-opened by an engineering
configuration switch. Making it configurable to "allow" would let two platform admins override an approved owner decision
(decision 18); that is a policy change for the owner, recorded as a proposed decision (record, section 7, D-2), not code.
The mechanism built in STEP 2 distinguishes the causes, records which one governed each decision and exposes the
tenant/brand causes as "owner-decided, not configurable" so that an amendment, if approved, has a named place to land.

## 1. Stages

| Stage | Code |
|---|---|
| R request (hold placed, `requested`) | `withdrawal.RequestWithdrawal` `internal/withdrawal/withdrawal.go:448-697` |
| A approval (`requested`/`pending_review` -> `approved`) | `withdrawal.Approve` `:837`, `Reject` `:1031`, `MoveToPendingReview` `:716` |
| H hold (funds in `player_withdrawal_hold`) | posted at R (`:609-649`); approved-hold freeze `migrations/0124...up.sql:809-835` (HR050); governed four-eyes release `internal/payments/withdrawal_hold_resolution.go` + `withdrawal.ReleaseForGovernedResolution` `:1875` |
| D dispatch T1p (`approved` -> `submitted`, attempt born `submitting`) | `payments.(*Orchestrator).ClaimForDispatch` `internal/payments/payout.go:352-723` |
| D' re-dispatch T2 re-claim / T12 resend (new `Withdraw` call) | `internal/payments/payout_sweep.go:267-` (`reclaimPayoutCreated`), `:359-` (`resubmitPayoutAmbiguous`) |
| S settlement / reversal | `withdrawal.Complete` `:1602`, `Fail` `:1692`, `Cancel` `:1789`, poll / evidence `payments/payout.go:838-1760`, payout reversal `internal/withdrawal/payout_reversal.go` |

Database backstops on the money path: `0131` `withdrawal_requests_subject_active_guard` (INSERT, WD010 tenant / WD011
brand), `0124` `withdrawal_requests_approved_hold_freeze` (any move out of `approved` on a non-active tenant/brand, HR050,
except the executing governed release), `0169` dispatch guards (WD052 account/wallet status re-read in SQL, WD053 an
allowing decision row of the same transaction), `0171` signed ALLOW (ADR 0110 proof on `withdrawal_gate_decisions`).

## 2. Matrix: cause x stage -> behaviour today

Legend: **refused** = nothing written (or only a denial audit), key not consumed; **park** = request stays `approved`, hold
kept, no attempt, no provider call; **escalate** = attempt T16 (sticky, needs a human, no resend, no release);
**defer** = attempt rescheduled on the ordinary backoff, resumes automatically; **n/g** = not gated.

### (a) Tenant status suspended / closed (also pending_launch)

| Stage | Behaviour | Where |
|---|---|---|
| R | refused (`tenant.ErrNotActiveForPaymentInitiation`, HTTP `TENANT_OR_BRAND_NOT_ACTIVE`); replay of an existing key still answered | Go `withdrawal.go:485` -> `tenant/payment_initiation_gate.go:38-50` (status key SHARED, fresh read); DB `0131` WD010 |
| A | n/g (approval proceeds) | `withdrawal.go:837-` reads no status |
| H | hold stays; frozen (no reject/cancel/fail/submit out of `approved`) except the four-eyes governed release back to `player_cash` | `0124:809-835` HR050; `payments/withdrawal_hold_resolution.go` |
| D | refused, request stays `approved` (pre-L1 status read skips the WD-RG-1 gate; post-L1 H-SEC-11 refuses) | `payout.go:440-457`, `:467-469`; DB HR050 on `approved -> submitted` |
| D' | defer (resolution-only), no escalation | `payout_sweep.go:293`, `:410` -> `sweeper_resolution_only.go:248-258` |
| S | n/g: poll, evidence, completion, decline, reversal continue | `payout_withdrawal_gate.go:14-16` (by design) |

### (b) Brand status suspended / closed (also pending_launch)

| Stage | Behaviour | Where |
|---|---|---|
| R | refused (`ErrBrandNotActive` + `ErrNotActiveForPaymentInitiation`) | `payment_initiation_gate.go:66-85` (brand row FOR SHARE); DB `0131` WD011 |
| A | n/g | |
| H | frozen as (a) | `0124` HR050 reads the brand too |
| D | refused, stays `approved` | as (a) |
| D' | **n/g** for payouts (only the TENANT is read by `checkPayoutResolutionOnly`; the brand deferral of H(8) is for deposit cascade children) | `sweeper_resolution_only.go:248-258` |
| S | n/g | |

### (c) Licence suspended / expired / not yet issued / missing (operating basis)

| Stage | Behaviour | Where |
|---|---|---|
| R, A, H, D, D', S | **n/g at every stage**; no licence read on any withdrawal / payout path; no audit records the licence state | `EvaluateLicenceValidity` only in `internal/operatingmarket` (`resolve.go:261`); `kyc/enforcement.go:380-381` (withdrawal operations do not call the licence chain); `payoutinstrument.maxAgeFor` does not judge status |

The market gate (`internal/marketgate`) refuses new deposits, launches, bets and registration on an invalid licence; it is
not called by any withdrawal path.

### (d) Jurisdiction / country closed for the tenant

| Stage | Behaviour | Where |
|---|---|---|
| all | **n/g**; there is no per-withdrawal market determination (no player country on the withdrawal path; the player-scoped resolver returns `unresolved`, Q18 / L-4) | `withdrawal/policy.go:63-74` (no per-withdrawal jurisdiction); Q19 (L-11) open |

### (e) Player-specific

| Cause | R | A | D | D' | S | Where |
|---|---|---|---|---|---|---|
| account not `active` (suspended, closed, pending_verification, identity_review_required, self_excluded status) | refused WD050 + denial audit | n/g | park WD050 (decision row + audit + alert) and DB WD052 | escalate WD050 + DB WD052 | n/g | `rggate.go:85-114`, `withdrawalgate/standard/standard.go:36-46`, `rg/rg.go:636-643`, `0169:165-184` |
| wallet not `active` | refused WD050 | n/g | park + WD052 | escalate + WD052 | n/g | same |
| self-exclusion / time-out (cooling-off) / exhausted limits / rg_config_missing | **allowed** (ADR 0115 owner decision; restriction recorded in the request audit) | n/g | allowed | allowed | n/g | `standard.go:5-8`, `withdrawal.go:658-661` |
| KYC not passed | refused (`KYCDeniedError`, decision row committed, no hold) | n/g | **DenyForCompliance**: `approved -> rejected`, hold released to `player_cash` (compensating entry) | escalate (no resend, no release) | n/g | `withdrawal.go:538-559`; `payout.go:505-555`; `withdrawal.go:1219-1320`; `payout_sweep.go:108-163` |

### (f) AML / sanctions / fraud hold (`aml.PlaceHold` / `LiftHold`, `internal/aml/holds.go:54,76`)

The hold is composed into the KYC enforcement decision (`kyc/enforcement.go:273-276`, `composeHold :309`): an active hold
denies the operation exactly like KYC `pending` (public outcome identical, internal verdict in the AML-scoped log).

| Stage | Behaviour |
|---|---|
| R | refused (as KYC denial; no hold placed) |
| A | n/g |
| D | DenyForCompliance (`approved -> rejected`, the withdrawal hold is released back to `player_cash`, where the AML hold keeps gating the next operation); an evaluator FAILURE is `unavailable` (retryable, stays `approved`, `payout.go:144-147`) |
| D' | escalate (unavailable: reschedule) |
| S | n/g |

### (g) Payment provider / instrument

| Cause | R | D | D' | Where |
|---|---|---|---|---|
| kill switch engaged | n/g | whole claim rolls back, stays `approved`, detached "hold" audit | defer (reschedule) | `payout.go:644-652`, `:693-695`; `payout_sweep.go:170-185`; `killswitch.go:334` |
| provider / capability disabled or no route | n/g | rolls back, stays `approved` | (attempt already routed) | `payout.go:557-572` |
| instrument invalid / revoked / expired / integrity | refused (nothing written) | denial audit only, stays `approved` (integrity: P1 alert) | escalate | `withdrawal.go:507-522`; `payout.go:580-607`; `payout_destination.go:258` |

### (h) Risk engine refusal (DENY / REVIEW)

| R | D | D' |
|---|---|---|
| refused WD051 + denial audit | park WD051 | escalate WD051 |

Settlement (S) is never gated for any cause: once money may have left, the platform records the provider's truth
(`payout_withdrawal_gate.go:14-16`, `0169:47-50`).

## 3. Classification

**HARD RESTRICTIONS (never overridable by a configured policy):** AML / sanctions / fraud hold (f); account or wallet not
active, including closed account (e); KYC not passed (e); kill switch, provider disabled, invalid instrument (g); risk
DENY / REVIEW (h). Also the database backstops (WD010/WD011, HR050, WD052, WD053, signed allow).

**OWNER-DECIDED, NOT ENGINEERING-CONFIGURABLE:** tenant / brand suspended or closed (a, b): ADR 0095 s44 decisions 13-18.
Changing it needs an owner amendment (proposed text in the record).

**POLICY-DEPENDENT, NO APPROVED POLICY (the gap):** licence suspended / expired / not yet issued / missing (c). Today: allow
at every stage, hard-coded by omission. Jurisdiction / country closed (d): policy-dependent (Q19) but NOT EVALUABLE on a
withdrawal today (no per-withdrawal market determination), so it cannot be made configurable without inventing a
determination; recorded as NOT IMPLEMENTED.

**Self-exclusion / time-out allowed** is an approved owner decision (ADR 0115); unchanged.

## 4. What is hard-coded today that should be policy

1. "Licence invalid -> withdrawals and payouts continue": an implicit allow with no per-tenant control, no record of
   which licence state a payout went out under, no operator visibility that the behaviour is an unapproved interim.
2. Q12 / C2 conflate licence and tenant/brand suspension; the code does not.
3. Nothing records, on a withdrawal or a payout gate decision, which restriction cause was in force and under which policy
   version it was decided.

## 5. Constraints carried into STEP 2

- Default (no policy row) MUST equal today exactly: licence causes allow at every stage; tenant/brand unchanged; no new
  refusal, no new failure mode on the default path except a fail-closed licence READ ERROR (a transport error, which today
  would abort the same transaction anyway).
- Single licence-validity implementation (INV-M-5): reuse `jurisdiction.EvaluateLicenceValidity`; no SQL copy, so the
  licence policy has no SQL backstop (same class as the risk half of WD-RG-1, residual).
- F4 semantics kept: a policy refusal at D / D' is a denying `withdrawal_gate_decisions` row through the existing pinned
  call sites (park at T1p, escalate at T2/T12); WD053 and the signed allow unchanged.
- Lock order (ADR 0082): the evaluation is plain reads, placed after the H-SEC-11 status gate (status key SHARED) and
  before the WD-RG-1 gate (L0.4); a policy change takes the tenant status key EXCLUSIVE first, so a decision and a policy
  change serialise like a decision and a tenant status change.
- Governance reuse: the 0177 licensing maker-checker semantics (widening = 2 approvals by distinct Persons other than the
  requester, 24 h freshness, 72 h expiry, narrowing = single actor with reason code executed in the request's own
  transaction, ADR 0110 proofs). Its registry/classifier (`licensing_change_kinds`, `licensing_change_classify`) may not
  be touched in this slice (parallel 0180 / 0181), so the slice reuses its session-actor function and proof verifier and
  mirrors its state machine on dedicated tables (justified in the record).
