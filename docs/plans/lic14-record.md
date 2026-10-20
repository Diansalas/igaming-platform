# lic14: operating-basis hardening (response to the independent security review of lic9)

Branch `lic14-basis-hardening` (from `integ-a` 243fc90 = lic9 + lic10; head 0181; **0182 is reserved for lic11, this slice is migration 0183**).
Software capability only; no value shipped; no legal position.

## 1. Status

| Finding | Fix | Label |
|---|---|---|
| H-1 (launch-blocking) KYC / RG / payout freshness default-allowed when only a basis opened the path | rulebook jurisdiction selected by the governing basis; fail closed when it cannot be selected | IMPLEMENTED for KYC (deposit, casino play, sportsbook play); PARTIALLY IMPLEMENTED for RG and payout freshness (see 3) |
| M-1 owner / single actor could create permission by closing a masking or restricting row | owner close refused (LC041); deferred constraint trigger requires a successor (LC044) | IMPLEMENTED |
| M-2 / R3 partner-licence exclusivity | permanent (any other tenant that ever had a basis on the licence), symmetric on `tenants.licence_id`, same advisory lock | IMPLEMENTED |
| L-1 widened RLS disjunct | basis-named licence readable only if platform-licensee or not bound to another tenant | IMPLEMENTED |
| L-3 readiness masking | a brand row masks the tenant-wide row; nil subject brand ignores brand rows | IMPLEMENTED |
| L-4 nil `ProductCode` wagering vs product-restricted bases | blocked (`basis_product_restricted`) | IMPLEMENTED |
| L-5 PII-ish content | DB CHECKs (character classes + 9-digit-run rule) aligned with the Go lint; audit state carries `has_restrictions_note`, not the note | IMPLEMENTED (other audit fields are ids and opaque tokens) |
| L-2 | I was not given the text of L-2; recorded here as ACCEPTED per the brief, to be named in the ADR by the orchestrator | n/a |

## 2. What changed

