# HUMAN DECISION PACK — Closed-tenant funds (HD-CTF-1..9) and E1 KYC (HQ-E1-1..4)

Prepared by: architect (read-only analysis, 2026-10-05, HEAD `95b17c5`). No repo edits, no tests run.
Sources: ADR 0107 §12 (design only), ADR 0106 §12 (E1 merged `d34f088`, MOCK KYC), ADR 0105 §1 (HD-PRH2-9, decided),
ADR 0102 §3.1 (severity semantics), registry `docs/governance/task-registry.md:4146-4157`, `docs/HANDOVER.md:91-101`.

Labels: **REQ** = Blueprint / recorded human decision / binding review ruling. **RECOMMENDATION** = architect or
reviewer proposal, reversible, not a legal or compliance threshold. Nothing below is a legal answer.

## 0. Status: what is still open

HD-PRH2-9 (ADR 0105 §1) is decided and is not reopened here. It fixes the frame:
- staff resolution path only;
- no auto-dispatch and no auto-cancel or auto-release;
- four-eyes per the applicable policy;
- configurable and jurisdiction-aware;
- no hard-coded legal outcome;
- any legally required outcome is a separate human decision.

It answers **none** of HD-CTF-1..9 completely. It explicitly hands the content of "the permitted resolution" to the
applicable operator or jurisdiction, and that is HD-CTF-1. Partial coverage by HD-PRH2-9 or other decisions:

| ID | Still unresolved? | Already settled by (cite) |
|---|---|---|
| HD-CTF-1 | **YES** (both (a) and (b)) | Mechanism only (HD-PRH2-9). Thresholds and approval counts are config under HD-PRH2-3, with none seeded (0107 §12 footnote) |
| HD-CTF-2 | **Partly.** The "may tenant staff act" half has a binding default; grant-chain separation is open | Security Q-CT-SEC-1 CONFIRMED platform_acting only, no T-family read (`reviews/k3-design-security.md:39`); K3 R-5 already refuses tenant-scope actors on closed tenants in 0115 (`migrations/0115_payment_force_resolution.up.sql:338,350,520,842`) |
| HD-CTF-3 | **YES** | — |
| HD-CTF-4 | **YES** | HD-PRH2-9 covers payouts and holds only. Applying "no automatic action" to non-withdrawal balances is a reasonable reading but an extrapolation |
| HD-CTF-5 | **YES** (legal) | — |
| HD-CTF-6 | **YES** (interim engineering default available) | — |
| HD-CTF-7 | **Partly.** The labels are proposed; the evidence obligations are open | Reason codes are "labels, not powers" (0107 §5.4) |
| HD-CTF-8 | **YES** for players, regulators and deadlines | ALERT-DELIVERY-1 / HD-PRH2-4-OPS remain the routing items |
| HD-CTF-9 | **YES** | — |
| HQ-E1-1 | **Open, with an implemented fail-closed default** | Security and IC support the default (`reviews/e1-design-identity-compliance.md:15`, `reviews/e1-design-security.md:24`) |
| HQ-E1-2 | **Confirmation only** (see §2) | ADR 0102 §3.1 severity taxonomy; security says "p2 acceptable for outage exhaustion" |
| HQ-E1-3 | **YES** (legal, per jurisdiction) | — |
| HQ-E1-4 | **YES** (legal and data protection) | — |

Overall code state for HD-CTF: **NOT IMPLEMENTED**. `git grep closed_tenant` finds no Go or SQL. No code path writes
`tenants.status`: the only `UPDATE tenants` sets `licence_id` (`internal/jurisdiction/tenant_licence_admin.go:129`), and
the status CHECK comes from `migrations/0001_create_tenants.up.sql:14`. **No tenant-closure flow exists today.** That
is why every HD-CTF item is a launch blocker for a closure flow only and not for operating active tenants (0107 §14).

---

## 1. Closed-tenant funds: HD-CTF-1..9 (ADR 0107 §12)

Shared dependencies for every HD-CTF item:
- No code exists for the ADR 0107 mechanism.
- Today the H resolution-only gate withholds T2 re-claim for non-active tenants
  (`internal/payments/sweeper_resolution_only.go:75`, `payout_sweep.go:253`). The current state is therefore
  "held funds stay held".
