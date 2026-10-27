# lic27b record: AML low items from the final confirmation (ledger-finance LOW-1 / LOW-2, security S-3 / S-4 / S-5 / INFO)

Branch `lic27b-aml-lows` from integ-c `c6667c3` (migration head 0188). Software capability only; the only screening provider is the MOCK; no regulatory approval is claimed. No rg file was touched.

## 0. Which migration

Migration 0181 was edited IN PLACE (unreleased) and so was 0188 (also unreleased). No new migration file. Databases that applied either before this change must be REBUILT, never rolled back with the down.

## 1. Findings addressed

| Finding | Change | Label |
|---|---|---|
| LOW-1 / S-3 | `aml_screening_parked_overdue` (0181) is rewritten. (a) Only persons the gate STILL denies (`aml_screening_gate_denies(.., 'withdrawal')`, scanning at most 5000 persons) count, so a person whose screening cleared is neither parked nor overdue although their last payout decision row still says parked. (b) A denied person with NO payout decision row at all is overdue once gated longer than the threshold, measured from the same anchor as `aml_screening_gate_overdue` (oldest unresolved request, else the first gating version); the overdue test uses the earlier of the run start and that anchor, so a dead worker with no retries is visible. (c) The decision look-back is `LEAST(p_since, now - GREATEST(7 days, 2 x the largest gating alert threshold))`, derived in SQL, so a threshold up to 30 days works; `aml.ScreeningParkedWindow` is now the advisory minimum. With no gating policy it returns 0, 0. Counts only. | IMPLEMENTED |
| LOW-2 | `aml_screening_request_replace` with no request named: the new row takes the age of the person's oldest unresolved request, else the first gating version of the jurisdiction (never now), so queueing does not reset `aml_screening_gate_overdue`. | IMPLEMENTED |
| S-4 | Table CHECK on `aml_screening_requests`: `outcome IS DISTINCT FROM 'confirmed' OR 'sanctions' = ANY (match_lists)`. Consistent with Go (`screening_orchestrator.go` always adds the sanctions list to a confirmed outcome and rejects one that did not request it). | IMPLEMENTED |
| S-5 | `TestLic24_Static_Pre0182HookIsIntegrationOnly`: only plain comments are skipped (`//go:` lines are checked as code) and a code line naming the hook must EQUAL one of the allowed trimmed lines exactly (declaration, the single `if pre0182SchemaHook.Load() {`, and in the hook file `prev := pre0182SchemaHook.Swap(true)` / `return func() { pre0182SchemaHook.Store(prev) }`); a trailing comment or extra code on the same line fails. | IMPLEMENTED |
| INFO (discriminator) | `aml.RaiseScreeningTrapped` takes a source: the alert discriminator is `worker:tenant:<id>` (worker) or `sweeper:tenant:<id>` (sweeper), so the two meanings of `persons` raise separate alerts. Existing open alerts with the old `tenant:<id>` discriminator (dev databases only, 0181 is unreleased) are simply left to age out. | IMPLEMENTED |
| INFO (0188) | `CREATE INDEX` without `IF NOT EXISTS`: a differently defined same-named index makes the migration FAIL instead of being kept. A test creates such an index and expects the failure, and checks the definition on a clean database. | IMPLEMENTED |

## 2. Tests and mutation checks

New: `internal/aml/screening_lic27b_integration_test.go` (LOW-1 a / b / c, LOW-2, S-4, Migration0188). Declared pin change: `TestLic26_N9_ProductionSweepReadsAndLogsParkedCountsOnly` seeded parked decisions and expected `ScreeningParked == 1` with NO gating policy; under the new rule (only persons the gate still denies count) that is 0 / 0, so the test now asserts 0 / 0 against the real function and exercises the count log line through the injected read. Changed assertions (strengthened, none weakened): the lic18 trapped-alert test and the lic26 sweeper test now also require the source-prefixed discriminator. `scratchThrough181` in the lic23 test file became a wrapper of a new `scratchThrough(t, prefix, max)`.

Each mutation was applied to the live function / table / source, killed by the named test, then restored:

| Mutation | Killed by |
|---|---|
| parked_overdue without the gate filter | `TestLic27b_Low1_ClearedPersonIsNotParkedOrOverdue` |
| look-back fixed at 7 days | `TestLic27b_Low1_ThresholdLongerThanTheSevenDayWindowStillFires` |
| overdue ignores the gate anchor (run start only) | `TestLic27b_Low1_DeniedPersonWithNoDecisionRowIsOverdue` |
| replace stamps now when no request is named | `TestLic27b_Low2_QueueForAPersonWithNoRequestKeepsTheGateAge` |
| S-4 CHECK dropped | `TestLic27b_S4_ConfirmedRequestMustNameTheSanctionsList` |
| worker discriminator back to `tenant:<id>` | `TestLic18_TrappedPersonRaisesTheAlert` |
| 0188 with `IF NOT EXISTS` | `TestMigration0188_ParkedIndex_DefinedAndNeverSilentlyKept` |
| hook named in a trailing comment / `Store(true)` / `//go:generate` line | `TestLic24_Static_Pre0182HookIsIntegrationOnly` |

## 3. Residuals

1. `aml_screening_parked_overdue` scans at most 5000 persons per tenant per pass (as `aml_screening_gate_overdue` does); a larger tenant is partially observed.
2. In `aml_screening_parked_overdue` the fallback anchor for a person with no unresolved request is the first gating version, which can predate the person's registration (an over-estimate of their age, as in `aml_screening_gate_overdue`).
3. The sweeper tests still inject the overdue count through `testScreeningParked`; the SQL is tested with a real policy in `internal/aml`.
4. A dead worker AND a dead sweeper remains invisible; ALERT-DELIVERY-1 stays open.
