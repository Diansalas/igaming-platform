# Runbook — withdrawal gate at payout dispatch (F4, migrations 0169 and 0171)

Status: IMPLEMENTED against MOCK providers. Detail and review: `docs/plans/f4-withdrawal-gate-backstop-record.md` (sections 2, 11, 14).

## What it does
The withdrawal gate (WD050/WD051 and the RG limits/self-exclusion rules) is evaluated at request time **and again** inside the
transaction that dispatches the payout: first dispatch (`approved` -> `submitted`, stage `dispatch`) and every re-dispatch of a
payout attempt (T2/T12, stage `redispatch`). Each evaluation appends a row to `withdrawal_gate_decisions` (append-only). The
database refuses the transition unless an allowing row was written in the same transaction (WD053) or the account/wallet is not
`active` (WD052).
Since 0171 an allowing row is a **signed system attestation** (ADR 0110 actor proof, scope `system`, operation
`withdrawal_dispatch_gate:allow`, bound to tenant, request, attempt, stage, outcome and the request figures, nonce consumed in the
same transaction): the database refuses an allow row without a valid proof (AP001..AP005), so a stolen runtime credential cannot
forge one. The signing key is the existing `ACTOR_PROOF_KEYS` / `ACTOR_PROOF_ACTIVE_KID`; **no new configuration**.

## Operator symptoms and actions
| Symptom | Meaning | Action |
|---|---|---|
| `POST .../submit` returns 409 `conflict` (generic) | gate denied at dispatch; request stays `approved` with its hold; alert `withdrawal.payout_gate_denied` | Look up the denying row in `withdrawal_gate_decisions` (gate code, reason). Resolve the cause (e.g. RG restriction, KYC) then re-submit, or reject the request (releases the hold). |
| `POST .../submit` returns 409 "held under the tenant's withdrawal restriction policy"; denying row with `gate_code` WD056 | lic11 (migration 0182): the tenant's CONFIGURED withdrawal restriction policy holds / blocks the payout (licence state); request stays `approved` with its hold | See `withdrawal-restriction-policy.md`. |
| `POST .../submit` returns 503 | gate unavailable; nothing written; fails closed | Retry after checking platform status (`GET /v1/admin/platform/status`) and the application logs. |
| Alert on a payout attempt `payout_attempt:<id>:gate:<code>` | re-dispatch denied; attempt escalated, no resend | Resolve the cause. **Note (SF4-3):** escalation is not a human-release latch: once the cause clears (e.g. account reactivated) the next sweeper tick re-runs the gate and may resend. If a compliance hold must persist, use the payment kill switch / force-resolve flow instead of relying on the escalation. |
| SQLSTATE WD052/WD053/WD054 in logs | a code path attempted to dispatch without a recorded allow decision | Treat as a defect or an attack; the transition was refused. Raise a P2 and preserve logs. |
| Alert `withdrawal.payout_gate_unavailable` (p2; subject `withdrawal:<id>` at dispatch, `payout_attempt:<id>` after 3 consecutive failed ticks at T2/T12; attributes `stage`, `cause` = `evaluation_failed` or `proof_unavailable`) | the gate (or the signer) could not decide; fail closed, payout stalled, nothing sent | `evaluation_failed`: check the risk/RG evaluation and application logs. `proof_unavailable`: the actor-proof key is not configured or not an active kid in the database (runbook operational-runbooks section 15); platform-api also refuses to start in production without it. Delivery of p2 alerts to a person is ALERT-DELIVERY-1 (open). |
| SQLSTATE AP001..AP005 on `withdrawal_gate_decisions` in logs | an allow row without a valid, fresh, unused, correctly bound proof (a forgery attempt, a key rotation gap or clock skew) | If not explained by a deploy/rotation, treat as an attack: preserve logs, rotate the actor-proof key (section 15). |

## Known residuals (launch risk list)
- SF4-1 / R1 (narrowed by 0171): arbitrary SQL with a stolen runtime credential can no longer forge an allow. Remaining (ADR 0110 T6): an attacker who holds the application's signing key can sign an allow; the proof attests that the application evaluated the gate, not that the evaluation was correct (inputs governance, SF4-6, unchanged).
- F4-R2-NEGBAL-SQL: still application-gate only (the SQL clause was built and withdrawn: it needs `wdpath.Plant` fixtures to be funded; see `docs/plans/f4r1-record.md` section 5). A forged allow can no longer bypass the application check.
- SF4-3 (documented behaviour, not changed): a T2/T12 escalation is not sticky.
- R3: status race window between evaluation and transition is covered by the SQL WD052 re-read, not by a lock.
- Rollback of migration 0169 is refused (WD095) while decision rows exist; rollback of 0171 while proof-bound rows or alerts of the new Kind exist.
