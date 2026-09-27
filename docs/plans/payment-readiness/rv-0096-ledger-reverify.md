# RV-0096 — Ledger-finance re-verification of ADR 0096 (revised)

**Reviewer:** `ledger-finance`. **Date:** 2026-09-27. **Repo:** `HEAD 6c8731e`
(ADR 0096 revision landed in `d50327b`).
**Scope:** I checked the ADR 0096 body text (§2.2, §2.6, §3.2, §3.6, §5, §7, §8) against my
original conditions C1–C7 (§12.2). I did not rely on the §14.2 revision record alone. I also
cross-checked it against:
- ADR 0095 §4.3 (T1p, W-KYC, T2, T12, M3), §4.7 and §5.2, and LF95-C10;
- the current code in `internal/withdrawal/withdrawal.go` (`RequestWithdrawal` :291,
  `getByTenantPlayerIdempotencyKey` :210, the release `UPDATE`s at :813/:1108/:1204/:1289);
- `internal/httpserver/withdrawal_handlers.go` :106-140;
- migrations 0026 and 0034 (the governance trigger is on `withdrawal_approvals` only; there is no
  DB state-transition trigger on `withdrawal_requests`);
- migration 0040 (`kyc_verifications.person_id` and `expires_at` already exist).

The ADR was not edited.

## Verdict

**SIGN-OFF STILL CONDITIONAL.** Five of the seven conditions are CLOSED. C1 and C2 are **OPEN on
narrow text defects only**: the design substance is correct, but the body text contradicts itself
or omits something I asked for explicitly. Each fix is a sentence or a paragraph. There is still
no veto: no floating point, no historical mutation, no direct balance `UPDATE`, and no money path
without an idempotency key. PRH-I3 must not start until N1 and N2 are fixed in the ADR text. The
§14.2 row statuses "Design-satisfied … in full" for C1/C2 are therefore not accurate yet.

| Cond. | Verdict | Basis |
|---|---|---|
| C1 — denial commits, no rollback loss; lookup → gate → insert | **OPEN (text)** | The order and the typed-result design are correct in §8 item 3 and §5. The general rule in §3.6, its required test, and the §7.6 row contradict the payout path (N1). The "existing/unchanged" pre-insert lookup does not exist in code, and the replay/mismatch semantics are underspecified (N2). |
| C2 — `DenyForCompliance` shape | **OPEN (text)** | Everything I required is present except one item: setting `release_ledger_transaction_id` in the same conditional `UPDATE` (N3). |
| C3 — no pay-out on known `failed` status | **CLOSED** | §3.2 point 1 goes further than C3: `passed` is required on every withdrawal and there is no exemption. A related read-key inconsistency is recorded as N5; it belongs to security/identity-compliance. |
| C4 — TOCTOU bounded | **CLOSED** | §5 "Concurrency and TOCTOU" adopts (a)/(b)/(c) verbatim. Re-entry re-gating is carried into ADR 0095 T2/T12. |
| C5 — ADR 0095 fit; never release after possible dispatch | **CLOSED** | See the C5 section below. Two cross-reference gaps are non-blocking (N4, N6). |
| C6 — threshold arithmetic | **CLOSED** | §2.6(e): `NUMERIC`/`big.Int`, never `float64`, same `asset_code` only, settled `deposit_completed` postings rather than `deposit_intents.amount`, and netting/in-flight handling deferred to HD-KYC-1. The §14.1 summary contradicts this (N7, cosmetic). |
| C7 — required tests | **CLOSED** | Every C7 item appears individually in §7.2/§7.4/§7.6. The wording fix for the payout-deny test is part of N1. |

## C1 — detail

Satisfied:
- §8 item 3 gives `RequestWithdrawal` the required order: a read-only lookup by
  `(tenant, player, idempotency_key)`, then replay-return, then the KYC gate, then either
  deny-commit or allow → `IdempotentInsert` → accounts → L3 → `Post`.
- A deny inserts no `withdrawal_requests` row and posts nothing. It returns `ErrKYCRequired`,
  which the handler **commits**. §5 places the gate before any state-changing statement.
- In the handler today, the statements before `RequestWithdrawal` in the `WithTenant` closure are
  reads (`GetPlayerAccountByID`, `wallet.GetByPlayerAndAsset`), so a committed deny transaction
  really does contain only the decision and audit rows.
- Payout: `(wr, ErrKYCDeniedCommitted)` is committed and can never reach `Withdraw`. Under
  ADR 0095, `Withdraw` is Phase B, which only follows a T1p commit, and W-KYC and T1p exclude
  each other under L1.

**N1 (blocking C1): the general deny rule contradicts the payout deny.** §3.6 "Commit discipline,
corrected", second bullet, says that on deny "the transaction that commits contains the decision
row and the audit record and **no domain effect** — no ledger posting, no state-machine
INSERT/UPDATE". Its "Required test" says: "for every one of the five enforcement points, a denial
leaves … **zero ledger effect**". The §7.6 row repeats this ("on `deny` they commit **without**
it").

