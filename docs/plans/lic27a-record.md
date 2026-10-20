# lic27a: RG options never hide the protective tools (S-1) and the SQL confirm honours a missing floor (S-2)

Branch `lic27a-rg-options` (from integ-c c6667c3; head 0188; no new migration: 0187 edited in place, up and down, as agreed). Software capability only.

## Findings and fixes

| Finding | Fix | Label |
|---|---|---|
| S-1 MEDIUM: `ReadOptions` refused entirely on a missing floor / calendar conflict, so the B2C page showed an error instead of time-out and self-exclusion | `ReadOptions` always returns `time_out_hours` and `self_exclusion_days`; an affected limit kind is returned with `ConfigInForce=false`, no periods, no ceiling, no cooldown and `UnavailableReason` = `missing_floor` or `calendar_conflict` (wire: additive optional `unavailable_reason`). The write path is unchanged: increases, removals and confirms stay refused. | IMPLEMENTED |
| S-1 B2C | `LimitsCard` offers only available kinds, shows a note for unavailable ones, hides the set form when none is offered; the page already renders `TimeOutCard` / `SelfExclusionCard` whenever options exist, which the server now guarantees | IMPLEMENTED |
| S-1 readiness | R-RG-CONFIG and the activation guard fail `calendar_conflict` when the relied-on jurisdictions (licence UNION approved bases) disagree on `calendar_tz` / `week_start` for any kind and currency (a one-member set cannot conflict, so licence-only tenants are unchanged). Acknowledgeable like the other detail codes. | IMPLEMENTED |
| S-2 LOW: confirm refusal only in Go | `rg_limit_change_classify_set` gets `p_strict` (true for a tenant with an approved basis: trigger passes `has_basis`, Go passes `JurisdictionStrict`, the 0135 wrapper passes false); in the confirm branch a strict tenant is refused (RL023) when the set is empty, incomplete or any member has no floor | IMPLEMENTED |

**SnapshotVersion is NOT bumped** (no check added; a detail code `calendar_conflict` was added to the existing R-RG-CONFIG).

## Declared test changes

- `TestHardening_H1_RGOptions_FailClosedForBasisTenantWithoutLicence` (lic14 pin) asserted that `ReadOptions` errors for a basis tenant without floors. Intent kept (no silent weaker options): it now asserts the read is served with every limit kind unavailable (`missing_floor`, no periods, no ceiling) and time-out / self-exclusion present.
- `TestLic25_BasisOnlyTenant_TimeOutAndLimitsWork_ReadOptionsStillFailClosed`: same change (name kept); the later "options with floors everywhere" half is unchanged.
- The migration 0187 function signature `rg_limit_change_classify_set` gained `p_strict boolean` (unreleased; the 0135 wrapper and all existing callers keep working).

## Tests

New: `TestLic27a_CalendarConflict_OptionsAndReadiness`, `TestLic27a_ConfirmRefusedInSQL_WhileAReliedJurisdictionHasNoFloor` (raw INSERT of a confirm row; control tenant without basis confirms), `wantUnavailableOptions` helper, two b2c vitest cases (protective cards present with every kind unavailable; only available kinds offered).
Mutations, each killed: options refusing on missing floor, calendar conflict refusing options, readiness without the agreement check, SQL confirm without `p_strict`, SQL confirm ignoring a missing floor.

## Residuals

1. Unavailable kinds show a generic note (no jurisdiction names are exposed to players).
2. `calendar_conflict` compares only the relied-on set at readiness time; a later floor change can re-introduce a conflict (the options read then marks the kind unavailable, enforcement itself is unaffected as it uses one jurisdiction).
3. The B2C page still shows an ErrorState for a genuine server failure of the options read; the protective cards then need data the client does not invent.
