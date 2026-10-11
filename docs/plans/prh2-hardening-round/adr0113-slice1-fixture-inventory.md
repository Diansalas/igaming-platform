# ADR 0113 slice 1 - fixture re-inventory (C6)

Migration 0132 (`migrations/0132_withdrawal_state_machine_and_hold_invariant.up.sql`). Base `6bda8ac`. Line numbers
are at the base commit. Rules applied (ADR 0113 s5): no adversarial test deleted; a fixture that needs a later state
is created through the shared legal-path helper `internal/testsupport/wdpath` (E0 + E0b exactly as
`withdrawal.RequestWithdrawal` writes them, then the REAL Go writers `MoveToPendingReview` / `MarkSubmittedPending`
/ `AttachProviderReference` / `Complete` / `Fail` / `Cancel`; E3 / E4 as the same legal database edge with an
AUTOMATED approve / reject record, because the Go writers need a four-eyes policy and a person-linked staff
approver these fixtures do not exercise), as `igaming_runtime` on the shared test database (`wdpath.RuntimeFor`),
as the scratch owner only on a throwaway `scratchdb` database (the runtime role has no grants there; refused on
the shared database). `internal/withdrawal` and the packages it imports (`payoutinstrument`) cannot import
`wdpath` (import cycle) and carry a package-local copy of E0b / E2 (`a13PlantE0`; `plantHold`, `cancelLegally`). `pitest.WithoutBindingGuard` now fires check A (SET CONSTRAINTS ... IMMEDIATE / DEFERRED) before
its `ALTER TABLE`, which PostgreSQL refuses while the table has pending trigger events (55006).

Classes: **S** setup (direct non-`requested` state or no hold), **A** adversarial immediate refusal, **O** owner
tamper plant (ADR 0110 9.4: history the database now refuses), **M** migration test pinned below 0132, **G**
generic-refusal assertion now answered first by the 0132 guard.

## 1. Raw `INSERT INTO withdrawal_requests` in `_test.go` (28 sites, 22 files)

| Site (base) | Class | Old | New | Change |
|---|---|---|---|---|
| `payoutinstrument/migration_0126:112` | M | success at 0125 | success | unchanged (scratch pinned through 0126) |
| `payoutinstrument/binding:35` (`insertWithdrawal`) | S + A | success / PI04x | success / PI04x | success path adds `plantHold` (E0b) in the same tx; refusals unchanged |
| `payoutinstrument/binding:65` (`plantLegacy`) | O | success | success | owner plant (USER triggers off) also writes the E0b hold leg + link |
| `payoutinstrument/binding:118` | A | PI043 | PI043 | unchanged |
| `payoutinstrument/binding:167` | S + A | success / PI044 | success / PI044 | success adds `plantHold` |
| `payoutinstrument/migration_0123:314,400` | M | success | success | unchanged (pinned 0122/0123) |
| `payoutinstrument/state:476` | S | success | success | adds `plantHold` |
| `httpserver/admin_withdrawal_instrument_view:160` | S | success | success | `wdpath.Plant` (legacy binding) |
| `httpserver/iwire_webhook_reversal_link:40` | S | success (`submitted`, no hold, NULL provider) | WD025 -> success | Plant + walk to `submitted` with the attempt's provider `mock` |
| `httpserver/launch_authorisation_harness:309` | S | success | WD025/WD031 -> success | Plant + walk to state (runtime) |
| `reconciliation/payment_statement_fixround:123` | S | success | WD025 -> success | `legacySubmittedWithdrawal` |
| `reconciliation/payment_statement:714` | S | success | WD025 -> success | `legacySubmittedWithdrawal` |
| `reconciliation/prh2_r9_bound_clear:98,339` | S | success | WD025 -> success | `legacySubmittedWithdrawal` |
| `reconciliation/prh2_r16_res1_tighten:74` | S | success | WD025 -> success | `legacySubmittedWithdrawal` |
| `withdrawal/adversarial:116` | A | 42501 (RLS WITH CHECK) | 42501 | unchanged |
| `withdrawal/b13b_binding:327` | A | PI046 | PI046 | unchanged |
| `withdrawal/migration_0082_brand_pinning:58` | A | FK `withdrawal_requests_player_tenant_brand_fkey` | same | unchanged |
| `withdrawal/migration_0082_brand_pinning:78` | S (control) | success | WD031 -> success | `a13PlantE0` |
| `payments/pay_callback_mismatch_bind:520` | S | success | WD025 -> success | `legacyWithdrawalAt(submitted, provider e.id)` |
| `payments/dep_ref_validate:451` | S | success | WD025 -> success | `legacyWithdrawalAt(submitted, providerID)` |
| `payments/b13b_destination:457` | S | success | WD025 -> success | `legacyWithdrawalAt(approved)` |
| `payments/migration_0101:231` (`insertWithdrawalRequest`) | M + S | success | success / WD025 at head | pinned-0101 callers unchanged (raw INSERT); on a scratch database migrated through 0132 (`killswitch_claim_predicate_coverage:88,101`, `migration_0101:698`) the helper plants E0 + E0b and walks to the state |
| `payments/rvlf_i1_regression:488` | S | success | WD025 -> success | `legacyWithdrawalAt(submitted, mock-m4)` (scratch) |
| `payments/r31_settle_instrument_state:730` (`rawInsertWithdrawal`) | A | PI041 / PI042 | PI041 / PI042 | unchanged (refusal-only callers `:551`, `:768`) |
| `launchgov/withdrawal_insert_guard:50` (const; `:57`, `:141`, `:201`) | S + A | success / WD010 / WD011 | same | success paths are `wdpath.Plant` (raw INSERT + hold, no Go gate); refusals unchanged |
| `launchgov/closure:51` | S + O | success | WD025 -> success | Plant + walk to state (runtime); `reversed` (no writer) is an owner relabel of a legally `completed` row |

