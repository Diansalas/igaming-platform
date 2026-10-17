# U-6 / U-7 closure record (worktree `u67`, branch `u67-closure`, base `76187cc`)

Provider-independent closure. No migration, no ADR edits, no governance or
progress edits. Proposed doc text is at the end, for the orchestrator to apply.

## U-6: test-support route flags derived through the GuardEnvironment check (IMPLEMENTED)

### What I found
`cmd/platform-api/main.go` set the four `httpserver.Deps` test-support route
flags (`CasinoPlaySimulationEnabled`, `PaymentsMockSettlementEnabled`,
`AccountActivationTestSupportEnabled`, `SportsbookSettlementSimulationEnabled`)
straight from `cfg.TestSupportRoutesEnabled()`. That function reads
`Environment` directly, so it returns true for a missing APP_ENV (which resolves
to "development") when TEST_SUPPORT_ENDPOINTS_ENABLED=true.
`mockProviderWiring` (wiring.go) already had the stricter
`&& GuardEnvironment() != "production"` check, but the four route flags did not.
This cannot happen today: `config.Load()` refuses production plus test support,
and `refuseSyntheticInProduction` refuses a missing APP_ENV before routes are
built. The route flags still depended on those separate checks running first.

### Decision
- New `testSupportRoutesGate(cfg config.Config) bool` in
  `cmd/platform-api/wiring.go`. It returns `cfg.TestSupportRoutesEnabled()` only
  when `cfg.GuardEnvironment()` is **explicitly** `"development"` or
  `"staging"`. This is an allow-list: production, a missing APP_ENV, an explicit
  empty APP_ENV, "test" and typos are all off, even for a `Config` built without
  `Load()`.
- All four Deps flags in `main.go` and `mockProviderWiring` (and through it
  `Deps.KYCWebhookEnabled` and every MOCK resolver) now derive from this one
  function.
