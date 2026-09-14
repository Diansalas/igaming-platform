# Withdrawal Approval Policy Configuration

Status: `IMPLEMENTED` (the configuration boundary and its zero-config
fallback) / `NOT IMPLEMENTED` (a real, business-approved policy; MFA/
step-up itself). Built in Stage 3C (Financial Hardening) per that stage's
directive item 5 ("withdrawal policy configuration") and item 6 ("MFA/
step-up boundary"). Owner: `payments`, with `ledger-finance` on
asset-precision correctness and `security` on the step-up boundary
(ADR 0017).

## 1. What this replaces

Stage 3B's four-eyes rule (`withdrawal-state-machine.md` §5) compared a
withdrawal's raw minor-unit amount against a single process-wide Go
constant (`httpserver.defaultWithdrawalApprovalThreshold = 100000`,
removed in Stage 3C), applied identically to every tenant, brand, and
asset. That constant was silently asset-blind: `100000` means EUR
1,000.00 (2-decimal exponent) but 0.001 BTC (8-decimal exponent) or
1,000,000 USDT (6-decimal exponent) — comparing the same raw number
across assets compares different real-world magnitudes, which is exactly
the imprecision CLAUDE.md's ledger rules ("never use floating-point...
per-currency exponent looked up from the Asset registry") exist to
prevent, one layer up from the ledger itself.

## 2. The configuration boundary — `IMPLEMENTED`

`internal/withdrawal/policy.go` defines:

- `ApprovalPolicy{ThresholdAmount, RequiredApprovals, RequireStepUp}` —
  the resolved rule set for one decision.
- `ResolveApprovalPolicy(ctx, tx, tenantID, brandID, jurisdictionCode,
  assetCode, at)` — reads the `withdrawal_policies` table (migration
  `0032`) and returns the most specific matching row, or an explicit
  fallback (§3) when none matches.

`withdrawal_policies` columns and how they map to the directive's listed
policy dimensions:

| Dimension | Column | Status |
|---|---|---|
| Tenant | `tenant_id` (required, RLS-scoped) | `IMPLEMENTED` |
| Brand | `brand_id` (nullable = any brand) | `IMPLEMENTED` |
| Jurisdiction | `jurisdiction_code` (nullable = any jurisdiction) | schema `IMPLEMENTED`, selection `NOT IMPLEMENTED` — see §5 |
| Asset | `asset_code` (required — never nullable) | `IMPLEMENTED` |
| Withdrawal amount | `approval_threshold_minor_units` | `IMPLEMENTED` |
| Required approvers | `required_approvals` | `IMPLEMENTED` |
| Approver role requirements | `required_approver_roles TEXT[]` | schema `IMPLEMENTED`, enforcement `NOT IMPLEMENTED` — see §6 |
| Step-up/MFA requirement | `require_step_up` | `IMPLEMENTED` (enforcement boundary; MFA itself `NOT IMPLEMENTED`) — see §7 |
| Policy version/effective time | `policy_version`, `effective_from` | `IMPLEMENTED` |

Selection (`ResolveApprovalPolicy`'s query) prefers the most specific
matching row — an exact `brand_id` match over a tenant-wide `NULL`, then
an exact `jurisdiction_code` match over a tenant-wide `NULL` — then the
most recently effective row (`effective_from DESC, policy_version DESC`,
then `created_at DESC, id DESC` as a final deterministic tiebreaker,
added after specialist review found two rows tying on every other key
made the outcome unspecified) among ties. `asset_code` is never a
wildcard: a policy row always belongs to exactly one asset, which is
what makes the precision bug structurally impossible once any row
exists — `Approve`/`Reject` can no longer compare a request's amount
against a threshold that was never meant for that asset. `brand_id`
uses a composite `(brand_id, tenant_id)` FK against `brands(id,
tenant_id)` (migration `0033`, matching every sibling table) rather than
a single-column FK, so a policy row can never silently name a brand
belonging to a different tenant.

Note: the distinct-human-approver COUNT itself (a separate mechanism
from policy resolution) dedupes by `staff_users.person_id` when linked,
not just by `approver_principal_id` — see ADR 0023 §1 for why (a
specialist-review finding: two staff logins sharing a person could
otherwise supply both required approvals for someone else's withdrawal).

`internal/withdrawal.Approve`/`Reject` no longer take a threshold
parameter. They resolve their own `ApprovalPolicy` internally (using the
locked `WithdrawalRequest`'s own `tenant_id`/`brand_id`/`asset_code`)
immediately after locking the row, inside the same transaction as the
decision itself — a caller can never apply one asset's or one tenant's
policy to a different asset's or tenant's withdrawal.

## 3. The zero-config fallback — fails closed, explicitly `NOT` a production policy

**Revised after specialist review.** The original design derived a
default threshold from the asset's own `decimal_exponent` alone — "1000
major units of THIS asset" — intending that to represent a comparable
real-world magnitude across assets. `ledger-finance` rejected this on
sign-off: decimal precision (how many digits an asset uses) says nothing
about that asset's actual market value, so "1000 major units" of EUR and
"1000 major units" of BTC are not comparable amounts. For BTC
specifically (`decimal_exponent = 8`) that design computed a default of
100,000,000,000 minor units — **1000 BTC** — which would have raised the
effective four-eyes bar to roughly 1000 BTC per decision, a large
WEAKENING of protection relative to even the flat Stage 3B constant it
replaced. Building a genuinely value-equivalent default across assets
requires FX/market-price data, which is explicitly out of this stage's
scope (no FX/conversion accounting).

The fallback (`defaultApprovalPolicy`) now fails closed instead:
**`ThresholdAmount` is always `0`**, for every asset, when no
`withdrawal_policies` row matches — so any non-zero withdrawal in an
unconfigured asset requires the full `RequiredApprovals` (defaults to
`2`, Stage 3B's original "two distinct human approvers" rule carried
forward). `RequireStepUp` always defaults to `false` (§7 — MFA doesn't
exist yet, so the fallback must never default to a policy this platform
cannot enforce). This is a deliberately maximally-conservative
**test/development stand-in**, not a business decision — see §5's open
decisions. A tenant that wants a lighter-touch, asset-appropriate
threshold must configure one explicitly via a `withdrawal_policies` row.

## 4. Adversarial test coverage

`internal/withdrawal/policy_integration_test.go` (real PostgreSQL 16):

- `TestResolveApprovalPolicy_DefaultFallbackFailsClosedForEveryAsset` —
  the same tenant resolves ThresholdAmount 0 for every asset with no
  configuration at all (revised after the §3 fail-closed redesign).
- `TestResolveApprovalPolicy_ConfiguredPolicyNeverBleedsAcrossAssets` —
  two explicit policy rows for the same tenant (EUR, BTC) never leak
  into each other or into a third, unconfigured asset (USD).
- `TestResolveApprovalPolicy_UnknownAssetFailsClosed` — an asset absent
  from both `withdrawal_policies` and the Asset registry is refused, not
  guessed at.
- `TestApprove_RequireStepUpFailsClosedWhenAboveThreshold` — see §7.

`internal/withdrawal/adversarial_test.go`'s
`TestApprove_ThresholdAmountRecordedPerDecisionAndCurrentThresholdGovernsReadiness`
proves a later policy change (a second, later-`effective_from` row)
between two decisions on the same request is picked up per-call, never
memoized — carrying forward Stage 3B's identical guarantee under the new
resolution mechanism.

## 5. Remaining open business decisions

1. **The real approval threshold(s), per tenant/brand/asset, are not
   decided.** §3's default (fail closed, threshold 0) is a deliberately
   conservative placeholder, not a risk/compliance/business decision.
   Nothing in this stage should be read as proposing any specific
   production threshold. Stage 3D's admin API (§8) lets a tenant
   configure one; it does not decide what value to configure.
2. **Jurisdiction-scoped policy rows cannot be selected today, and are
   now schema-blocked from being written.** No per-player/per-withdrawal
   jurisdiction assignment exists in the platform (a `player_account`
   belongs to a `brand`; a `tenant`, not a brand, resolves to a set of
   jurisdictions via `tenant_jurisdiction_configs`, and can serve several
   at once — see `15-jurisdiction-and-licensing-model.md`). Migration
   `0033` added `CHECK (jurisdiction_code IS NULL)` after specialist
   review flagged the original free-`TEXT`, no-FK column as a
   false-sense-of-enforcement risk (a compliance officer could configure
   a jurisdiction-scoped policy that silently never matched anything).
   This remains a schema-readiness decision for a future stage, now
   structurally enforced rather than merely documented. Stage 3D's admin
   API (§8) never accepts a client-supplied `jurisdiction_code` for the
   identical reason - it always inserts `NULL`.
3. **`RESOLVED` (Stage 3D, `docs/decisions/0024` §4).** Policy-config-edit
   and withdrawal-approval permissions are now held by disjoint roles:
   `PermWithdrawalPolicyWrite` belongs only to `RoleTenantAdmin`;
   `PermWithdrawalReview/Approve/Reject/Submit` belong only to
   `RoleFinance`. No role holds both, closing `withdrawal-state-
   machine.md` §5 bypass #3 structurally, not merely via the
   after-the-fact `threshold_amount_at_decision` audit trail.
4. **`RESOLVED` (Stage 3D).** A minimal admin API now exists:
   `GET`/`POST`/`DELETE /v1/admin/withdrawal-policies`
   (`internal/httpserver/withdrawal_policy_handlers.go`), gated by
   `PermWithdrawalPolicyWrite` and scoped to the caller's own tenant via
   RLS. `POST` only ever INSERTs a new row (the table remains an
   append-only history of policy-in-force-over-time, matching every
   other financial-configuration table in this codebase - see that
   file's own doc comment for why); `DELETE` removes a misconfigured row,
   which is safe precisely because `threshold_amount_at_decision`/
   `request_amount_at_decision` already snapshot the policy onto each
   `withdrawal_approvals` row at decision time, so removing a policy row
   can only change what a FUTURE decision resolves to, never rewrite a
   past one. This is deliberately the minimal boundary the Stage 3D
   directive asked for - no partner-console/back-office UI, no bulk
   import, no policy versioning/rollback UI - a real operator-facing
   surface for tenant/brand staff to manage their own policy remains
   future work.

## 8. Admin API — `IMPLEMENTED` (Stage 3D, minimal boundary only)

See §5 items 3-4 above for the business-decision context. Request/
response shapes:

- `POST /v1/admin/withdrawal-policies` — body:
  `{asset_code, approval_threshold_minor_units, required_approvals,
  require_step_up?, brand_id?, policy_version?, effective_from?}`.
  `brand_id` omitted/empty applies to every brand under the tenant;
  supplying one is validated by the composite `(brand_id, tenant_id)` FK
  (migration `0033`), so a brand belonging to a different tenant is
  refused as an invalid reference, not silently accepted as a dead row.
  `effective_from` defaults to now and may be a future timestamp (a
  scheduled policy change); `policy_version` defaults to `1`. Every
  create is audited (`withdrawal_policy.created`).
- `GET /v1/admin/withdrawal-policies` — lists every policy row for the
  caller's own tenant (RLS-scoped), most specific/most recent first.
- `DELETE /v1/admin/withdrawal-policies/{id}` — removes one row (audited
  as `withdrawal_policy.deleted`); RLS scopes the id lookup to the
  caller's own tenant, so naming another tenant's row id is a 404, not a
  403 (no confirmation that the id even exists elsewhere is ever leaked).

Both routes require `RequireTenantScope` (excludes `platform_admin`'s
nil-tenant token, matching every other tenant-scoped admin route) and
`PermWithdrawalPolicyWrite`.

## 6. Approver role requirements — schema-only, `NOT IMPLEMENTED`

`required_approver_roles TEXT[]` exists so a future policy can require,
e.g., "at least one approval from `tenant_admin`, not just `finance`".
`internal/withdrawal.Approve` does not read or enforce this column today
— doing so would require passing the approver's own role into `Approve`
(currently it only takes a principal id) and deciding how role
requirements interact with `RequiredApprovals` (e.g., "2 approvals, at
least 1 of which must be `tenant_admin`" vs. "2 approvals, each from a
distinct required role"). Left as an explicit open decision rather than
guessed at, per CLAUDE.md's "no uncontrolled scope expansion." Migration
`0033` added `CHECK (required_approver_roles IS NULL)` after specialist
review flagged the same false-sense-of-enforcement risk as
`jurisdiction_code` above: a value could be written and silently ignored
by every current caller.

## 7. Step-up/MFA enforcement boundary — `IMPLEMENTED` (boundary only)

MFA/step-up authentication itself is **`NOT IMPLEMENTED`** — ADR 0017
(`docs/decisions/`) is unchanged by this stage and remains the
authoritative record of that open decision. What Stage 3C adds is the
enforcement *point*: `ApprovalPolicy.RequireStepUp`, checked by
`internal/withdrawal.Approve` immediately after computing whether a
decision is above the resolved threshold.

- Below threshold: `RequireStepUp` has no effect — a normal single
  approval succeeds exactly as it would if `RequireStepUp` were `false`.
- At or above threshold, `RequireStepUp = true`: `Approve` returns
  `ErrStepUpRequired` (mapped by
  `internal/httpserver/withdrawal_handlers.go` to `403 forbidden`)
  **before recording anything** — no `withdrawal_approvals` row, no
  state change, no ledger effect. The refusal is total and repeatable;
  it never partially applies a decision.

This means a tenant can safely configure `require_step_up = true` on a
policy row today, before MFA ships, and the platform will refuse the
above-threshold path outright rather than silently accepting an
unauthenticated-by-MFA approval. **Once MFA is implemented, the
enforcement point does not move**: the exact place `Approve` currently
returns `ErrStepUpRequired` is where a real step-up challenge/verification
call should be inserted instead (see `internal/withdrawal/policy.go`'s
`ErrStepUpRequired` doc comment and `withdrawal.go`'s `Approve` doc
comment for the precise location). No threshold or challenge mechanism
is invented here — only the boundary.
