# Security confirmation — ADR 0099–0101 rev 2 (2026-09-28)

Reviewer: `security` specialist. The orchestrator recorded this file, because the reviewer's rules forbid it
from writing report files. Scope: the ADR bodies at `ca3014d`, checked against
[`adr-0099-0101-security.md`](adr-0099-0101-security.md) and against the code and migrations where
the ADRs make claims. Review only: no code was changed, no DB was used, and no role or credential was
touched.

**Verdict: CONFIRMED WITH CONDITIONS.**

K1 may start once C-1 is written into the ADR text. The orchestrator has done this: ADR 0099 §6.3, §10.1 and
A-4. C-2 and C-3 bind K2 and K3. C-4 applies only if the ledger-finance ruling-1 vs 5(c) proposal is adopted.

## 1. Earlier findings: all addressed

| Earlier finding | Where rev 2 addresses it | Verified |
|---|---|---|
| K1-1 / C-99-1 (NULL-arm exposure) | 0099 §6.2 corrected. §6.4 adds `AS RESTRICTIVE` policies per command on `staff_users`, `audit_log`, `sessions`, `login_attempts`, `persons`, `player_restrictions` and `risk_rules`. Also §10.6, A-4, A-17 and A-18. | Yes. The reviewer's own scan of every `CREATE POLICY` with a NULL arm found that each non-SELECT NULL arm outside the seven tables requires a platform-admin or platform-service GUC. |
| K1-2 / C-99-2 (`compliance` grantee) | §3.3: only `finance` and G-P2 `platform_admin`; INV-CAP-7; A-2 and its mutant | Yes |
| K1-3 / C-99-7 | §8.1 (revoke is the emergency stop); §7.5 and §16.1 cover STAFF-LIFECYCLE-1 | Yes |
| K1-4 / C-99-3 | §10.6 `audit_log_acting_actor`; A-12 | Yes |
| K1-5 / C-99-4 | §9: INV-CAP-6 reworded to "no HTTP API"; A-14/A-14b; the two-person `seed-admin` runbook | Yes |
| K1-6 / C-99-5 | §6.8; A-19 | Yes |
| C-99-6 | §8.2: R-13, NOT NULL `valid_until` for G-P2, and the settings row. Without a settings row, G-P2 is refused. A-10. | Yes |
| C-99-8 | §6.1 setter contract; A-16 | Yes |
| K2-1 / C-100-2 | 0100 §6.7 choice (b); §3.2 | Yes |
| K2-2 / C-100-4 | §11 B-21; §12 detective kind; LEDGER-MANUAL-ADJ-LINK-1 is launch-blocking | Yes |
| K2-3 / C-100-5 | §5.1: note of 1–1000 bytes | Yes |
| C-100-1 | §3.3 (MA011); INV-ADJ-3; B-10 | Yes |
| C-100-3 | §5.2 and §5.4 (`goodwill_credit` refused for non-active tenants); B-26 | Yes |
| C-100-6 | B-3 | Yes |
| K3-1 / C-101-1 | 0101 §6.4: the acting UPDATE WITH CHECK is bound to an executing M2 with `executed_txid = txid_current()`; the `deposit_intents` WITH CHECK is `false`; C-14b and its mutant | Yes |
| K3-2 / C-101-2 | §5.1 and §8.2: `evidence_ref_hash` NOT NULL for M2 | Yes |
| K3-3 / C-101-3 | §5.4: `ValidatePaymentReference` at every payments ingress, including statement import; C-9 | Yes |
| C-101-4 | §5.1: tenant status recorded at submission and at execution; C-21 | Yes |

## 2. The specific checks

- **Acting GUCs cannot be set or spoofed by a tenant session: confirmed, with C-1.**
  - P comes only from the verified token subject, and X only from the `canActOnTenant` route target.
  - There is one sole setter, and A-16 statically forbids raw `set_config('app.acting_…')`.
  - Spoofing inside the DB would need arbitrary SQL in the app role. That is the same trust model as `app.tenant_id`, and strictly less powerful than forging `app.tenant_id`.
  - A spoofed shape without a valid grant gets nothing from any policy that uses `financial_acting_session_valid()`. The exception is the disclosed TM-3 residual on the two recursion-exempt tables.
  - A mixed session loses power rather than gaining it.
- **HD-PRH2-2 (platform co-approval, no self-grant): confirmed.** R-1..R-4, R-6, R-7 and R-8; the approval and the grant are inserted in one transaction.
- **HD-PRH2-6 (platform staff act only through an explicit tenant-scoped grant): confirmed.**
  - The plain platform family has no policy on the K2/K3 or ledger tables.
  - Acting requires an in-force, time-bounded G-P2 grant for exactly tenant X.
  - Acting writes are fenced.
