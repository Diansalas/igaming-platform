# lic28: protective first limit and lower-only controls for an unavailable limit kind (F-1)

Branch `lic28-rg-protective-ui` (from integ-c 6390a68; head 0188; no migration, 0187 untouched). Software capability only. AML untouched.

## F-1 and the fix

A kind marked `unavailable_reason` (missing_floor, calendar_conflict) was dropped from the B2C form and its rows offered only "Remove limit", although the server always accepts an initial value and a decrease and refuses only increases, removals and confirms.

- `ReadOptions`: an unavailable kind now carries `protective_only=true` and `allowed_periods` = the union of the periods of the relied-on jurisdictions that DO have a floor (missing_floor: from the partial fold; calendar_conflict: from `composeConfig`, which does not read the calendar). No ceiling, no cooldown, `config_in_force=false`. If no jurisdiction has a floor the list is empty and the form offers nothing for the kind. Wire/OpenAPI/b2c type: additive optional `protective_only`.
- `LimitsCard`: a protective kind is selectable in the form with its periods; a value that is not lower than the existing limit of that period is refused locally (the server would classify it as an increase and refuse it); the cooling-off blurb and the raise warning are replaced by "only a first limit or a lower one"; rows of an unavailable kind show a "Lower to" input and "Lower limit" button instead of "Remove limit"; the note now says exactly "Raising or removing a limit is not available right now for: <kinds>. You can still lower an existing limit or set a first one where the form below offers it...". `submit` takes the kind explicitly (the old "Remove limit" path read a stale `kind`).

## Declared test change

`wantUnavailableOptions` (lic27a/lic25 tests) previously required an empty period list for an unavailable kind; it now requires `ProtectiveOnly`, no ceiling, no cooldown, `ConfigInForce=false` (the pin test with no floors anywhere still gets an empty list, asserted by the new tests). Intent (never a silent weaker option: nothing that raises exposure is offered) is preserved.

## Tests

Go (integration): `TestLic28_ProtectiveOnly_FirstLimitAndDecreaseAccepted_IncreaseRemovalRefused` (options periods; first limit accepted, the floored jurisdiction's ceiling still applies, decrease accepted, increase and removal `config_missing`; confirm covered by `TestLic27a_ConfirmRefusedInSQL...` and `TestLic25_LimitSetting_Reliance...`), `TestLic28_CalendarConflict_ProtectivePeriodsAndWritesStillWork`.
vitest: lower an existing limit while its kind is unavailable (local refusal of a non-lower value, then the exact POST body); set a first protective limit (no cooling-off text, no remove control); a raise of an existing limit is refused locally and nothing is posted.
Mutations killed: no protective periods (missing floor), none for calendar conflict (two variants), `ProtectiveOnly` false, Remove shown for unavailable kinds, Lower control hidden, local raise guard removed.

## INFO documented (security)

An acknowledged `calendar_conflict` makes the account-level usage read (`ReadUsageConfig` / `ReadEffectiveLimitInputs`, via `readUsageConfigFor`) of that kind fail closed (error `errCalendarConflict`), and the options read marks the kind unavailable. Money-path enforcement (`EvaluateAction`) selects ONE jurisdiction per scope and does not fold calendars, so it is not blocked by it; limit setting is not blocked either (composeConfig ignores the calendar). Nothing here weakens a protection. (Checked in code; the coordinator's wording "blocks deposits/play" would only hold for a consumer of the account-level read.)

## Residuals

1. The first protective limit is bounded by the ceiling of the floored jurisdictions only; the UI does not show that ceiling (nothing partial is displayed), an over-ceiling value gets the server's refusal.
2. `src/features/rg/*` other files were not reformatted (U-5 float-money allow-list matches exact source lines).
