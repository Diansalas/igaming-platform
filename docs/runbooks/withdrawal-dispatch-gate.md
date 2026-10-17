# Runbook — withdrawal gate at payout dispatch (F4, migration 0169)

Status: IMPLEMENTED against MOCK providers. Detail and review: `docs/plans/f4-withdrawal-gate-backstop-record.md` (sections 2, 11, 14).

## What it does
The withdrawal gate (WD050/WD051 and the RG limits/self-exclusion rules) is evaluated at request time **and again** inside the
transaction that dispatches the payout: first dispatch (`approved` -> `submitted`, stage `dispatch`) and every re-dispatch of a
payout attempt (T2/T12, stage `redispatch`). Each evaluation appends a row to `withdrawal_gate_decisions` (append-only). The
database refuses the transition unless an allowing row was written in the same transaction (WD053) or the account/wallet is not
`active` (WD052).

## Operator symptoms and actions
| Symptom | Meaning | Action |
|---|---|---|
| `POST .../submit` returns 409 `conflict` (generic) | gate denied at dispatch; request stays `approved` with its hold; alert `withdrawal.payout_gate_denied` | Look up the denying row in `withdrawal_gate_decisions` (gate code, reason). Resolve the cause (e.g. RG restriction, KYC) then re-submit, or reject the request (releases the hold). |
| `POST .../submit` returns 503 | gate unavailable; nothing written; fails closed | Retry after checking platform status (`GET /v1/admin/platform/status`) and the application logs. |
| Alert on a payout attempt `payout_attempt:<id>:gate:<code>` | re-dispatch denied; attempt escalated, no resend | Resolve the cause. **Note (SF4-3):** escalation is not a human-release latch: once the cause clears (e.g. account reactivated) the next sweeper tick re-runs the gate and may resend. If a compliance hold must persist, use the payment kill switch / force-resolve flow instead of relying on the escalation. |
| SQLSTATE WD052/WD053/WD054 in logs | a code path attempted to dispatch without a recorded allow decision | Treat as a defect or an attack; the transition was refused. Raise a P2 and preserve logs. |
| Gate keeps failing at T2/T12 | fails closed; currently log-only (SF4-5) | Check logs for the gate error; no alert exists for this case yet. |

## Known residuals (launch risk list)
- SF4-1 / R1: a runtime-role credential with arbitrary SQL can insert a forged allow row. Owner decision before real money: signer outside the runtime's reach (ADR 0110) or accept plus a scheduled re-evaluation job that alerts on divergence.
- F4-R2-NEGBAL-SQL: the per-wallet negative-balance block is enforced in the application gate only, not in SQL.
- R3: status race window between evaluation and transition is covered by the SQL WD052 re-read, not by a lock.
- Rollback of migration 0169 is refused (WD095) while decision rows exist.