- The fail-closed default holds until each item is answered: **every outcome is disabled**. In detail:
  - no rule row means false (0107 §4.2 step 3);
  - with no `financial_policy_required_approvals('closed_tenant_hold_resolution', …)` platform row, the operation is
    disabled (0107 §5.2);
  - with no `closed_tenant_resolution_settings.dispatch_permit_max_lifetime` row, no permit can be created (0107 §6.2).

### HD-CTF-1: permitted outcomes and precedence
- **Question:**
  - (a) Which §4.1 outcomes are permitted, per jurisdiction (Anjouan first) and per own-licence operator, for each
    item class? This is the content of the first `closed_tenant_resolution_rules` rows.
  - (b) Is the platform row a floor that a jurisdiction cannot loosen, or does the most specific level win?
- **Dependency:**
  - Reads the answer: `closed_tenant_resolution_rules` (append-only, levels platform/jurisdiction/licence) through
    `closed_tenant_outcome_permitted()` (0107 §4.2).
  - Precedence is configured by (b) before any rule row is accepted. Until then, rule changes are refused.
  - Outcomes are catalogued in `closed_tenant_resolution_outcomes` (`release_hold_to_player_cash`,
    `dispatch_via_governed_permit`, `retain_pending_determination`; all "example only").
- **Options for (a):** content is legal, so the architect gives no recommendation.
  - A1: Anjouan permits `retain_pending_determination` only, for every class. Nothing moves, and decisions are
    recorded and audited.
  - A2: A1 plus `release_hold_to_player_cash` (CT-PRE, CT-NEVER-SENT).
  - A3: A2 plus `dispatch_via_governed_permit` (CT-NEVER-SENT).
- **Options for (b):**
  - B1, **RECOMMENDATION:** tighten-only. Every present level must permit, so a lower level can forbid but cannot
    loosen. This follows the existing floor precedent (ADR 0034 §14.2, `migrations/0043_open_bet_self_exclusion_policy.up.sql:27-29`).
  - B2: most specific wins (licence > jurisdiction > platform). This suits own-licence operators (ADR 0006).
  - B3: B1, except that a `licence`-level row may loosen for own-licence operators only.
- **Consequences:**
  - A1: money-neutral and lowest risk. It leaves R-CT-1 open (funds stay stranded).
  - A2: moves hold to `player_cash`. It is useless without HD-CTF-3, because the closed-tenant submit gate (0107 §6.4)
    blocks withdrawal of that cash.
  - A3: real outbound money for a closed tenant. It has the highest insider risk (TM-CT-1) and depends on H-SEC-5 for
    deposits (R-CT-6).
  - B1: safest. The platform becomes a gatekeeper even for own-licence operators, which is a commercial and legal
    posture.
  - B2: delegates legal responsibility to the most specific configuration, so a misconfigured licence row can loosen
    platform policy.
- **Blocks a real PSP?** No. It blocks only a tenant-closure flow (0107 §14).
- **Can engineering continue without it?** Yes for the mechanism, once a workstream is authorized. Nothing is seeded,
  everything is refused, and rule changes are refused until (b) is answered.

### HD-CTF-2: closed-tenant staff, and grant-chain separation
- **Question:** May the closed tenant's own staff see, request or approve anything (default none)? Is grant-chain
  separation (§5.2) required?
- **Dependency:**
  - §3 "no T-family read until HD-CTF-2";
  - `eligible_tenant_roles = '{}'` on the new capability pair (§5.2);
  - the optional DB check "approver ≠ Person who approved the requester's G-P2 grant".
  - Also the security r4 delta, item 2 (`reviews/k3-design-r4-security-delta.md:23`): the existing tenant-session
    `Reject`, KYC-deny and `Cancel` withdrawal edges (`withdrawal.go:950/1117/1668`) stay open to closed-tenant staff.
    Whether to DB-refuse them for `closed` tenants is decided together with HD-CTF-2.
- **Options:**
  - O1, **RECOMMENDATION:** keep "none" (already a security requirement, Q-CT-SEC-1). Also DB-refuse the tenant-session
    withdrawal edges for `closed` tenants. Grant-chain separation is required.
  - O2: none for staff actions, plus a read-only T-family projection for the closed tenant's staff.
  - O3: as O1, but without grant-chain separation.
