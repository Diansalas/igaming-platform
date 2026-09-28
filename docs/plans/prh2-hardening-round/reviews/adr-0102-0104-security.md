# Security review — ADRs 0102–0104 (2026-09-28)

Reviewed at `0939c5a`, read-only. Claims were checked against code by reading and grep; no tests or DB probes were run.

| ADR | Verdict |
|---|---|
| 0102 durable alerting | **ACCEPT WITH CONDITIONS** (C-102-1…9) |
| 0103 launch-token bootstrap | **ACCEPT WITH CONDITIONS** (C-103-1…6); B starts after A (0108) merges |
| 0104 tenant-visible audit | **ACCEPT WITH CONDITIONS** (C-104-1…6) |

No High findings; no redesign needed.

## Findings

**ADR 0102 (alerting)**
- **SR-1 (Med)** `RaiseDetached` chooses its scope from the caller's Alert struct. That lets business code borrow `alert_dispatcher`, lets a swallowed wrong-subject 42501 be retried in the *subject's* scope (laundering a cross-tenant write), and records a false `raised_by_scope`. → C-102-1.
- **SR-2 (Med)** `alert_occurrences` has no `kind` column, so the meta-only WITH CHECK is not expressible. Fix with a copied `kind` or an `EXISTS` WITH CHECK, plus an RLS test.
- **SR-3 (Med)** `request_id` is caller-controlled free text (`middleware.go:28-33`) and flows into tenant-readable P1s. Add a charset CHECK `^[A-Za-z0-9_.:-]{1,128}$` (or use a platform-minted id); build rows 9–13 discriminators from server-side ids only, and bound the alert count per entity.
- **SR-4 (Low-Med)** "Never prevents the brake" is false for transient SQLSTATEs that propagate. Correct the §7.6 text; `reconciliation.run_failed` (detached P1) becomes **required** in I-wire; add a kill-switch injected-failure test.
- **SR-5 (Low)** `ON CONFLICT` inside REPEATABLE READ transactions (rows 6–7) raises 40001 against a row committed after the snapshot. Keep raises there conflict-free and update the `tenant_snapshot.go` comment.
- **SR-6 (Low)** Meta-Kinds must not recurse on their own unrouted or dead events.
- **SR-7 (Low)** Before any real channel: route changes need four-eyes, or raise a p2 `alerting.route_changed`; superseding the last p1 route is refused.
- **SR-8 (Info)** Run failure-path detached raises inside the ADR 0097 admission hold.

**ADR 0103 (casino bootstrap)**
- **SB-1 (Med)** Step 2 must start with the ADR 0094 in-tx Redeem+Recheck of the verified handle (as `orchestrator.go:1020-1027` does), and parse only from verified bytes. Test: a handle revoked between verify and tx → uniform 401, no write.
- **SB-2 (Low)** Add `provider_game_id` to the CAS, plus a mutant.
- **SB-3 (Low)** §11 item 3 overstates `postBet`: it re-checks risk (with the frozen jurisdiction), capability, RG and the provider binding, but not jurisdiction resolution, the game blocklist or `casino_games.status`.
- **SB-4 (Med, pre-existing)** `postBet` never reads `casino_games.status`, the documented platform game kill switch. **Registered as CAS-GAME-KILL-BET-1**; it should block real-money casino launch. It does not block B.
- **SB-5 (Low)** Betting does not require a bootstrap. Registered as CAS-BET-REQUIRES-BOOTSTRAP-1.
- **SB-6 (Low)** Store a digest of the request and compare it on replay.
- **SB-7 (Low)** The RLS family must also require `app.platform_service_id` and 0099's `app.acting_*` GUCs unset.
- **SB-8 (Info)** CAS-PLAYER-REF-1 (already registered) must close before a real vendor.

**ADR 0104 (tenant-visible audit)**
- **SA-1 (Low)** Add `casino/rejections.go:257-266` to the readers-unchanged tests.
- **SA-2 (Low)** The trigger must also require `app.platform_service_id` and the `app.acting_*` GUCs unset.
- **SA-3 (Low-Med, owned by 0099)** 0099 acting sessions leave `app.tenant_id` unset, so they satisfy `audit_log` `dual_scope_isolation`'s `tenant_id IS NULL` arm: they could read every platform audit row and insert platform rows. Resolve in 0099/0112.
- **SA-4 (Low)** `display_name` is mutable and resolved on read. → C-104-4.
- **SA-5 (Low)** The `approval_chain` source and how pseudonymity applies to it are underspecified. → C-104-5.
- **SA-6 (Info)** **LEGAL/PRIVACY REVIEW REQUIRED** before any presentation policy enables `show_network_metadata`.

## Rulings

**Q1 — dispatcher meta-alert INSERT: ACCEPTED**, as a narrow exception to S-7.4; the metric-only alternative is rejected.
- WITH CHECK: `tenant_id IS NULL AND subject_tenant_id IS NULL AND kind IN ('alerting.unrouted','alerting.delivery_dead')`, with every other scope GUC unset. The same restriction applies on occurrences.
- A trigger forces `state='open'` and `raised_by_scope='platform_service'`.
- No UPDATE, DELETE or route writes.
- Only dispatcher code uses the identity (static test).

