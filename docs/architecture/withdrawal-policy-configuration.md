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
most recently effective row (`effective_from DESC, policy_version DESC`)
among ties. `asset_code` is never a wildcard: a policy row always
belongs to exactly one asset, which is what makes the precision bug
structurally impossible once any row exists — `Approve`/`Reject` can no
longer compare a request's amount against a threshold that was never
meant for that asset.

`internal/withdrawal.Approve`/`Reject` no longer take a threshold
parameter. They resolve their own `ApprovalPolicy` internally (using the
locked `WithdrawalRequest`'s own `tenant_id`/`brand_id`/`asset_code`)
immediately after locking the row, inside the same transaction as the
decision itself — a caller can never apply one asset's or one tenant's
policy to a different asset's or tenant's withdrawal.

## 3. The zero-config fallback — explicitly `NOT` a production policy

When no `withdrawal_policies` row matches, `defaultApprovalPolicy`
derives a threshold from the asset's own `decimal_exponent` (the same
Asset registry the ledger itself reads): **1000 major units of THIS
asset**, not the flat, asset-blind Stage 3B constant.

| Asset | Exponent | Default threshold (minor units) |
|---|---|---|
| EUR / USD / GBP / BRL / MXN | 2 | 100,000 (= 1,000.00) |
| USDT | 6 | 1,000,000,000 (= 1,000.000000) |
| BTC | 8 | 100,000,000,000 (= 1,000.00000000) |

This is a **test/development stand-in**, not a business decision — see
§5's open decisions. `RequiredApprovals` defaults to `2` (Stage 3B's
original "two distinct human approvers above threshold" rule, carried
forward unchanged) and `RequireStepUp` always defaults to `false` (§7).

If the multiplication needed to compute a default would overflow
`int64` (possible for an asset with a very high `decimal_exponent`, up
to 18 per migration `0003`'s own constraint — no asset seeded in this
platform today has one that high), the fallback returns a threshold of
`0` rather than a silently wrong number: every non-zero withdrawal in
that hypothetical asset would require the full `RequiredApprovals`,
erring toward stricter approval, never looser.

## 4. Adversarial test coverage

`internal/withdrawal/policy_integration_test.go` (real PostgreSQL 16):

- `TestResolveApprovalPolicy_DefaultFallbackIsAssetPrecisionAware` —
  the same tenant resolves a different, asset-derived threshold per
  asset with no configuration at all.
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
   decided.** §3's default is a placeholder chosen for continuity with
   Stage 3B's test value, not a risk/compliance/business decision.
   Nothing in this stage should be read as proposing EUR 1,000 (or its
   asset-scaled equivalents) as the actual production policy.
2. **Jurisdiction-scoped policy rows cannot be selected today.** No
   per-player/per-withdrawal jurisdiction assignment exists in the
   platform (a `player_account` belongs to a `brand`; a `tenant`, not a
   brand, resolves to a set of jurisdictions via
   `tenant_jurisdiction_configs`, and can serve several at once — see
   `15-jurisdiction-and-licensing-model.md`). A `jurisdiction_code` row
   can be written but will never match until that assignment exists.
   This is a schema-readiness decision, not a functional gap in this
   stage's own scope.
3. **Whether policy-config-edit and withdrawal-approval permissions must
   be held by disjoint roles remains open** — unchanged from Stage 3B
   (`withdrawal-state-machine.md` §5 bypass #3): a staff member who can
   both edit `withdrawal_policies` and approve withdrawals could still
   lower a threshold immediately before approving. `threshold_amount_at_
   decision` on each `withdrawal_approvals` row makes such a change
   detectable after the fact; it does not prevent it.
4. **No admin API exists yet to write `withdrawal_policies` rows.** This
   stage only builds the resolution boundary and the table; a
   partner-console/back-office CRUD surface for tenant/brand staff to
   configure their own policy is future work (would also need to decide
   #3 first).

## 6. Approver role requirements — schema-only, `NOT IMPLEMENTED`

`required_approver_roles TEXT[]` exists so a future policy can require,
e.g., "at least one approval from `tenant_admin`, not just `finance`".
`internal/withdrawal.Approve` does not read or enforce this column today
— doing so would require passing the approver's own role into `Approve`
(currently it only takes a principal id) and deciding how role
requirements interact with `RequiredApprovals` (e.g., "2 approvals, at
least 1 of which must be `tenant_admin`" vs. "2 approvals, each from a
distinct required role"). Left as an explicit open decision rather than
guessed at, per CLAUDE.md's "no uncontrolled scope expansion."

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