- `migrations/0183_operating_basis_hardening.{up,down}.sql` (down restores the 0180 guard, conflict function and read policies byte for byte;
  snapshot-tested). New objects: `operating_bases_close_requires_successor` (DEFERRABLE INITIALLY DEFERRED constraint trigger, SECURITY DEFINER,
  pinned search_path), `tenants_licence_basis_exclusive` (SECURITY DEFINER), `operating_bases_licence_conflict` (now SECURITY DEFINER and
  permanent), `operating_bases_definer_read` / `licences_definer_read` (SELECT policies gated by `pg_trigger_depth() > 0 AND
  launch_is_table_owner()` so the definer functions see every tenant's rows in any session), the narrowed `licences_read` /
  `licence_country_ceilings_read`, and the CHECKs `operating_bases_ref_charset`, `_note_charset`, `_no_long_digit_runs`. The guard
  `operating_bases_governed_guard` is replaced: an owner UPDATE with no matching executed request is LC041 (INSERT keeps the provisioning
  path with NULL change request). SQLSTATE LC044 = close without successor.
- `internal/operatingmarket/rulebook.go` (new): `TenantHasApprovedBasis`, `ResolveRulebookJurisdiction` (Legacy / Basis / Unavailable).
  `resolve_basis.go`: selection carries `jurisdiction_id`; L-4.
- `internal/kyc/enforcement.go`: `resolveEnforcementJurisdiction` (country from the player's verified residence, the same evidence the market
  gate uses).
- `internal/rg/limits_service.go`, `internal/payoutinstrument/service.go`: no licence + approved basis -> error (deny).
- `internal/launchgov/readiness_basis.go` (L-3), `internal/licensingchange/operating_basis_kinds.go` (L-5 lint), `basis_admin.go` (audit).

## 3. H-1 in detail

Rule: for a tenant with NO approved, active, in-force basis, nothing changes (exact legacy path, no extra read beyond one `EXISTS`). For a tenant
WITH bases, KYC deposit/play resolves the scope (tenant, brand, verified residence country, operation[, product]) through the same selector as the
market resolver: a permitting row gives its `jurisdiction_id` (documented no-licence: mandatory field) or its licence's jurisdiction; no row
falls back to the tenant licence, and if there is no licence either, `unavailable` (`kyc_unavailable:basis_scope_unavailable`); no verified
residence, a withdrawn / overdue / not-approved / ambiguous row: `unavailable`. `unavailable` is already a deny at every call site (deposit,
casino, sportsbook, payout). The player's country is not a request parameter: it is read inside the same transaction from kyc's verified
residence (`ReadMarketResidenceEvidence`), so no call site changed. If a vendor geolocation Locator later replaces the KYC-residence evidence
at the market gate, the two sources must be reconciled (the KYC read would then fail closed for players without verified residence).

Not done, deliberately, and why (PARTIALLY): **RG and payout-instrument freshness have no country at their call sites** (a limit is set by a
player, a freshness window is read at payout). For them the rule is: a tenant with an approved basis and NO licence is refused (it can no
longer read as "no configuration / no window"). A tenant that has a licence AND bases keeps the licence's jurisdiction rulebook there
(a real, non-dormant rulebook, but possibly not the partner-brand's). Selecting RG floors by (brand, country) needs a context parameter at
those call sites; proposed as follow-up F1. **Reporting** has no runtime consumer of the tenant licence jurisdiction (grep of every
`ResolveLicensingChain` / `licenceJurisdiction` consumer: kyc, rg, payoutinstrument, operatingmarket, launchgov readiness); nothing to change.
`R-KYC-POLICY` readiness still reads the tenant licence jurisdiction, so it reports `no_licence_jurisdiction` (fail, acknowledgeable) for a
no-licence tenant even though KYC now follows the basis jurisdiction: conservative, to be refined with F1.

## 4. Tests (written with the change; mutation-checked where noted)

`internal/licensingchange/operating_basis_hardening_integration_test.go`: KYC matrix (NULL tenant licence + no-licence basis -> `passed`
under a jurisdiction with a policy [fails without the fix: `not_required`]; same with a jurisdiction without a policy -> `not_required`;
`platform_licence` basis with NULL tenant licence; partner licence for one brand differing from the tenant default; no basis ->
unchanged for no-licence and for licence-with-policy; fail closed for no verified residence, a residence country with no row and no
licence, policies present/absent per jurisdiction); RG `ReadOptions` fail closed (fails without the fix) and unchanged without a basis;
M-1 (owner close LC041, bare close LC044 at commit with the guard disabled, close with successor passes); permanent and symmetric
exclusivity (withdrawn basis does not release; owner and governed bind refused); L-1 read narrowing; L-4; L-5 CHECKs and audit.
`migration_0183_integration_test.go`: up/down/up byte-for-byte. `internal/launchgov/readiness_basis_test.go` (L-3),
`internal/licensingchange/operating_basis_kinds_test.go` (lint classes).

## 5. Pins and existing assertions

No existing assertion was changed or weakened by this slice (lic9 tests pass unchanged). Behavioural note: the lic9 test that
exercised an owner close was not present; an owner close without a request is now LC041 by design.

## 6. Residuals

- F1: scope-aware RG floors / payout freshness (needs country + brand at those call sites).
- R-KYC-POLICY readiness wording for basis tenants (see 3).
- The 9-digit-run CHECK is a character-level approximation of the Go lint (which also screens IBAN / wallet / card shapes); the Go lint is the
  fuller control, the CHECK the last line of defence.
- `classify` does not repeat the new character classes: a request filed by raw SQL with a bad token is refused at execution by the CHECK
  (23514 -> invalid input) rather than at filing.
- Unrelated owner provisioning remains possible for records (never permission): rows without a change request are not approved bases.

## 7. Proposed ADR text (ADR 0045 AMENDMENT-5 / ADR 0123 addendum 2; orchestrator places)

> **Rulebook jurisdiction follows the governing basis (lic14).** The jurisdiction whose rulebook (KYC thresholds, RG floors, payout
> freshness) governs an operation in a scope is, when the tenant has any approved, active, in-force operating basis, selected by the
> governing basis of that scope: the basis row's `jurisdiction_id` (mandatory for `no_licence_documented`) or the governing licence's
> jurisdiction. A tenant with no such basis keeps the tenant-licence behaviour exactly. Where the scope cannot be selected (no verified
> residence country, a basis that does not permit, no row and no licence), the answer is `unavailable`, which every enforcement point treats
> as a deny; "no licence" never reads as "not required" for a tenant that has a basis. Where no scope context exists (RG limits, payout
> freshness), a tenant with a basis and no licence is refused. **Hardening of the basis table:** the table owner cannot close a version
> without an executed request and every close needs a successor (LC041 / LC044); partner-licence exclusivity is permanent and symmetric
> (any tenant that ever had a basis on a tenant-licensee licence excludes every other tenant, from both the basis and the licence binding,
> under one advisory lock); the basis-licence read disjunct is limited to platform-licensee licences or licences not bound to another tenant;
> stored references are single tokens and the note is plain short text, with no long digit runs. **L-2 accepted** (finding text not available
> to the author; the orchestrator names it).

## 8. Commands for the orchestrator

```
export PRIV_DB=<prefix>$(date -u +%H%M%S)p$$x01 PRIV_SRC=/home/user/igaming-platform/.claude/worktrees/lic14
bash <scratchpad>/priv_db.sh
bash <scratchpad>/priv_test.sh -tags integration -count=1 -p 1 ./internal/licensingchange/ ./internal/operatingmarket/ ./internal/launchgov/ ./internal/marketgate/ ./internal/kyc/ ./internal/rg/ ./internal/payoutinstrument/ ./internal/casino/ ./internal/sportsbook/ ./internal/payments/ ./internal/withdrawal/ ./internal/jurisdiction/ ./internal/db/
go test ./...
```
`TestMigration0183_UpDownUp_Snapshot` bases on 0182 and tolerates its absence, so it needs no change when lic11 merges.