At enforcement point #5 that is wrong. A payout deny **must** commit, atomically, with the
decision and audit rows:
- the `withdrawal_rejected` reversal posting;
- the `approved → rejected` transition.

Otherwise the hold strands and every later submit re-denies forever. An implementer or QA who
follows §3.6/§7.6 literally would either write a test asserting zero ledger effect at #5, or split
`DenyForCompliance` into a separate transaction. Either outcome is the C1 bug class.

Required fix: carve #5 out explicitly in §3.6 bullet 2, the §3.6 required test and §7.6. At #5
the committed deny transaction is "decision + audit + exactly one `withdrawal_rejected` reversal
+ `approved→rejected`", and nothing else (no attempt row, no provider call). My original C1 also
asked for a test proving that the reversal, the state change, the decision row and the audit all
survive **the handler's** transaction. The §7.2 payout row asserts the reversal, decision and
audit, but not the persisted `rejected` state, and it does not say that it runs through the
handler rather than the domain function. Add both.

**N2 (blocking C1): the pre-insert lookup is new code, not "existing"/"unchanged".** §5 says the
gate runs "immediately after the **existing** idempotency-replay lookup", and §8 item 3 says
"replay short-circuit, **unchanged**". In the code, `RequestWithdrawal` has no read-only lookup
before the insert. Replay is detected only *after* `db.IdempotentInsert` reports a conflict
(`withdrawal.go:306-330`), and only then does it call `getByTenantPlayerIdempotencyKey`. The ADR
must state the lookup → gate → insert shape as a change. It must also require:
- (a) the new pre-lookup, on a hit, applies the same wallet/asset/amount mismatch check and
  returns `ErrIdempotencyKeyReused` on mismatch. It must never blindly return the existing row;
- (b) the post-insert conflict branch stays as it is. Two concurrent same-key requests can both
  miss the pre-lookup and both pass the gate, and the unique constraint plus the conflict branch
  is what keeps that exactly-once. The pre-lookup is an optimisation of the replay path and does
  not replace the DB guarantee;
- (c) a denied request leaves no row, so a retry with the same key after KYC passes creates a
  fresh request. That is correct because nothing was posted, but it must be stated, and the
  §7.2 QA gap 4 row should cover the withdrawal key as well as the deposit key.

## C2 — detail

Satisfied in §8 item 3:
- a new function that is not `Reject`, runs under the same L1 `lockRequestForUpdate`, adds the
  `approved→rejected` edge, and requires an amendment to `withdrawal-state-machine.md`;
- accounts resolved `(hold, cash)` via `GetOrCreateAccounts`, debit `player_withdrawal_hold`,
  credit `player_cash` for `wr.Amount`;
- `withdrawal_rejected`, with `ReversesTransactionID = wr.HoldLedgerTransactionID` and
  `CorrelationID = requestID`;
- key `requestID + ":kyc_denied"`, kept separate from `:rejected`/`:failed`;
- exactly-once through L1 plus `UPDATE … WHERE state='approved'`, with `RowsAffected()==0` →
  `ErrStateConflict` rolling back the posting;
- no `reason_code` on the ledger transaction; the audit uses `ActorSystem` and
  `withdrawal.rejected_kyc` with the required metadata;
- no `withdrawal_approvals` row; release to `player_cash` by default, with an AML-freeze left as a
  separate HD;
- the partial-unique-index backstop marked as recommended.

I checked two further points against the schema. Migration 0034's governance trigger fires only
on `withdrawal_approvals` inserts, so a system-driven `approved→rejected` with no approval row is
not blocked. There is no DB transition trigger on `withdrawal_requests.state`. The `withdrawal-
state-machine.md` amendment is still outstanding (it has no KYC edge today). That is an
implementation gate, which is acceptable.

**N3 (blocking C2): `release_ledger_transaction_id` is missing.** My C2 required: "Set
`release_ledger_transaction_id` in the same `UPDATE`." The body text does not say this; §8 item
3 mentions only the state predicate. Every existing release path (`withdrawal.go:813, 1108, 1204,
1289`) sets it in the same statement. Without it, the reconciliation and audit linkage from the
request to its release transaction is lost, and the §8 audit metadata field
`release_ledger_transaction` has no durable counterpart on the row. Required fix: one sentence in
§8 item 3.

## C5 — detail (fit with ADR 0095)

- **Placement.** ADR 0096 §5 and §8 item 9 require the payout gate inside Phase A, under L1,
  before the T1p CAS. ADR 0095 T1p (§4.3), W-KYC and LF95-C10(a) give the matching order:
  approver eligibility → L1 → KYC gate → kill switch → T1p. T1p is the last commit before the
  first `Withdraw`. The two ADRs are consistent. ADR 0095 Phase A0 routing (a read-only
  transaction before the gate) makes no provider money call, so it does not breach "never flows
  into `RouteProvider`/`Withdraw`" in any way that matters financially.