## 2. Hand-built postings to `player_withdrawal_hold` in tests

| Site (base) | Class | Old | New | Change |
|---|---|---|---|---|
| `ledger/lockorder:126` | S | success | WD040 -> success | the new account is `player_cash` of a second wallet (same L3 lock class; ledger-finance: lock-order coverage kept, the entry-order inversion is unchanged) |
| `ledger/lockorder:229` | - | success | success | account creation only (no entry): unchanged |
| `ledger/lockorder:739,774` | S | success | WD040 -> success | the fourth leg class is `player_locked_cash` (casino_bet shape); real concurrent holds are covered by `withdrawal` races |
| `ledger/replay:152` | - | mismatch | mismatch | the replay comparison refuses before any insert: unchanged |
| `risk/cumulative_leg:101` | S | success | WD040 -> success | `wdpath.Seed(requested)` (runtime, bound instrument): the same Flow 3 step A shape |
| `reconciliation/payment_statement:697`, `_fixround:107`, `prh2_r9:82,323`, `prh2_r16:58` | S | success | WD040 -> success | folded into `legacySubmittedWithdrawal` (hold key is now the request id) |
| `reconciliation/prh2_r9:285`, `prh2_r8_standing1:69` | O | success | WD040 -> success | the test's subject IS a hold leg no request names: owner tamper plant, B-i / B-ii disabled for those two postings only (`ownerTamperHoldLegs`) |
| `payments/hsec_hold_resolution_db:389,432,440` (`hsrPost`, cross-wallet) | A | CG030 / HR020 | CG030 / HR020 | unchanged (the acting fence and the key guard fire before B-i) |
| `payments/hsec_hold_resolution_db:690` (DeferredShapeCheck) | A | HR041 | HR041 (6 variants) / WD041 (`wrong_amount_both_legs`) | first answer with 0132 on; then 0132's B-i / B-ii / A lifted on the throwaway scratch: HR041 for all 7 (deeper guard) |
| `payments/k3_fence:27,29` | A | refused (generic) | refused | unchanged |
| `payments/m4_resolve1:825` | A | CG030 | CG030 | unchanged |
| `payments/m4_resolve1:1153` (planted wrong-key completion) | A + O | plant success, then MR041 | ordinary plant WD042; owner-tamper plant, then MR041 | owner tamper (B-i / B-ii off for the plant only) |
| `payments/m4_resolve1:1185` (planted wrong-key failure) | A + O | plant success, then MR041 | ordinary plant WD041; owner-tamper plant, then MR041 | owner tamper (B-i / B-ii off for the plant only) |
| `payments/b11_payout_unbound_hold:1602` (`Complete` under ANOTHER tenant's provider id) | S | success | WD041 -> success | B's withdrawal is submitted to provider id `pid` legally, then completed under it (a completion carries its own request's provider) |

## 3. Raw `UPDATE withdrawal_requests` in tests (38 sites, 14 files)

| Site (base) | Class | Old | New | Change |
|---|---|---|---|---|
| `payoutinstrument/binding:182,183,184` | G | error (0026/0123 immutable fields) | WD022 | assertion is `err != nil`: unchanged; the deeper trigger is pinned by `TestADR0113_DeeperGuardsStillArmedBehindTheStateGuard` |
| `payoutinstrument/binding:197` | G | error (immutable binding, legacy row) | WD022 | unchanged assertion |
| `payoutinstrument/binding:357`, `state:499` | O -> S | owner tamper `state='cancelled'` | legal E2 | `cancelLegally` (runtime, `:cancelled` reversal + release link) |
| `payoutinstrument/binding:372` | G | error (immutable instrument id) | WD020 (terminal row) | unchanged assertion |
| `payoutinstrument/migration_0123:415` | M | success | success | unchanged (pinned) |
| `httpserver/hsec_hold_resolution_api:132` | S | success | WD021 -> success | `wdpath.AdvanceAsRuntime(approved)` after `RequestWithdrawal` |
| `httpserver/launch_authorisation_http:724` | S | success (`rejected`, no release) | WD021 -> success | legal E2 `Cancel` (runtime); the test needs a terminal state |
| `withdrawal/adversarial:144` | G | error (0026) | WD022 | assertion STRENGTHENED: `pgErr.Code == "WD022"` |
| `payments/hsec_hold_resolution_db:482` (E7 without provider) | A | HR050/HR030/42501 | WD022 | + NEW case: the legal E7 shape (provider_id set) still wants HR050/HR030/42501 |
| `payments/hsec_hold_resolution_db:487` (rejected with an unrelated link) | A | HR030 | HR030 | unchanged (passes the guard: legal edge, release set) |
| `payments/hsec_hold_resolution_db:491,495` (approved -> cancelled / failed) | A | HR050 | WD021 | not edges; HR050 stays proven on the legal shapes (`:482` E7 case, FreezeTrigger) |
| `payments/hsec_hold_resolution_db:623` (borrowing a governed release link) | A | HR020 | HR020 | unchanged |
| `payments/hsec_hold_resolution_db:727` | A | HR041 | see section 2 `:690` | |
| `payments/hsec_hold_resolution_db:753` (FreezeTrigger control) | S | success (rolled back) | WD021 -> success | probe is the legal E7 shape |
| `payments/hsec_hold_resolution_db:784` (FreezeTrigger matrix) | A | HR050 x5 targets x4 sessions | HR050 for `rejected` (release link set) and `submitted` (provider set); WD021 for `cancelled` / `failed` / `requested` | probes carry the legal column shape |
| `payments/hsec_hold_resolution_db:1153-1157` (second UPDATE after the release) | A | HR030 | WD020 | the released row is terminal; HR030 stays proven by `:487` |
| `payments/hsec_hold_resolution_db:1220` (unreadable status) | S + A | success / HR050 | success / HR050 | probe is the legal E7 shape |
| `payments/hsec_hold_resolution_guard:199` | S | success | WD022 -> success | provider_id set (E7) |
| `payments/hsec_hold_resolution_guard:217`, `hsec_hold_resolution:488` (submitted -> approved) | O | success | WD021 -> success | owner tamper (`forceStateOwnerTamper`: the 0132 guard off for that statement, A still fired) |
| `payments/k3_fence:274,432,443` | A | refused (generic) | refused (WD022 / 42501) | unchanged assertions; `:432,:443` still prove the acting WITH CHECK (42501) |
| `payments/k3_ingress:568` (reserved-prefix CHECK) | A | 23514 | WD022 -> 23514 | probe targets a `submitted` request with no reference (E8), legal-path fixture added |
| `payments/k3_policy:110` (move out of submitted inside the executing tx) | S | success | WD021 -> success | legal E10 (`withdrawal.Fail`, the admitted `:failed` leg) |
| `payments/m4_resolve1:1134` (release link rewritten) | A | MR041 | WD020 | + NEW re-driven case through the legal edge (genuine Complete, resolution links another tx): MR041 |
| `payments/m4_resolve1:1167,1198` | A | MR041 | MR041 | unchanged (after the owner-tamper plants, section 2) |
| `payments/r31_settle_instrument_state:773` | A | refused (generic) | refused (WD022) | unchanged assertion |
| `payments/r31_settle_instrument_state:800` | O | success | success | unchanged (owner, USER triggers off) |

## 4. Changed assertions (complete list)

No adversarial test was deleted. No assertion was removed. Every change below either names the NEW first
refusal of an attack the database now stops earlier, or adapts a balance/count to the hold the fixture's own
request now (correctly) carries; each pre-existing deeper code is still asserted somewhere, and the
deeper-guard mutants (`adr0113-slice1-mutation-kill.txt`, D1-D6) prove those assertions are live.

| # | Test | Before | After |
|---|---|---|---|
| 1 | `withdrawal/adversarial` `TestWithdrawalRequests_AmountImmutableAfterInsert` | any PgError | + `Code == "WD022"` (stronger) |
| 2 | `launchgov/withdrawal_insert_guard` `TestWithdrawalInsertGuard_Catalog` | first BEFORE INSERT trigger = `aa_subject_active_guard` | first two = `[aa_state_transition_guard, aa_subject_active_guard]` (stricter) |
| 3 | `payments/hsec_db` `..._CTR3_...` "the state edge to submitted" | HR050/HR030/42501 | WD022; NEW case "legal E7 shape" keeps HR050/HR030/42501 |
| 4 | same, "cancelled", "failed" | HR050 | WD021 |
| 5 | `payments/hsec_db` `..._DeferredShapeCheck_...` | HR041 (7 variants) | 0132 on: HR041 (6) / WD041 (`wrong_amount_both_legs`); NEW phase with 0132 lifted: HR041 (7) |
| 6 | `payments/hsec_db` `..._FreezeTrigger` matrix | HR050 for 5 targets | HR050 for `rejected` / `submitted` (legal shapes); WD021 for `cancelled` / `failed` / `requested` |
| 7 | `payments/hsec_db` `..._ExecutingWindow_SecondUpdate...` (5 probes) | HR030 | WD020 |
| 8 | `payments/m4_resolve1` `TestM4_MR041_...` "release link rewritten" | MR041 | WD020; NEW re-driven case keeps MR041 |
| 9 | same, "planted wrong-key completion" / "failure" | (plant success) MR041 | NEW: ordinary plant WD042 / WD041; owner-tamper plant then MR041 (unchanged) |
| 10 | `payments/dep_ref_validate` `assertParkedNoMoney`, `..._ReferenceBoundToAPayoutAttempt_Parks`, `..._CascadeDrivenParkReturnsNoRedirect` | ledger tx count == 0, cash == 0 | count excludes exactly the payout fixtures' E0b hold transactions; cash == -(open fixture holds on the wallet) |
| 11 | `payments/pay_callback_mismatch_bind` `b3NoMoney` | cash == 0 | cash == -(open fixture holds on the wallet) |
| 12 | `payments/k3_ingress` C9 withdrawal CHECK probe | `WHERE tenant LIMIT 1` | `WHERE tenant AND state = 'submitted' AND provider_reference IS NULL` (23514 unchanged) |

Everything else in sections 1-3 is a fixture change with the assertion text unchanged.
