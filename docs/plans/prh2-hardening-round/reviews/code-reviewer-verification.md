# Code-reviewer verification — PRH-2 plan rev 2 (2026-09-28)

Reviewer: `code-reviewer`. Read-only. Plan at `d2cf040`; `git diff 559483a d2cf040 -- '*.go' '*.sql'` is empty, so the cited code is unchanged. This fulfils QA condition F6.

**Verdict: VERIFIED WITH CORRECTIONS.** Nearly every citation is confirmed, including every revision-2 correction: HR-9, `lockorder.go:355`, 20 sites in 10 files, `:989-1024`/`:1021`/`:1509`, `scheduler.go:333-341`, the absent non-test callers of `NewSweeper` and `ResolveLaunchToken`, the 0042 trigger, `kycgate.go` and the role map. The citation-by-citation table (roughly 55 rows: A1–L1 plus misc) is in the orchestrator's session record; the non-confirmed rows and dispositions are reproduced below.

## Citations not confirmed

| # | Plan claim | Result | Correct fact |
|---|---|---|---|
| C5 | Sync success posts `attempt.Amount` at `drive.go:260-264` | WRONG (location) | 260-264 classifies the outcome; the posting is at `drive.go:354`, the sync tombstone check at `:329`. |
| C7 | An empty Pending reference hits the 0099:136-137 CHECK | Imprecise | `MarkAccepted` (drive.go:294) first hits the `payment_attempts` CHECK at 0101:90-93, then 0099:136-137. The conclusion (untyped error, not a park) holds. |
| E2c | 20 call sites in 10 files | Gap | Add the direct `orch.resolveAmbiguous(...)` call at `migration_0107_integration_test.go:1774`: 21 sites of deleted functions. |
| E2d | "The legacy chain is the only path to `ErrDepositAlreadyPostedForIntent`" | **WRONG** | The live `postDepositSuccess` maps it at `orchestrator.go:1169`. `TestX5_LedgerBackstopMapping` (`:1510-1569`) reaches it through a real race, and a direct `ledger.Post` fixture already exists at `migration_0107_integration_test.go:344`. Ledger-finance should re-confirm the LF-16 parity scope. |
| F3 | `payout.go:285-323` records no decision | WRONG (partly) | The T1p deny path records one via `DenyForCompliance` → `withdrawal.go:1114`; only the allow path records nothing. §5-F already says so; §1-F does not. |
| H3 | H activates payout dispatch | Nuance | Payout sweeping runs only if `Sweeper.PayoutKYCGate` is set (`sweeper.go:84-89`, `payout_sweep.go:72`), and `NewSweeper` does not set it. |
| I4/I6 | Alert inventory | Incomplete | Missing: reconciliation `MISMATCH FOUND` P1s at `scheduler.go:420` (sportsbook), `:487` (casino consistency), `:562` (casino statement) and `:719` (payment statement), and `payment_deposit_simulation_handlers.go:240`. |
| K10 | A tenant caller can create only `tenant_admin` and `support` | WRONG (incomplete) | The allowlist at `admin_routes.go:491` also lets a tenant caller create `compliance`; only `finance`, `risk_manager`, `promotions_manager` and `bonus_operations` are platform-only. This strengthens S-1. |
| K14 | `licensing_model` evidence at `permission.go:348` | WRONG (weak) | That line is a comment. The attribute is real: `0001:13` (NOT NULL, CHECK in `('under_platform_licence','own_licence')`), kept consistent by 0007/0017, and read at `casino/orchestrator.go:728-741` and `bonus/eligibility.go:220`. No Go path UPDATEs it. Fitness for authorization is security's call. |
| E2a | Chain order | Nit | The call direction is `:674 → :555 → :700`. |

Additional sweep (not a correction): `HealthStatus` (payments `orchestrator.go:247`, casino `:590`) and `HandleCallback` (payments `:1319`, casino `:1040`, `kyc/provider.go:380`) are documented as in-memory or inbound-verification calls; not every adapter was checked against those contracts.

## Dispositions not carried, or carried inaccurately

| Finding | Problem |
|---|---|
| **S-1 item 2** (High) | The §5-K tests say sock-puppet cases are "documented as not structurally prevented unless (c)/(d)". That presumes K1 ships under (a)/(b) and softens security's condition "do not implement K1 on (a)/(b) without platform-minted grantees or co-approval". It also conflicts with QA W1 ("the sock-puppet case is refused"). |
| Header binding list (LF) | It omits LF-4, LF-5 and LF-7, which are HIGH and therefore binding. The body carries them. |
| S-12 | The beneficiary exclusion is applied to M2 only; the M1 attempt owner and the 0114 trigger are missing. |
| LF-16 | Carried, but on the false premise E2d. |
| LF-7(3) | Four reconciliation P1 sites are neither included nor excluded. |
| Payments F1 | Carried without the `PayoutKYCGate` nuance. |

Everything else was checked and is carried correctly. §7 takes no decision for the human; the interim fail-closed defaults are labelled as defaults.

## Required plan corrections

1. **E2 premise.** Delete the "only path" claim; cite `:1169`, `TestX5` and the fixture at `:344`; ask ledger-finance to re-confirm the LF-16 scope.
2. **E2 call sites.** Add the `resolveAmbiguous` site at `:1774`: 21 sites in 10 files.
3. **§1-F.** Only the T1p allow path records no decision; the deny path records via `withdrawal.go:1114`.
4. **§1-C.** Re-cite the sync posting as `drive.go:354` (tombstone `:329`), and the Pending empty-reference failure as 0101:90-93 via `MarkAccepted`, then 0099.
5. **S-1 fact.** In the §5-K inventory, the S-1 fact and HD-PRH2-2, add that a tenant caller can create `compliance` staff.
6. **HD-PRH2-6 evidence.** Use `0001:13`, 0007/0017 and the existing consumers; leave fitness to security.
7. **Alert inventory.** Add the reconciliation P1s at `scheduler.go:420,487,562,719` and `payment_deposit_simulation_handlers.go:240`, or exclude them with a reason.
8. **K1 sock-puppet condition.** Replace the "documented as not structurally prevented" wording with security's condition: an (a)/(b) answer without platform-minted grantees or co-approval sends K1 back to security; K1 does not proceed.
9. **Header binding set.** LF-1, -2, -3, -4, -5, -7, -9, -10, plus the LF-13 ruling (LF-11/14/15 optional).
10. **S-12.** Apply the beneficiary exclusion to the M1 attempt owner too, and name the trigger in the 0114 row.
11. **H.** State that H's wiring sets `Sweeper.PayoutKYCGate`.
12. **E2 chain order.** Fix it (cosmetic).
