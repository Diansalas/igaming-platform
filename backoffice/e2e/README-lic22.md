# Specs 09 and 10: withdrawal restriction policy and AML screening (lic22)

Added on top of the lic13 harness (`README.md`); same stack, same fresh private database per run, same rules (synthetic data only, no mocked API).
Run exactly as in `README.md`; `E2E_SPEC=09` or `E2E_SPEC=10` runs one of them. Record: `docs/plans/lic22-e2e-record.md`.

## What is new in the stack

| Piece | Where | Why |
|---|---|---|
| `setup/wd_aml_env.sh` (sourced by `run.sh` with `set -a`, one added line) | API environment | `TEST_SUPPORT_ENDPOINTS_ENABLED=true` and `MOCK_GEOLOCATION_COUNTRY=MT` (MOCK PSP callback, MOCK KYC, MOCK e-mail seam, market gate evidence; non-production `APP_ENV=development`), run-time generated `PAYOUT_INSTRUMENT_*` key families (withdrawals need a payout instrument), and the runtime-role URL for the below-the-app-layer attempts |
| `lib/wd_world.ts` | helpers | one extra synthetic ACTIVE tenant per scenario (`cmd/seed-staging-world`, enrolled over the real HTTP flow), tenant payments capability (tenant_admin PUT), funded players (register, e-mail confirm, deposit + MOCK PSP simulate-callback, MOCK KYC + compliance review, payout instrument), withdrawals, finance approvals |
| `lib/wd_db.ts` | ground truth | owner-role reads with ONE scope (tenant-owned tables are FORCE RLS; tenant + platform principal together sees nothing), ledger and projection invariants, runtime-role SQL for refusals |
| `lib/wd_ui.ts`, `lib/wd_aml.ts` | UI drivers | the policy screens and `/aml/screening` |
| `tools/aml-fixture` (Go, build tag `e2efixture`) | test support | authors the jurisdiction SCREENING policy through the real AML officer service, because no HTTP route exists for it |

The MOCK screening worker is not started by the harness: `platform-api` starts it itself outside production and it reports its heartbeat every 10 s.
Owner-role writes (documented fixture path of the governed-write guard, private database): licence rows, re-pointing `tenants.licence_id`,
the MOCK marker on a verification's vendor subject reference (`SYNTH-SANCTION-` / `SYNTH-PEP-`, the only way the MOCK provider is told to answer a match).

## No-heartbeat window

`withoutHeartbeat()` (lib/wd_aml.ts) deletes the heartbeat rows and then holds `SHARE ROW EXCLUSIVE` on `screening_worker_heartbeats` in an open transaction
so the real worker's next heartbeat waits and the authoring check really finds none. `release()` commits and the worker reports in again. Nothing is stubbed.