- **Consequences:**
  - O1 raises the collusion floor from 2 to 3 platform admins (TM-7). That needs at least 3 platform admins on staff:
    an operational and staffing fact for the owner.
  - O2 adds a cross-org data-exposure surface after the commercial relationship has ended.
  - O3 keeps the floor at 2.
- **Blocks a real PSP?** No.
- **Can engineering continue without it?** Yes. The default is none and platform-only (a binding security ruling).
  Grant-chain separation can be built as a strict default and relaxed later. It is DB code, so relaxing it needs a
  migration.

### HD-CTF-3: how a player obtains released cash
- **Question:** After a release to `player_cash`, how does the player obtain the money from a closed tenant (access,
  withdrawal path, notification)?
- **Dependency:**
  - `withdrawal_requests_closed_tenant_submit_gate` refuses `→ submitted` for closed tenants (§6.4).
  - CT-PRE dispatch is not expressible (§4.1).
  - Player access to a closed tenant's brand is not defined.
  - Residual R-CT-1.
- **Options:** all are product or legal choices; none is a RECOMMENDATION.
  - P1: a new governed platform-dispatched withdrawal for released cash. This needs a new outcome, a new ADR and
    HD-CTF-5-style approval.
  - P2: funds remain in `player_cash` until an external process (operator, regulator or successor) handles them. This
    links to HD-CTF-5.
  - P3: platform-hosted, limited "closed brand" player access for withdrawal only.
- **Consequences:**
  - P1 and P3 create a new outbound money path on a closed tenant (KYC/AML gate still applies). P3 also needs identity
    and session work.
  - P2 keeps the mechanism money-neutral but shifts the problem to legal.
- **Blocks a real PSP?** No.
- **Can engineering continue without it?** Yes. The safe default is that `release_*` stays disabled (HD-CTF-1),
  because a release without HD-CTF-3 only moves stranded money from hold to cash.

### HD-CTF-4: balances not under a withdrawal
- **Question:** What happens to closed-tenant balances that are not under a withdrawal?
- **Dependency:** outside the ADR 0107 mechanism ("Not covered", §2). No code exists. It would need a new design and
  ADR.
- **Options:** legal; none is a RECOMMENDATION.
  - V1: no action. Balances stay in the ledger, the player may raise a request, and it is handled under HD-CTF-3's
    path.
  - V2: a governed bulk process (an extension of ADR 0107).
  - V3: a transfer to a third party, unclaimed-funds body or successor (this is HD-CTF-5).
- **Consequences:**
  - V1 is money-neutral, keeps funds visible, and is reconciliation-safe.
  - V2 and V3 need new posting shapes, four-eyes and audit.
- **Blocks a real PSP?** No.
- **Can engineering continue without it?** Yes. The default is no action. Balances are ledger projections and nothing
  mutates them.

### HD-CTF-5: legally required outcomes outside §4.1
- **Question:** Is an outcome outside §4.1 legally required: a third-party or unclaimed-funds transfer, escrow, or a
  successor operator?
- **Dependency:** not expressible. It would need a new `posting_shape` (closed CHECK set with a one-to-one pin, LF
  CT-4) and an ADR.
- **Options:** "no, not for Anjouan" / "yes, specify which" / "unknown, obtain a legal opinion".
- **Consequences:**
  - A "yes" creates a new ledger shape, new counterparty accounts and reconciliation scope.
  - Each new shape needs LF and security review.
- **Blocks a real PSP?** No.
- **Can engineering continue without it?** Yes. Fail-closed: such outcomes are not expressible.

### HD-CTF-6: closing a tenant while holds exist
- **Question:** May a tenant be set to `closed` while hold-bearing withdrawals exist?
- **Dependency:**
  - No code writes `tenants.status` today; there is no closure flow.
  - The rule would be a guard trigger on `tenants` or a precondition in a future closure workflow.
- **Options:**
  - C1, **RECOMMENDATION (architect, interim):** refuse the transition to `closed` while any hold-bearing withdrawal
    exists.
  - C2: allow it. Every hold enters the ADR 0107 queue.
  - C3: allow it only with a documented regulator or legal instruction (reason code plus evidence).
- **Consequences:**
  - C1 is the safest. It may be legally untenable if a regulator or licence revocation forces closure, because closure
    is then a fact, not a platform choice.
  - C2 relies entirely on HD-CTF-1, 3 and 4.
  - C3 adds governance on the closure action itself.
