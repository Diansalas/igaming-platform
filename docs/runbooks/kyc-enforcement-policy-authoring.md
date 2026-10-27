# KYC enforcement policy authoring (lic10, L-20)

- **Scope:** `internal/kyc/enforcement_admin.go`, `enforcement_policy_admin.go`, `internal/httpserver/kyc_enforcement_policy_handlers.go`; ADR 0096; migrations 0100 and 0103 (unchanged).
- **Status:** IMPLEMENTED (API). Back Office is NOT IMPLEMENTED. **The platform ships NO policy and NO threshold.** With no active policy KYC enforcement is dormant for the jurisdiction ("not required"); the launch readiness item that flags a brand with zero policies is unchanged.
- **Not a legal decision.** Every threshold, asset and operation comes from a human compliance decision, with a legal review reference.

## 1. Routes (platform_admin only, step-up critical on every write)

| Route | Effect |
|---|---|
| `GET /v1/admin/kyc/enforcement-policies[?licensing_jurisdiction_id=&status=]` | list (at most 500, newest first) |
| `POST /v1/admin/kyc/enforcement-policies` | create a DRAFT. `cumulative_deposit` needs `threshold_minor_units` (positive integer string, minor units) and `asset_code`; `play` needs `play_operation` (`casino_play` or `sportsbook_play`). `legal_review_reference` and `reason_code` required. |
| `POST .../{policyID}/activate` | draft to active. **Four-eyes, enforced by the database trigger:** the activating platform administrator must differ from the author. |
| `POST .../{policyID}/withdraw` | withdraw a never-activated draft |
| `POST .../{policyID}/supersede` `{successor_policy_id}` | atomically replace an ACTIVE policy by a DRAFT of the same enforcement key; the acting administrator must differ from BOTH authors |

- `edd_amount` and `registration_tier` are not accepted: the evaluator does not consult them and the database refuses to activate them (NOT IMPLEMENTED).
- There is no way to switch an active policy off without a successor (migration 0103, security F3): enforcement is never silently disabled. "Deactivate" an active policy by superseding it.
- At most one active policy per enforcement key (jurisdiction, trigger type, play operation, asset); a second activation is 409.
- Every write is audited (`kyc_enforcement_policy.created | activated | withdrawn`) with the reason code, IP, user agent and request id.

## 2. Procedure

Administrator A creates the draft; administrator B activates it. To change a value: A creates the replacement draft; a third administrator C supersedes (C must differ from the authors of both rows). Three distinct platform administrators are therefore needed to replace an active policy.

## 3. Residuals

Back Office pages; a readiness item per jurisdiction for "policy present"; the evaluator-wiring of `edd_amount` and `registration_tier`; per-asset aggregation (KYC-FX-AGG-1).