- **HD-PRH2-7 (tenants may only tighten): confirmed.**
  - Evaluation takes the MAX, with `GREATEST(1, …)`.
  - A tighten-only trigger (MA010) and an authorship matrix enforce it.
  - Policy rows are inserted only by the change-approval trigger.
- **ADR 0101: confirmed.**
  - M1 is evidence-only (enforced by CHECK and by a `false` WITH CHECK).
  - M2 is payout-only and requires `submitted` (C-5 mutant).
  - The reserved prefix `platform-operator-declared:` is checked with `left()` against an IMMUTABLE function. An all-sessions `ledger_transactions` guard backs it, and 0114 refuses on a pre-existing prefix.
  - The allow-list refuses `amount_asset_mismatch` (MR010, C-5).
  - The posting keys match `withdrawal.go`.

## 3. Ledger-finance item: ruling 1 vs ruling 5(c)

The proposal is to accept, as causation for a `compensating_entry` with `credit_player`, a reserved-prefix
`withdrawal_completed` that has a `player_withdrawal_hold` leg. From a security view it is **acceptable
under C-4**. Its parts:

- **(a) The causation is keyed strictly on the reserved prefix.**
  - It requires `left(provider_tx_id, 27) = payment_reserved_ref_prefix()` **and** `transaction_type = 'withdrawal_completed'`.
  - Add a mutant that accepts a non-prefixed `withdrawal_completed`.
  - Without this, a genuine provider Step B could be "compensated", paying the player twice.
- **(b) No silent clearing.**
  - Suppose a compensating credit caused by an M2 Step B is later followed by a confirming provider `succeeded` line. That must raise a standing, unwindowed **P1**, for example a new kind `pay_declared_paid_compensated_but_paid` or a re-raise of `pay_declared_not_paid_but_paid`.
  - It clears only when compensating debits against that credit total the credited amount.
- **(c) Person separation across the two four-eyes steps.**
  - The Persons counted on the M2 resolution may not be initiator or counted approver on a compensation whose causation is that resolution's Step B.
  - This is enforced by trigger and at execution, and tested.
- **(d) Shape limits.**
  - Credit only; a debit with this causation is refused.
  - Same tenant, wallet and asset.
  - The cumulative cap is set by the hold leg, under the L2 lock.
  - `evidence_ref_hash` is required.
- **(e) No other widening.**
  - `deposit`, `deposit_reversal` and `tombstone` causations stay refused.
  - INV-ADJ-5 is unchanged.

Until ledger-finance confirms, the ADR's fail-closed interim is correct: ruling 1 applies literally, the 5(c) clearing path is disabled, and the finding stands.

## 4. Remaining conditions

1. **C-1 (K1, before code)** — `financial_acting_gucs_exact()` must require both acting GUCs set
   **and** `app.tenant_id`, `app.principal_id`, `app.platform_admin_principal_id`,
   `app.player_account_id` and `app.platform_service_id` unset. A-4's mixed-session cases must cover
   `staff_users` and `staff_capability_grants`. **Written into ADR 0099 §6.3, §10.1 and A-4 by the orchestrator.**
2. **C-2 (K2)** — B-3's K1-1 negatives must run in K2's real acting executor transaction, not only in a
   K1 fixture. The same applies to A-19 column discipline via B-22.
3. **C-3 (K3)** — C-14b must show that `withdrawal.Complete`/`Fail` under an acting session succeed only
   inside the governed M2 transaction. Every table they touch must be covered by an acting policy or a fence, and the positive test must find any gap before merge.
4. **C-4 (only if the ledger-finance proposal is adopted)** — all of §3 (a)–(e), with tests and mutants.

**Launch flags (unchanged, for the human):**
- TM-7: two colluding platform admins, and the `seed-admin` trust root;
- TM-10: legal review before any own-licence G-P2 grant;
- HD-PRH2-8 must be answered;
- LEDGER-MANUAL-ADJ-LINK-1 before the first real-money tenant.

## 5. Not covered

- The ADR text only; K1, K2 and K3 each need a security diff review.
- The ADR's "every table has RLS enabled" claim and its list of 19 fixture files were not verified.
- The SELECT NULL arms outside the seven tables were accepted as reference data without opening each one.
- The `player_open_payment_exposure` key column (ledger-finance) and the basis/context vocabulary (payments) were not checked.
