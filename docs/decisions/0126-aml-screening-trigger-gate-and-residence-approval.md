# ADR 0126: AML screening triggers (MOCK), withdrawal screening gate, residence maker-checker, KYC policy signed writes

Status: ACCEPTED as the engineering design (migrations 0181, 0185). Detail: `docs/plans/lic10-record.md`, `lic18-record.md`, `lic19-record.md`. All screening is MOCK; no real vendor.

## Decision
1. Screening triggers (registration, kyc_approval, scheduled) are governed per tenant and jurisdiction (officer, quorum, signed proof, legal review reference). A durable queue (unique (tenant, idempotency key), state machine, lease CAS, retry, terminal failure) feeds a worker; the worker never clears a match and never places a hold.
2. The withdrawal/payout screening gate (`gate_withdrawal`) holds without a fresh completed clear, with an unresolved match or after terminal failure; deposits/stakes are never gated. It can only be enabled while a worker heartbeat exists and requires the `scheduled` event, so it cannot be enabled in production (the worker is not started there) and clears cannot go stale unattended. New policy versions neither reopen the grace nor discard clears; unresolved matches keep holding when the policy changes. A merely outstanding screening PARKS the payout (approved, funds hold kept); real officer holds and KYC failures still reject. A trapped-funds alert (`aml.screening_gate_trapped`, p1) fires after a configured age. An officer cannot clear a sanctions-class match alone. Officers resolve requests through governed HTTP routes.
3. Residence maker-checker: per-tenant mode `none | into_open_market | all`; stricter = one actor immediately, looser = two distinct Persons; default none (today's behaviour). A determination that needs approval and has none is not market evidence (fail closed). The country is snapshotted on the check row; direct residence UPDATEs are guarded.
4. KYC enforcement policy writes (create, activate, withdraw, supersede) carry a signed proof (`kyc_enforcement_policy:*`) and a database Person-distinct author/activator check; accounts without a Person link are refused.
5. Pre-real-provider prerequisites (hard): separate DB principal or ADR 0116 F1 system actor for the worker, revoke claim/finish from the runtime role, daily vendor reconciliation, an alert route (ALERT-DELIVERY-1).

## Owner decisions needed
Grace and re-screen interval values; whether the gate may ever be enabled before a contracted provider exists; resolution policy for sanctions false positives; KYC threshold values; residence mode per tenant.