- **Blocks a real PSP?** No.
- **Can engineering continue without it?** Yes. C1 can be implemented as a reversible interim default when a closure
  flow is built.

### HD-CTF-7: reason codes and evidence
- **Question:** What is the reason-code vocabulary, and what are the evidence obligations (§5.4)?
- **Dependency:**
  - `closed_tenant_resolution_reason_codes` (migration-seeded);
  - `evidence_ref_hash` is required for every reason except `retained_pending_determination`.
- **Options:**
  - E1, **RECOMMENDATION:** adopt the proposed five labels. Evidence = a hash reference to an externally retained
    document.
  - E2: the same labels, plus a compliance-defined minimum evidence type and retention per reason.
- **Consequences:**
  - The labels confer no power (0107 §5.4), so the vocabulary itself is low risk.
  - Evidence retention and acceptable evidence types are compliance obligations.
- **Blocks a real PSP?** No.
- **Can engineering continue without it?** Yes. Seed the labels. Evidence semantics stay documented as open.

### HD-CTF-8: alerts, notifications and deadlines
- **Question:** Are alerts, notifications to players or regulators, or deadlines required? No timelines, recipients or
  retention are set.
- **Dependency:**
  - R-CT-5 ("nothing alerts on a closed tenant holding funds");
  - ADR 0102 alert Kinds (migration-only `alert_kinds`);
  - ALERT-DELIVERY-1 OPEN and HD-PRH2-4-OPS (no recipients).
- **Options:**
  - N1, **RECOMMENDATION (engineering-resolvable part):** add a durable platform alert Kind for "closed tenant holds
    player funds", unrouted like every other Kind.
  - N2: N1 plus player and/or regulator notifications with deadlines. Content, recipients and timelines are legal.
- **Consequences:**
  - N1 gives visibility only; nobody is paged while ALERT-DELIVERY-1 is open.
  - N2 creates regulatory reporting obligations and a messaging channel.
- **Blocks a real PSP?** No.
- **Can engineering continue without it?** Yes for N1. N2 needs owner and legal input.

### HD-CTF-9: reopening a closed tenant
- **Question:** May a `closed` tenant ever be reopened?
- **Dependency:**
  - Execution requires `closed`.
  - A reopen voids nothing automatically, but pending resolutions refuse at execution (0107 §8).
  - It also interacts with HQ-E1-1: KYC rows of closed tenants are deferred indefinitely, and they resume only on
    reactivation.
- **Options:**
  - Z1, **RECOMMENDATION (interim):** `closed` is terminal. A DB guard refuses `closed → *`, and reversing that is a
    migration.
  - Z2: reopen only through a governed, four-eyes, audited platform action. All closed-tenant resolutions and permits
    are voided on reopen.
  - Z3: reopen allowed freely.
- **Consequences:**
  - Z1 gives HQ-E1-1's indefinite deferral a definite meaning: rows never resume, which feeds into HQ-E1-4 retention.
  - Z2 adds state-transition governance.
  - Z3 lets stale permits and authorizations survive context changes. Not recommended.
- **Blocks a real PSP?** No.
- **Can engineering continue without it?** Yes. No writer of `tenants.status` exists today, so the state is
  effectively terminal already.

---

## 2. E1 KYC: HQ-E1-1..4 (ADR 0106 §12)

### HQ-E1-1: suspended and closed tenants
- **Question:** For suspended or closed tenants, should the platform keep the default (no vendor call; deferred
  indefinitely without consuming attempts; never cancelled; never a status write), auto-cancel a closed tenant's
  submissions, or ever send for a suspended tenant? This is a data-processing and contractual decision.
- **Dependency:** **IMPLEMENTED default.**
  - `internal/kyc/outbox_worker.go:444-450` reads `tenants.status` and defers with `deferred_tenant_inactive`.
  - The 0114 trigger admits that class only while the tenant is not active (F6).
  - Tests T-H (42) and mutants M13 and M34 cover it.
- **Options:**
  - K1, **RECOMMENDATION:** keep the default (security and IC concur).
  - K2: suspended keeps deferring; closed auto-cancels (new `cancel_reason`, migration).
  - K3: send for suspended tenants.
- **Consequences:**
  - K1 sends no PII to a vendor on behalf of an inactive operator, but rows grow (HQ-E1-4).
  - K2 is a clean end state. It needs HD-CTF-9 to be terminal first.
  - K3 processes PII for a non-operating controller (legal exposure).
