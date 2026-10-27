# ADR 0125: Per-tenant withdrawal restriction policy

Status: ACCEPTED as the engineering design (migrations 0182, 0184). Detail: `docs/plans/lic11-withdrawal-restriction-analysis.md`, `lic11-withdrawal-restriction-record.md`, `lic17-record.md`. Values are owner/legal decisions; none are shipped.

## Decision
1. Code-verified facts (supersedes the Q12 statement "withdrawals continue under suspension"): under tenant or brand suspension/closure new withdrawal requests and normal payouts already STOP (owner decisions 13-18, ADR 0095 s44, H-SEC-11, WD010/WD011, HR050); under a LICENCE suspension/expiry/not-yet-issued/missing, withdrawals continued only because no withdrawal path read the licence.
2. The four licence causes are now policy-driven per tenant: each cause x stage (request, T1p dispatch, T2/T12 redispatch) maps to `allow | hold_for_review | block`. Tenant/brand status is NOT configurable here (owner-decided). Market-closed is listed but not evaluable (no market determination at withdrawal; Q18/Q19).
3. Hard restrictions (AML/sanctions holds, closed or restricted accounts, kill switch, invalid instrument, KYC) are evaluated independently and cannot be overridden by any policy row.
4. Default: with no policy row the action is `allow` (the interim engineering default, labelled `default_unconfigured` in every gate decision and audit row and shown to operators as UNCONFIGURED). With policy rows, an unreadable licence fails closed. The licences evaluated are every licence the tenant relies on (own licence plus licences of approved in-force operating bases); strictest wins; a documented no-licence basis adds no licence cause.
5. Governance: tightening single-actor with reason code; loosening (including a first configured `allow` over the default) needs two distinct Persons (platform scope, signed actor proof ADR 0110), approvals <=24 h, requests expire after 72 h; the executor takes the tenant status key exclusive (ADR 0082 order); money paths take it shared; redispatch reads one policy version per tick.
6. `hold_for_review` means: accept the request, park at T1p, escalate at T2/T12; there is NO governed per-withdrawal release yet (OWNER DECISION required: releasing to dispatch would override the restriction; releasing to the player for an active tenant would change the owner-decided ADR 0111 eligibility). A parked count and age is exposed; the runbook default alert age N=72 h is an engineering default.

## Owner decisions needed
Per-cause values per tenant; the `hold_for_review` per-withdrawal release policy; whether tenant/brand status should become configurable (amendment question D-2 in the record).