**Q2 — no abort-on-failure for any PRH-2 Kind: CONFIRMED**, with two amendments:
- the text must state that transient SQLSTATEs still propagate;
- `reconciliation.run_failed` is **required** in I-wire.

**Q3 — the swallow allowlist (22, 23, 42501, P0001) plus a mandatory detached retry: MEETS addendum (a)–(e)**, with conditions:
- the savepoint encloses only `Raise`, and a mutant widening it must be killed;
- 25P02, 40001, 40P01, 55P03 and 57014 propagate;
- the per-site real-scope raise test is mandatory;
- the retry reopens the originating scope;
- it must be written down that log plus metric is the real signal for deterministic failures.

**Q4 — no risk/jurisdiction re-check at bootstrap: ACCEPTED**, with the rationale corrected (SB-3).

**Q5 — the timing residual: ACCEPTED.** Every step 1–3 refusal performs the same lookups, writes nothing and uses no class-specific sleeps. Re-rule if the endpoint becomes reachable without authentication or token entropy drops.

**Q6 — gate denial → `revoked`, then 403: ACCEPTED.**
- Revoke only on a definitive denial; an evaluation error rolls back and returns 5xx.
- Assert that the revoke returned `true`.
- Audit in the same tx (system actor, `prior_status`, reason enum).
- The 403 body is constant.

**Q7 — HD-PRH2-5 presentation: ACCEPTED.**
- Trigger correct in shape, plus SA-2. It must **not** check staff `status`: the audit records what happened.
- CHECK, FK and NULLIF-form RLS are correct.
- No new write power.
- Either the full table or the resolver-only form is acceptable, provided the defaults are "identified, network hidden" and the resolver fails closed.

## Implementation conditions

**0102**
- **C-102-1 (detached-raise scope)**
  - `Deferred` records the originating scope, and `RaiseDetached` reopens exactly that scope.
  - A subject different from the originating tenant is refused in Go.
  - A per-Kind `requires_subject` column plus trigger.
  - Dispatcher identity used only by dispatcher code (static test).
- **C-102-2:** meta-only INSERT on both tables.
- **C-102-3:** `request_id` charset CHECK; server-side discriminators.
- **C-102-4:** corrected §7.6 text; kill-switch injected-failure test; `run_failed` required.
- **C-102-5:** no in-snapshot conflicts; updated `tenant_snapshot.go` comment.
- **C-102-6:** meta-Kinds do not recurse.
- **C-102-7:** the state guard requires a validated actor; ack/resolve endpoints need a named permission; no tenant ack endpoint in PRH-2.
- **C-102-8:** SR-7 before a real channel; SR-8 admission hold.
- **C-102-9:** every family predicate excludes the `app.acting_*` GUCs.

**0103**
- **C-103-1:** Redeem+Recheck first.
- **C-103-2:** `provider_game_id` in the CAS.
- **C-103-3:** the Q6 conditions.
- **C-103-4:** request digest.
- **C-103-5:** RLS exclusions; raw-token scan of the new error paths.
- **C-103-6:** text and citation fixes (`:311` → `:314`); registry rows.

**0104**
- **C-104-1:** trigger unset-list extended; no `status` check.
- **C-104-2:** `rejections.go` added to the readers tests.
- **C-104-3:** the name lookup selects only `id, display_name` for ids in the RLS-filtered result; never email.
- **C-104-4:** `display_name` 1–100 characters, no control characters, written only by the audited staff path, self-rename audited, staff id always shown, output-encoded.
- **C-104-5:** `approval_chain` built only from RLS-filtered rows; pseudonyms via a stored random mapping; resolver failure fails closed.
- **C-104-6:** security diff review of the implementation; SA-3 tracked in 0099/0112; SA-6 legal flag.

## Launch flags

- PAY-P1-MULTISUCCESS-ALERT-1 stays launch-blocking until real recipients **and** a real channel exist.
- KS-AUDIT-TENANT-1 stays launch-blocking until G1 is implemented and reviewed.
- CAS-GAME-KILL-BET-1 blocks real-money casino launch.

## Orchestrator dispositions (2026-09-28)

**Conflict: reconciliation dedup key** (LF F6 stable keys vs SR-5 run-unique keys in snapshot transactions).
- Resolved by adopting **LF F7 option (b)**: at the REPEATABLE READ sites (casino_statement, the payment_statement match), the alert is raised **post-commit, detached**, in a fresh READ COMMITTED transaction.
- Keys are stable (`stream:<stream>[:provider:<id>]`, run id as an attribute), so a persisting condition is one open alert with growing occurrences (LF F6).
- There is no in-snapshot `ON CONFLICT`, so SR-5 holds.
- Non-snapshot reconciliation sites may raise in-tx under the savepoint rule.

**Open item for security** (asked with the 0099–0101 review): LF F3's terminal fallback `alerting.raise_failed` would be a **third** dispatcher meta-Kind, which would widen the Q1 list.