- **Blocks a real PSP?** No.
- **Can engineering continue without it?** Yes. The default is already fail-closed.

### HQ-E1-2: alert severity of `kyc.submission_failed_terminal`
- **Question:** Confirm `p2`, or raise it to `p1`, before 0114 reaches a real tenant (a change needs a migration).
- **Dependency:**
  - seed in `migrations/0114_kyc_submission_outbox.up.sql:613-623` (`'p2'`);
  - Go mirror `internal/alerting/kind.go`;
  - parity test `migration_0114_kyc_kind_integration_test.go`.
- **Evidence check:**
  - ADR 0102 §3.1 defines p1 as integrity or money-correctness, and p2 as an operational safety event. Every existing
    p1 Kind is a money or integrity Kind (`migrations/0110_durable_alerting.up.sql:160-182`).
  - Exhausting submission retries does not change any verification status or enforcement outcome: IC F3,
    INV-KYC-OB-5, and enforcement never reads the outbox. A player stays unverified, which fails closed.
  - The security review says "p2 acceptable for outage exhaustion"; IC says "keep p2".
  - **One honest caveat:** the `:binding_mismatch` discriminator is called "an integrity signal, escalate to
    security" (ADR 0106 §8.3). By ADR 0102's taxonomy that leans p1, but severity is per Kind, not per discriminator.
  - Today p1 and p2 both page nobody (ALERT-DELIVERY-1 OPEN).
- **Verdict:**
  - **No compliance or security evidence requires changing p2.** A decision is needed only as an owner/compliance
    confirmation ("confirm p2"). Compliance's open question is whether a stranded KYC submission is a reportable
    compliance incident, and that ties to HQ-E1-3.
  - The binding-mismatch nuance is an engineering and security choice (split it into its own p1 Kind), not an owner
    question.
- **Options:**
  - S1, **RECOMMENDATION:** confirm p2.
  - S2: p1 for the whole Kind.
  - S3: p2 kept, plus a separate p1 Kind for `credential_binding_mismatch` (security and architect ADR, migration).
- **Consequences:**
  - S2 over-pages outage noise once delivery exists.
  - S3 aligns with ADR 0102 at the cost of one more Kind.
- **Blocks a real PSP?** No. The registry gates it only before 0114 reaches a real tenant.
- **Can engineering continue without it?** Yes. p2 is live on main.

### HQ-E1-3: jurisdiction KYC deadlines
- **Question:** Does a jurisdiction impose a KYC submission or completion deadline? If so, it becomes jurisdiction
  configuration, never a worker constant.
- **Dependency:**
  - The worker retry budget is a **PLACEHOLDER** technical bound: `BackoffBase` 30 s, `BackoffCap` 30 min,
    `MaxFailedAttempts` 8 (`internal/kyc/outbox_worker.go:54-86`).
  - **No deadline configuration row or key exists.** ADR 0106 §2.8 assigns any deadline to the ADR 0096 §3 jurisdiction
    configuration model (the `kyc_enforcement_policies` family, `migrations/0100_…up.sql:18`, keyed by
    `licensing_jurisdiction_id`).
  - Related: HD-KYC-1..8.
- **Options:** none is a RECOMMENDATION.
  - D1: "no deadline for Anjouan" (keep placeholders as technical only).
  - D2: "deadline X for jurisdiction Y" (needs a new configuration column or table by migration).
  - D3: unknown; legal opinion required.
- **Consequences:**
  - D2 creates enforcement and alerting obligations, so failing to meet the deadline becomes a compliance incident.
    That would re-open HQ-E1-2.
- **Blocks a real PSP?** No. It likely gates a real KYC vendor or real-money onboarding in any jurisdiction that has a
  deadline.
- **Can engineering continue without it?** Yes. Never present the retry budget as a deadline.

### HQ-E1-4: outbox retention and document erasure
- **Question:** How long are outbox rows retained (ids and states only; they are the only durable claim record)? Does
  erasure of a document oblige any action on an already-sent submission?
- **Dependency:**
  - The 0114 guard refuses DELETE and TRUNCATE.
  - The down migration refuses on any row (`0114…up.sql`/`down.sql`, ADR 0106 §5.3).
  - There is no deletion path (`NOT IMPLEMENTED`).
