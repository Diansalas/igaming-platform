> Stage 10.1 planning gate — specialist working paper (verbatim, recorded 2026-09-25 against 56f5135). Where it differs from the Orchestrator rulings in `docs/plans/stage-10.1-planning-gate-proposal.md` §O, the rulings govern.

# Review: ADR 0089 (Future AI Agent Architecture Boundary)

## Verdict: PASS — no changes required. Accurate, no bypass risk.

## Verification performed against HEAD

- `internal/bonus/suggestion.go`: `OriginatingKind` = `rule|model|manual`
  (matches "originating_kind=model"). `OriginatingModelVersion *string`
  field confirmed. Migration `migrations/0056_bonus_suggestions.up.sql`
  CHECK constraint confirms `originating_kind='model' AND
  originating_model_version IS NOT NULL` — ADR's description is accurate.
- `internal/auth/permission.go:267-272`: `PermBonusSuggestionCreate =
  "bonus_suggestion:create"`, comment states "held by no human role" and
  "deliberately absent from EVERY entry of rolePermissions" — matches
  ADR's claim this permission is granted to no role today (service-only,
  not yet wired to any non-human principal).
- `PermBonusReportRead` (permission.go:264) confirmed defined; grep of
  `internal/httpserver` for its usage in a route found none — matches
  ADR §4.1 "defined, but no route uses it."
- Routes: `POST /v1/admin/bonus/suggestions/{suggestionID}/claim` and
  `.../decide` both exist (`internal/httpserver/bonus_routes.go:40,42`).
  `POST /v1/admin/bonus/change-requests` and
  `/{id}/decide` confirmed via `change_governance.go`-backed integration
  tests. Both match ADR §3 "Approval workflows" row.
- `SuggestionReviewEvent` (append-only) confirmed in `suggestion.go:242+`.
- `ActivateSuggestionAsSingleGrant` confirmed in
  `internal/bonus/suggestion_lifecycle.go`.
- Doc 10 §N3.3 quote ("a future AI/CRM/recommendation system ... can only
  ever populate Generated rows; it structurally cannot move money") is a
  faithful paraphrase of the actual text at lines 5108-5111 ("such a
  system can only ever populate `Generated` rows; it structurally cannot
  move money, no matter how it is built, because it is never issued the
  credential that could").
- Docs 17, 21, 23, 30 all carry explicit `NOT IMPLEMENTED` /
  "architecture-freeze proposal, no code" status headers at HEAD —
  consistent with ADR's "Missing" labels in §4.1.
- No `RequireStepUp` function/middleware exists for bonus
  suggestion/change-request approvers (only an unrelated withdrawal
  policy JSON field and `withdrawal.ErrStepUpRequired`) — ADR's "NOT
  IMPLEMENTED" label for approver step-up is correct and not overstated.

## Governance/liability safety check

- ADR never grants an agent any of `bonus_grant:issue`,
  `bonus_bulk:execute`, `bonus_adjustment:write`,
  `bonus_held_disposition:resolve`, `bonus_campaign:activate`,
  `bonus_approval_policy:write`, or `bonus_suggestion:create` itself —
  it explicitly keeps `bonus_suggestion:create` un-grantable to any role
  today and treats granting it to an agent as future, security-reviewed
  work, not decided here.
- §5.3 correctly extends maker-checker: agent proposal on behalf of
  staff S cannot be approved by S; matches existing four-eyes
  self-approval refusal in `four_eyes_ops.go`.
- §2.2/§9 (I-1..I-6) correctly keep ledger, wallet, RG, risk, RBAC
  authoritative outside any agent path; ADR 0019 originator-matrix
  claim ("agent is not a new actor class for ledger purposes") is
  consistent with the existing suggestion object having zero write path
  to ledger/wallet (verified: no `internal/ledger`/`internal/wallet`
  import in `suggestion.go` or `suggestion_lifecycle.go`).
- No code, migration, route, or permission is added by the ADR itself
  (confirmed: diff-free against `internal/bonus`, `internal/auth`,
  `migrations/`).

## Findings requiring text changes

None. No inaccuracy, no misstatement of wagering/eligibility/liability
semantics, and no path by which the ADR's design would let an agent
become authoritative for bonus liability or bypass four-eyes/change
governance was found.