- **Never release after possible dispatch.** ADR 0096 §5 limits `DenyForCompliance` to `approved`
  and forbids any KYC-driven automated release once a request may have reached the provider
  (`submitting`/`pending`/`ambiguous`/`disputed`/`ever_possibly_sent`, and `submitted` today).
  ADR 0095 enforces the same rule:
  - INV-IO-7 ("a KYC outcome … never enough");
  - the §4.3 forbidden list (`→rejected` for a payout only from `created ∧ ¬ever_possibly_sent`);
  - §4.7 ("W-KYC here and only here").
- **Re-entry.** ADR 0095 T2 (the sweeper re-claim of `created`) and T12 (resend from `ambiguous`)
  both re-run the payout KYC gate. On a non-pass, T2 leaves the attempt in `created` with an
  escalation, and the hold is released only by manual M3 (`kyc_denied`, `withdrawal.Fail`, safe
  by INV-IO-9). On a non-pass, T12 means no resend and the attempt resolves only through evidence.
  This satisfies C4(b)/C5.
- **N4 (non-blocking, cross-reference).** ADR 0096 §5 says the sweeper claim "must re-run the KYC
  gate" but not what a deny does there. An implementer reading only ADR 0096 might call
  `DenyForCompliance` on a T2 deny. It would fail safely with `ErrStateConflict`, because the
  withdrawal is already `submitted`, but the correct route is ADR 0095 M3. Add one sentence to
  §5 pointing at ADR 0095 T2/M3/T12 and LF95-C10(b)/(c).
- **N6 (non-blocking, but must be fixed before the raw-guard test is written).** The §7.5 and §8
  item 3 raw-guard test (security condition 6) targets `withdrawal.MarkSubmitted`. Under ADR 0095
  the dispatch-authorising transition is T1p (`ClaimForDispatch`, `approved→submitted`) plus the
  attempt claims T2/T12. `MarkSubmitted` then only records the reference in Phase C, after the
  call. A guard on `MarkSubmitted` would pass while guarding the wrong edge. Retarget it to "every
  claim that precedes a `Withdraw` (T1p, payout T2, payout T12) ran `EvaluateEnforcement` in the
  same transaction". Security owns this condition; I flag it because it is the enforcement
  mechanism for C4(b)/C5.
- **Note for `architect` (ADR 0095, not ADR 0096).** ADR 0095 §5.2's "Flow" cell (Phase A:
  "approver eligibility → L1 lock → T1p") leaves out the KYC gate, although the T1p row and
  LF95-C10(a) include it. Align the text so ADR 0095 does not contradict itself.

## Other new findings

- **N5 (needs security/identity-compliance to resolve; relates to C3).** The read key for the
  structural withdrawal rule is inconsistent:
  - §2.2 (`PersonID`), §3.2 point 1 and the second half of §5's performance bullet scope it
    **per `Person`**;
  - §2.6(a), the first half of the same §5 bullet, §2.3 ("for this tenant/brand") and HD-KYC-6
    key the latest-row read by `(tenant_id, brand_id, player_account_id)`.

  The difference matters in one case. PlayerAccount A has a latest verification of `rejected`,
  and PlayerAccount B under the same Person has an older `approved` row. A per-account read lets
  B withdraw. Pick one key and state it in §2.6(a). It must stay within the tenant, under RLS; do
  not widen reuse across tenants, which is HD-KYC-6. C3 as I wrote it is satisfied either way, so
  it stays CLOSED.
- **N7 (cosmetic).**
  - §14.1's row for security 4(e) says totals are computed "across all assets/wallets". The body,
    §2.6(e), correctly says same `asset_code` only (C6). Fix the record row, not the body.
  - §14.2's last row says "10.3 Veto check"; it should be §12.3.
  - §3.2 point 1 cites "§3.6 point (b)" for expiry; it should be §2.6(b).
- **N8 (informational, pre-existing, not introduced here).** `EnforcementParams.Amount int64`
  mirrors the existing `withdrawal.RequestParams.Amount`. It cannot hold 18-exponent crypto amounts
  above about 9.22 units. Crypto rails (row #19) are deferred, so this is not blocking. When they
  are built, the enforcement amount type must follow the platform's `NUMERIC(38,0)` crypto
  representation and must not silently truncate.

## To close

1. Fix N1, N2 and N3 in the ADR 0096 text (identity-compliance, as ADR owner). Then C1 and C2 can
   be CLOSED on re-read, and PRH-I3 may start as far as ledger-finance is concerned.
2. N4, N6 and N7 before PRH-I3 test authoring. N5 goes to security/identity-compliance.
3. Unchanged: the final `DenyForCompliance` implementation still needs ledger-finance code review
   against C1/C2/C7 before it can be marked IMPLEMENTED.

Label: this is a design re-review. Nothing is implemented, and the mechanism remains
**NOT IMPLEMENTED**.