- **Options:**
  - T1, **RECOMMENDATION interim:** keep everything; no deletion.
  - T2: a period equal to audit retention, with claim history audited before deletion (security condition).
  - T3: erasure triggers a vendor-side deletion request (PROVIDER DEPENDENT).
- **Consequences:**
  - T1 means unbounded growth (R7). There is no PII in the outbox, so the data-protection risk is low.
  - T2 needs a governed delete path and a migration.
  - The T3 obligation for the EU/GDPR market is a legal question.
- **Blocks a real PSP?** No.
- **Can engineering continue without it?** Yes.

---

## 3. Engineering-resolvable vs needs owner, compliance or legal

**An engineer can resolve these with a safe, reversible default** (architect, security and LF concurrence; record it
in an ADR):
- HD-CTF-1(b) precedence = tighten-only (B1). This still needs owner confirmation because it touches own-licence
  operators (ADR 0006).
- HD-CTF-2: tenant staff none (already a security requirement); grant-chain separation ON; DB-refuse tenant-session
  withdrawal edges for `closed`.
- HD-CTF-6: interim refusal of closure while holds exist.
- HD-CTF-7: seed the label vocabulary.
- HD-CTF-8 N1: an unrouted durable alert Kind.
- HD-CTF-9: interim "closed is terminal" guard.
- `closed_tenant_resolution_settings.dispatch_permit_max_lifetime`: technical value, security-approved (§6.2).
- HQ-E1-1 (default implemented). HQ-E1-2 (p2 stands; split binding-mismatch is security's call). HQ-E1-4 interim keep-all.

**These truly need the owner, compliance or legal:**
- HD-CTF-1(a): the rule content per jurisdiction or licence.
- HD-CTF-3, HD-CTF-4, HD-CTF-5.
- HD-CTF-7: the evidence obligations.
- HD-CTF-8 N2: player and regulator notification and deadlines.
- HD-CTF-9 permanent answer.
- HQ-E1-1 if deviating from the default.
- HQ-E1-2 formal confirmation.
- HQ-E1-3.
- HQ-E1-4: retention period and erasure duty.
- Configuration rows that only the owner may populate:
  - `financial_policy_required_approvals` threshold/count for `closed_tenant_hold_resolution` (HD-PRH2-3; none seeded);
  - `closed_tenant_resolution_rules` content.

**None of the 13 blocks a real PSP integration for active tenants.**
- HD-CTF-* block any tenant-closure flow (together with H-SEC-5 for deposits, R-CT-6).
- HQ-E1-2 gates 0114 reaching a real tenant.
- HQ-E1-3 gates real-money KYC in jurisdictions with deadlines.
- The real-PSP prerequisites live elsewhere (`docs/HANDOVER.md:79`, e.g. PAY-RECON-PARKED-CAPTURE-STANDING-1).

## 4. Ranked "ask the owner" list (one-line answers)

1. **HD-CTF-1(a):** For Anjouan / our own licence, which outcomes are permitted per class: retain only, plus release
   to player cash, or plus governed dispatch? (Or "obtain legal opinion".)
2. **HD-CTF-1(b):** Is the platform rule a floor that jurisdictions and own-licence operators can only tighten, or does
   the most specific level win?
3. **HD-CTF-3 / HD-CTF-4:** After release, and for balances never withdrawn, how must a closed tenant's player get
   their money? (Platform-run withdrawal, operator/successor, or legal TBD.)
4. **HD-CTF-5:** Is any unclaimed-funds, escrow, regulator or successor transfer legally required? (Yes, which one /
   no / unknown.)
5. **HD-CTF-6 + HD-CTF-9:** Interim: refuse closing a tenant with open withdrawal holds, and treat `closed` as
   permanent? (Yes/no.)
6. **HD-CTF-2:** Confirm that closed-tenant staff get no access, and that grant-chain separation is required (a minimum
   of 3 platform admins)? (Yes/no.)
7. **HQ-E1-2 + HQ-E1-3:** Confirm KYC terminal-failure alert severity p2, and whether Anjouan imposes any KYC
   submission or completion deadline? (Confirm p2; deadline yes/no.)
8. **HQ-E1-4 + HD-CTF-8:** Retention period for KYC outbox and closed-tenant records, and are any player or regulator
   notifications required on tenant closure? (Period or "legal TBD"; notifications yes/no.)