- `config.Config.TestSupportRoutesEnabled()` is **unchanged**. The existing pin
  in `internal/config/config_test.go:363` ("must be unaffected by
  GuardEnvironment's stricter rule") still holds. No existing assertion changed.
- Documented dev/test behaviour (ADR 0085) is preserved. Explicit
  `APP_ENV=development|staging` with `TEST_SUPPORT_ENDPOINTS_ENABLED=true` still
  turns the routes on. Unset APP_ENV with the flag was already refused at
  startup by the synthetic guard, so no working flow changes.

### Tests (`cmd/platform-api/test_support_gate_test.go`)
- `TestTestSupportRoutesGate_AppEnvTable`: APP_ENV values {unset, empty,
  development, test, staging, production}, each with the flag on, through
  - `config.Load()`: Load either refuses (empty, test, production) or the gate
    equals the expected value (unset → off, development/staging → on); and
  - a hand-built `Config` (bypassing Load), with the flag on and off: the gate
    is on only for explicit development/staging with the flag. It is never on
    when `GuardEnvironment()=="production"`. All six `mockWiring` booleans move
    in lockstep with the gate.
- `TestDepsRouteFlags_DerivedFromSingleGate` (AST/static pin):
  - Every bool field of `httpserver.Deps` (found by reflection) must be
    classified in `depsBoolFlagDerivation`. A new field fails the test, and so
    does a stale entry.
  - `main.go`'s single `httpserver.Deps` literal must assign exactly the
    classified expression: `testSupportRoutesGate(cfg)` for the four flags,
    `wiring.KYCWebhookEnabled`, `true` for SportsbookEnabled, and
    `cfg.GuardEnvironment() == "production"` for AlertRoutingProduction.
  - `wiring := mockProviderWiring(cfg)`.
  - No non-test function in the package calls `TestSupportRoutesEnabled()`
    except `testSupportRoutesGate`, and `mockProviderWiring` calls the gate.
- Mutation-checked:
  - Reverting one Deps flag to `cfg.TestSupportRoutesEnabled()` fails the AST pin.
  - Reducing the gate to `TestSupportRoutesEnabled()` fails the table on the
    "unset" row.

## U-7: every mutating staff route reaches an audit write (IMPLEMENTED, with disclosed limits)

### Static proof (`internal/httpserver/staff_mutation_audit_static_test.go`)
**Enumeration.** The mutating (POST/PUT/PATCH/DELETE) `staffRoute` registrations,
cross-checked three ways: the typed scan, `scanStaffRoutes` (the freeze scanner)
and `staffRouteGolden`. All three must agree. Today that is **135 mutating
routes**.

**Program loading.** Uses only the standard library: `go list -deps -export`
plus `go/types`. Module packages are type-checked from source. Everything else
is imported from compiler export data. A load failure fails the test; it never
skips.

**Call graph.**
- Edges are module functions referenced in a body, whether called or passed as
  a value. Closures count as part of their enclosing function.
- A call through an interface declared in this module expands to every module
  type that implements it (class-hierarchy analysis).
- Interfaces declared outside the module (`http.Handler`, `pgx.Tx`, and so on)
  are not expanded, so the graph cannot route through "every ServeHTTP".
- Bound: 12 calls.

**Roots.** The functions referenced by the route's inner handler expression,
following local variables to their definitions. Package `internal/auth` is
excluded, so a permission or tenant-scope gate that audits a denial can never
be the witness.

**Target.** `audit.Record` is the platform's only audit_log writer. These do
**not** count as reaching it:
- an `audit.Record` call whose Entry, either a literal or a local variable
  defined by one, names `audit.OutcomeDenied` or `audit.OutcomeFailure`;
- any call that passes such a constant as an argument (for example
  `stepUpAudit(..., OutcomeDenied, ...)` or launchgov's
  `auditEntry(..., OutcomeDenied, ...)`);
- a bare value reference to `audit.Record` (`cfg.Audit = audit.Record`);
- the read-audit sink `staffReadCall.auditPlatformRead`.

The one reviewed func-field indirection, `rg.LimitsConfig.Audit`, counts as an
audit write.

**Exact allow-list.**
- A route that reaches no audit write and is not listed fails the test.
- A listed route that does reach one fails the test as stale.
- A listed pattern that is not a mutating route fails the test.

The list has **8 entries, all AML**: holds place/lift, case
open/transition/notes/sar-flag/proposals/approvals. ADR 0116 §5.2 audits these
in their own append-only SECURITY DEFINER trails. I verified this in the
migrations:
- `aml_hold_place` inserts into `aml_holds` with `placed_by` actor, person and
  scope.
- `aml_hold_lift` inserts into `aml_hold_lifts`.
- The `aml_case_*` functions append to `aml_case_events`.

They deliberately do not write audit_log, because audit_log is tenant-readable
(no tipping-off).

**Result.** 127 routes reach a counted audit write, 8 are allow-listed.
Witness depths: 40 at depth 1, 26 at depth 2, 38 at depth 3, 23 at depth 4.

**Controls (`TestStaffMutationAudit_NegativeControls`).** These must **not**
reach an audit write:
- `writeJSON`;
- the denial-only helper `recordAdjustmentDenied` (Entry held in a local
  variable);
- `(*launchgov.Service).denied` (Entry built by a call that passes
  OutcomeDenied);
- `rg.NewLimits` (only a value reference);
- `auth.RequirePermission`.

These must reach one:
- `(*rg.Limits).DecideFloor` (through the field indirection);
- `bonus.DecideSuggestion`;
- `bonus.ClaimSuggestionForReview`;
- `newStaffStepUpHandler` (its success path passes OutcomeSuccess).

The blocked read sink must not reach audit, and no excluded-package root may
appear.

**Mutation-checked.** Removing the claim fix fails both the route check and the
positive control.

### Finding fixed: bonus suggestion claim wrote no audit_log row (severity: Medium)
- **Failure scenario.** `POST /v1/admin/bonus/suggestions/{id}/claim` moves a
  suggestion from Generated to UnderReview and assigns the reviewer. That is a
  staff state change, and it left no audit_log row. It wrote only the bonus
  domain's own `bonus_suggestion_review_events` row. docs/security/
  security-architecture.md §W15 requires `bonus_suggestion.reviewed` rows to
  cover claim/edit/approve/reject. DecideSuggestion audits; the claim did not.
- **Fix.** `internal/bonus/suggestion_lifecycle.go` `ClaimSuggestionForReview`
  now calls `audit.Record` in the same transaction, following DecideSuggestion's
  idiom: action `bonus_suggestion.claimed`, target `bonus_suggestion`/<id>, actor
  staff, tenant, outcome success. Provider-independent, no migration.
- No other mutating staff route lacked an audit write. The AML routes are
  audited by their own design-mandated trails (allow-listed above).

### Runtime proof (`internal/httpserver/staff_mutation_audit_integration_test.go`, build tag `integration`)
`TestStaffMutatingRoutes_WriteAuditRow_PerFamily` sends real HTTP through
`New(Deps)` and the whole `staffRoute` chain:
- authentication and the staff-principal check;
- a live sealed session (`authtest.LiveStaffToken`) and AAL2;
- step-up and permission checks;
- tenant scope.

Each family's request must add exactly one success audit_log row with the right
actor (staff, the caller), tenant, entity (target_type/target_id), and, where
the route takes one, the reason in metadata.

| Family | Route | Role | Reason checked |
|---|---|---|---|
| players | suspend | compliance | yes |
| staff | self display-name | compliance | n/a |
| rg | restrictions | compliance | yes (reason_code) |
| risk | rules (financial step-up) | risk_manager | n/a |
| payments | kill-switch engage | tenant_admin | yes (reason_code) |
| bonus | suggestion claim (the fix) | promotions_manager | n/a |
| platform | brand create on the tenant | platform_admin | n/a |

Mutation-checked: with the pre-fix bonus code, the bonus subtest fails with
"0 bonus_suggestion.claimed audit rows ... want exactly one more".

### Disclosed limits (residuals)
- **Path-insensitive.** The static check proves that the handler can reach a
  success-counting audit write. It does not prove that write is on every
  success branch. Shared parameterized handlers (for example
  `newLifecycleActionHandler(deps, platform, op)`, which switches on the op) are
  proven once for all their routes. The runtime test covers 7 families, not
  every route.
- Calls through function-typed fields or variables are not followed, except
  the reviewed `rg.LimitsConfig.Audit`. A route that audits only through such
  an indirection would show up as a failure, never as a false pass.
- An Entry with a variable or parameter Outcome counts as a write. It is
  filtered out only when the denial or failure constant is visible at the call
  site or in a local definition.
- Scope is the staff route freeze, meaning every `staffRoute` registration.
  These are outside it:
  - pre-session staff auth endpoints (login, MFA verify, refresh, logout,
    invitation accept);
  - player routes;
  - provider webhooks.

  Those audit through `auth`/provider-event paths and are not part of this
  proof.
- The AML runtime trail is not exercised by the new runtime test. It needs the
  officer quorum, acting session and actor proofs. The existing
  `internal/aml/*_integration_test.go` suites cover it.

## Commands and counts (all against a PRIVATE DB `u67141416p14713x01`, dropped afterwards)
- `gofmt -l internal cmd`: clean.
- `go vet` and `go vet -tags integration` on `./internal/httpserver/`,
  `./internal/bonus/`, `./cmd/platform-api/`, `./internal/config/`: clean.
- Unit tests, `go test -count=1 -v`:

  | Package | Passed | Failed |
  |---|---|---|
  | internal/httpserver | 208 | 0 |
  | cmd/platform-api | 81 | 0 |
  | internal/config | 89 | 0 |
  | internal/bonus | 3 | 0 |

- `priv_test.sh -tags integration -count=1 -p 1 ./cmd/platform-api/ ./internal/bonus/`:
  170 PASS, 0 FAIL, 0 SKIP.
- `priv_test.sh -tags integration -count=1 -p 1 ./internal/httpserver/ -run 'Bonus|Suggestion|StaffRoute|StepUp|StaffMutating|Audit|KillSwitch|Risk|RG'`:
  212 PASS, 0 FAIL, 0 SKIP (441 s).
- The full httpserver integration suite was **not** run; only the subset above.

## Proposed registry / doc text (for the orchestrator to apply)
- **ADR 0085 §1 amendment (proposed):**
  > *Amended (U-6, provider-independent closure):* the four `Deps`
  > test-support flags and `mockProviderWiring` are derived through ONE
  > function, `cmd/platform-api/wiring.go` `testSupportRoutesGate(cfg)`. It
  > requires `cfg.TestSupportRoutesEnabled()` AND `cfg.GuardEnvironment()`
  > explicitly in {development, staging}, so no route flag can be true when
  > `GuardEnvironment()=="production"` (including a missing APP_ENV), as
  > defence in depth independent of `Load()` and the synthetic guard.
  > `Config.TestSupportRoutesEnabled()` itself is unchanged.
  > `cmd/platform-api/test_support_gate_test.go` pins this with an APP_ENV
  > table and an AST check over every bool field of `httpserver.Deps`.
- **docs/security/security-architecture.md (audit section, proposed):**
  > Every mutating staff route (`staffRoute`, POST/PUT/PATCH/DELETE) is
  > statically proven to reach `audit.Record` with a success-counting entry
  > (`internal/httpserver/staff_mutation_audit_static_test.go`). The only
  > exceptions are the AML officer routes, which ADR 0116 §5.2 audits in
  > append-only SECURITY DEFINER trails instead of tenant-readable audit_log.
  > They are kept in an exact allow-list that fails on new or stale entries.
  > `bonus_suggestion.claimed` is the audit action for a suggestion claim.
- **Registry status line:** U-6 IMPLEMENTED; U-7 IMPLEMENTED (static
  reachability for 135 routes plus 7-family runtime proof; path-insensitivity
  disclosed); finding U-7-F1 (bonus claim unaudited) FIXED.
- The comment in `staff_stepup_integration_test.go` that counts "125 mutating
  routes" is historical. The freeze now has 135. It was not changed here.
