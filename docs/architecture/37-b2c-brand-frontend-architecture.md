# 37 — B2C Brand Frontend Architecture

Status: Stage 4H-B1 Wave 1.5 Fix Round 2 ("Product Surface Roadmap Gate")
architecture freeze. **No UI code is authorized or written by this
document.** This is the third of three parallel Product Surface
documents this round — see `35-product-surfaces-roadmap.md` (overall
roadmap skeleton and cross-domain dependency graph, `architect`) and
`36-backoffice-and-partner-console-architecture.md` (Operator Back
Office / Partner Console, `backoffice`), neither of which this document
restates. This document covers the **B2C Brand Frontend** only — the
player-facing site/app for our own B2C casino brand and, later, any
other tenant's B2C-shaped brand under the hybrid licensing model.

Owning specialist: `frontend`. Cross-cutting architecture changes implied
here (new config fields, new API endpoints) are dependency requests to
the owning backend domain, per `integration-protocol.md`, not
unilateral additions.

## 0. Why this document exists

The roadmap has no explicit implementation stage for any of the three
product surfaces the Blueprint defines (B2C Brand Frontend, Operator
Back Office, Partner Console) — a real, human-flagged gap, not a defect
found by a specialist. This document defines the target architecture and
a concrete, honestly-scoped MVP stage for the B2C surface so that stage
can be authorized and sequenced by the human, exactly as every other
domain in this project has been. It does not select a Human Decision
Register item and does not start `Stage 6C` (`35-product-surfaces-
roadmap.md` §5, `OI-PSR-1`) itself.

## 1. Core architectural principle: brand differences are configuration, never code

`CLAUDE.md`'s multi-tenancy rule is absolute: *"Nothing brand-specific
may become a code path. Brand differences are configuration rows (theme,
catalogue, payment methods, currencies, languages, RG defaults, bonus
templates, jurisdiction rules, provider credentials, domains), versioned
and editable from the partner console."* The B2C Brand Frontend is a
**single codebase, single deployable artifact** (mirroring ADR 0010's
platform-wide single-deployable posture) that renders **N different
brands** — our own first-party B2C brand today, external B2B operator
brands later — from data, never from an `if (brand === 'x')` branch, a
brand-named component, a brand-named CSS file, or a brand-named build
target.

### 1.1 What already exists to drive this today

This is not a green-field configuration model to invent — real rows
already exist and this frontend consumes them as-is:

| Config axis | Source (verified in code) | What it drives |
|---|---|---|
| Brand identity, theme, default locale | `brands` table (migration `0008`; `internal/identity.Brand` — `Name`, `Slug`, `Status`, `DefaultLocale`, `Theme JSONB`). Public-read RLS (`brand_public_read`) — deliberately public, since an unauthenticated visitor's browser needs it to render the site at all, the same class of data as a public site's own HTML. | Logo/color/typography tokens, the brand's default UI language, which brand a given hostname/slug resolves to |
| Per-jurisdiction currencies, payment methods, geo-block list, RG/KYC/AML/reporting ruleset IDs | `tenant_jurisdiction_configs` (migration `0002`: `allowed_currencies`, `allowed_payment_methods`, `geo_block_list`, `kyc_ruleset_id`, `aml_ruleset_id`, `rg_ruleset_id`, `reporting_ruleset_id`, versioned via `effective_from`/`effective_to`) | Which currencies/assets and payment methods a player in a given jurisdiction is offered; which RG/KYC ruleset governs them; geo-blocking |
| Asset registry (per-currency/crypto decimal exponent) | `assets` (migration `0003`), filtered per-jurisdiction by `tenant_jurisdiction_configs.allowed_currencies` | Correct minor-unit display/rounding per asset, never hardcoded per brand |
| Game catalogue and availability | `internal/casino` — platform-wide catalogue (`PUT /v1/admin/casino/games`, platform_admin-owned) with **tenant-scoped opt-in availability** (`PUT /v1/admin/casino/games/{gameID}/availability`) and provider capability config (`PUT /v1/admin/casino/providers/{providerID}/capability`) | Which games a given brand's lobby shows — a filter over a shared catalogue, never a brand-specific game list baked into the frontend |
| Withdrawal policy | `internal/withdrawal` policy admin API (`GET/POST/DELETE /v1/admin/withdrawal-policies`) | Cashier withdrawal limits/approval thresholds shown to the player as policy, not frontend-hardcoded limits |

**Gap, stated honestly**: `brands.theme` is a single opaque `JSONB`
blob today — sufficient for an MVP's color/logo tokens, but the schema
does not yet define a structured contract (e.g. named token keys,
versioning, a JSON Schema) for what the frontend may read from it.
Defining that contract is in-scope for the MVP stage (§3.2) and is a
`tenant-config`/`architect`-owned schema addition, not a frontend-local
convention. Likewise there is no first-class `bonus_templates`,
`languages` (beyond `default_locale`), or `rg_defaults` config surface
today, because the domains that would own that state — Bonus Engine,
CRM/segment-facing localization, RG per-brand defaults beyond the
jurisdiction ruleset — are themselves design-only or not started (see
§2). The frontend must not invent a substitute for any of these; it
renders what exists and degrades gracefully (hides the feature area)
where the config axis doesn't exist yet, per §3.

### 1.2 How this is enforced concretely, not just stated

- **One render path.** A single `BrandTheme` resolver reads
  `brands.theme` + `tenant_jurisdiction_configs` for the resolved brand
  and jurisdiction and produces one typed configuration object; every
  component consumes that object, never a brand identifier used to
  select a component variant.
- **No compiled-in brand list.** The frontend build is brand-agnostic;
  a new brand requires zero frontend deploy, only new rows (`brands`,
  `tenant_jurisdiction_configs`, casino availability, withdrawal policy).
  This is the frontend-side mirror of `02-domain-and-service-
  boundaries.md`'s "no brand-specific code path" rule and of doc 26's
  identical rule for the retail hierarchy ("no table, constraint, enum,
  Go type or permission may contain a brand-specific value as a
  structural element").
  Enforced by CI: a lint rule fails the build on any brand-name literal
  (other than test fixtures) or on any string comparison against
  `brand.slug`/`brand.id` used to branch rendering logic.
- **Brand resolution is server-driven, never client-asserted.** The
  frontend resolves which brand it is by hostname/slug against
  `GetBrandBySlug` (public read, pre-authentication — this is
  intentional and matches `identity.GetBrandBySlug`'s own documented
  rationale: brand identity is public data, not a trust assertion) and
  never lets a query parameter or `localStorage` value pick a different
  tenant's configuration for a rendered page. Once a player authenticates,
  every subsequent read/write is scoped server-side to the JWT's
  `tenant_id`/`brand_id`, exactly as every other surface on this
  platform (§4).
- **Per-jurisdiction variation is data, not a second theme system.**
  A Europe player and a LATAM player under the *same brand* see the same
  theme, different currencies/payment methods/RG ruleset — one
  `tenant_jurisdiction_configs` row per jurisdiction, not a
  region-specific frontend build.

## 2. Feature areas: information architecture and backend dependency mapping

Each feature area below states its backend dependency, that dependency's
actual implementation state (verified against `internal/`, not assumed),
and the honest consequence for what an MVP can ship today. This mirrors
`02-domain-and-service-boundaries.md`'s Owns/Never-does discipline: the
frontend is never the place a feature area gets "implemented anyway" via
a stubbed API or hardcoded fixture data standing in for a domain that
doesn't exist.

| Feature area | Backend domain | Backend state | MVP inclusion |
|---|---|---|---|
| Registration / login | `internal/identity`, `internal/auth`, `internal/identityresolution` | **IMPLEMENTED** — `POST /v1/auth/register`, `/login`, `/refresh`, `/logout`, `GET /v1/me`, session listing/revocation (`internal/httpserver/auth_routes.go`) | **Included.** Full email/password registration+login, refresh rotation, session management. |
| Lobby | `internal/casino` (catalogue + tenant availability) | **IMPLEMENTED** (mock provider) — `GET /v1/me/casino/games` | **Included**, mock-provider content only, labeled as such in UI copy is not required (players don't need "mock" in copy) but the *system* is `MOCK` per CLAUDE.md's completion labels — see §3.5. |
| Casino (game launch/play) | `internal/casino` | **IMPLEMENTED** (mock provider; real aggregator is `PROVIDER DEPENDENT`) — `POST /v1/me/casino/games/{gameID}/launch`, bet/win/rollback via provider webhook | **Included** against the mock provider. Frontend embeds/redirects to whatever launch URL the mock returns; no game-specific frontend logic (that would violate provider abstraction — game specifics stay behind `internal/casino`'s adapter). |
| Sportsbook | `internal/sportsbook-adapter` (per doc 02's proposed boundary) | **NOT STARTED** — no `internal/sportsbook` package exists | **Excluded.** No sportsbook UI, navigation entry, or route in the MVP. Adding a disabled nav placeholder is a UI decision for the MVP build, not this document. |
| Wallet / cashier (balance, deposit, withdrawal) | `internal/wallet`, `internal/ledger`, `internal/payments`, `internal/withdrawal` | **IMPLEMENTED** — `GET /v1/me/wallets`, `GET /v1/me/wallets/{assetCode}`, `POST/GET /v1/me/deposits`, `POST/GET/GET-by-id/{cancel} /v1/me/withdrawals`, withdrawal state machine, four-eyes admin review (player-facing side only touches the `/v1/me/*` routes) | **Included.** Multi-wallet balance read (per-asset, never a single generic balance), deposit initiation against the mock/sandbox PSP, withdrawal request + cancel + status. |
| Bonuses | `bonus-engine` (Campaign/Offer/Grant/Progress) | **NOT STARTED** as a runtime package — architecture exists (`10-bonus-engine-architecture.md`, `28`, `29`) but no `internal/bonus` implementation ships bonus-crediting or wagering-progress code today (per Stage 4H-B0-R7's active-stage record: workstream design-validated only, no bonus code authorized) | **Excluded.** No bonus opt-in, wallet bonus-balance display beyond the raw `player_locked_cash`/`player_locked_bonus` ledger split if a player happens to have one, or wagering-progress UI. Gated on the Bonus Engine landing (Stage 4H-B1 proper, not this round). |
| Promotions | `bonus-engine` (Offer surfacing) + `crm` (targeting) | **NOT STARTED** — both `internal/bonus` and `internal/crm` are absent | **Excluded** entirely, including a static "promotions" marketing page wired to real offer data — a hardcoded promotions page would itself violate CLAUDE.md's "no fabricated" and "brand differences are configuration" rules by inventing content with no backing domain. |
| CRM communications (in-app messages, preference center, notifications) | `internal/crm` | **NOT STARTED** — no package, and DEP-CRM-1 (a marketing/communication consent model) doesn't exist anywhere in the platform yet, per doc 02 §"Segmentation / CRM / Affiliate boundaries" | **Excluded.** No in-app inbox, no communication-preference screen beyond what §2's "Account" row below covers (login/security notices are not CRM). |
| Gamification (points, XP, achievements, missions, tournaments, leaderboards) | `internal/gamification` | **NOT STARTED** — architecture frozen Stage 4H-A, zero implementation | **Excluded** entirely. |
| Responsible Gaming (self-exclusion, status) | `internal/rg` | **IMPLEMENTED** (the shipped subset: self-exclusion + status; deposit/loss/wagering/session limits and reality checks are documented ADR 0026 §15 extension points, **NOT IMPLEMENTED** yet) | **Included** for the implemented subset only: self-service self-exclusion (`POST /v1/me/rg/self-exclusion`), status read (`GET /v1/me/rg/status`). Deposit/session/loss limits and reality checks are **excluded from this MVP because the backend doesn't exist**, not a design choice — see §3.4 for why this is stated as a compliance-relevant gap, not silently dropped. |
| Account (profile, sessions) | `internal/identity`, `internal/auth` | **IMPLEMENTED** — `GET /v1/me`, session list/revoke | **Included.** |
| KYC | `internal/kyc` | **IMPLEMENTED** (foundation; real vendor verification is `PROVIDER DEPENDENT`/mock) — `POST/GET /v1/me/kyc/verifications`, `POST/GET /v1/me/kyc/documents`, document content read | **Included**: verification status display, document upload against the mock/sandbox verifier. Frontend never renders a KYC "approved" state as legal clearance — it renders `internal/kyc`'s own status string, unmodified, per CLAUDE.md's "software capability ≠ legal approval." |
| Language | `brands.default_locale` (per-brand) | **IMPLEMENTED** as a config field; no `internal/i18n`-style multi-language content management exists — this is a frontend-owned string-catalogue concern, not a missing backend domain | **Included** at MVP scope as: brand-driven default locale + a frontend-maintained string catalogue for the languages the brand's markets require. Not a backend dependency in the way other rows are; recorded here for completeness of the information architecture, not as an "excluded — backend missing" item. |
| Currency / asset | `assets` registry + `tenant_jurisdiction_configs.allowed_currencies` | **IMPLEMENTED** | **Included** — wallet/cashier only ever displays and lets a player act on an asset in that jurisdiction's allowed set, using the registry's per-asset exponent, never a frontend-local currency list. |

### 2.1 Honest summary

Real backend support exists **today** for: registration/login, lobby +
mock-provider casino, wallet/cashier (read + deposit/withdrawal against
the existing ledger and withdrawal state machine), RG self-service
(self-exclusion + status only), account, KYC (foundation), language and
currency/asset. Bonuses, promotions, CRM communications, gamification,
and sportsbook have **no backend package at all** — building any
frontend surface for them now would mean either faking data (a CLAUDE.md
violation — "no fake completion," "never fabricate a successful
integration") or building throwaway UI against an interface that will
change once each domain's real architecture (already frozen in docs
10/17-20/30-32) is actually implemented. Both are the exact
"uncontrolled scope expansion" CLAUDE.md's scope-expansion test exists to
block.

## 3. B2C Brand Frontend MVP implementation stage

**`Stage 6C`** (`35-product-surfaces-roadmap.md` §5, `OI-PSR-1`) — this
document originally used a placeholder here pending `architect`'s
numbered roadmap skeleton; that document has since been published and
names this stage `Stage 6C`, mechanically substituted here per its own
`OI-PSR-1` follow-up. This section defines the stage's scope, dependencies,
technology choice, and release criteria so it is ready to slot into that
roadmap and be authorized by the human at the appropriate gate — it does
not itself authorize starting the stage.

### 3.1 Scope

**In scope** (buildable against implemented backend, per §2):
registration/login (incl. session management), lobby, mock-provider
casino game launch, wallet/cashier (balance read, deposit initiate/list,
withdrawal request/list/cancel), RG self-service (self-exclusion,
status), account (profile, sessions), KYC (verification status,
document upload).

**Explicitly excluded**, each gated on its backend domain landing first:
bonuses, promotions, CRM communications, gamification, sportsbook. No
placeholder screen for these may call a stubbed/mocked bonus, CRM, or
gamification endpoint — an empty state, a "coming soon" static message,
or simply omitting the nav entry are the only acceptable treatments,
decided at MVP build time, not architected here.

### 3.2 Dependencies

- **Backend APIs**: the full `/v1/auth/*`, `/v1/me/*` (profile, sessions,
  wallets, deposits, withdrawals, casino games/launch, rg/self-exclusion,
  rg/status, kyc/verifications, kyc/documents) surface listed in §2's
  table, all already implemented and route-registered in
  `internal/httpserver`. No new backend endpoint is strictly required to
  build the in-scope feature set.
- **Tenant/brand config**: `GetBrandBySlug` (public read) for
  brand/theme/locale resolution pre-authentication;
  `tenant_jurisdiction_configs` (via whatever read API the backend
  exposes for it — none is listed among today's routes, so **this is a
  dependency request**: the frontend needs a public, jurisdiction-aware
  read of `allowed_currencies`/`allowed_payment_methods` to render the
  cashier and registration currency/country pickers correctly. Filed as
  an open item, §3.2.1, not built around with a frontend-local
  workaround).
- **`brands.theme` structured contract** (§1.1's stated gap): the MVP
  needs an agreed, versioned key set (e.g. `primary_color`,
  `logo_url`, `favicon_url`, typography tokens) before the frontend can
  consume it type-safely instead of treating it as an arbitrary blob.
  This is a small `tenant-config`/`architect` schema addition, tracked
  as a dependency, not invented frontend-side.

#### 3.2.1 Open dependency items (not resolved by this document)

| Item | Owner | Blocks |
|---|---|---|
| A public read endpoint for `tenant_jurisdiction_configs` (or the specific fields a pre-auth registration/cashier flow needs from it) scoped to the resolving brand's tenant | `tenant-config` / `architect` | Registration country/currency picker; cashier currency list before jurisdiction can otherwise be inferred |
| `brands.theme` structured key contract (versioned schema, not an open blob) | `architect` (schema) + `frontend` (consumer contract) | Type-safe theming; prevents a de-facto "whatever keys the first brand happens to use" contract |
| Confirmation that no additional player-facing read is needed for RG's not-yet-implemented deposit/loss/session-limit/reality-check controls (they are out of scope, but the frontend's account/RG screen should not need rework when they land) | `identity-compliance` (RG) | Low — informational, avoids near-term rework |

### 3.3 Frontend technology recommendation

**Recommendation: a component framework with first-class SSR and partial
hydration (React with Next.js, or an equivalent such as SvelteKit) over
a pure SPA.**

Rationale, given this platform's actual multi-tenant/multi-brand
requirements — not a generic framework preference:

- **Brand resolution must happen before first paint, server-side.** A
  brand's theme, default locale, and (once §3.2.1 lands)
  allowed-currency set are resolved from the hostname/slug before a
  single pixel renders — a client-side-only SPA would either flash
  unbranded/default-branded content first (a real player-facing defect
  for a white-label B2B brand) or block on a client fetch before
  rendering anything. SSR resolves brand config as part of the initial
  request, matching how `GetBrandBySlug`'s own pre-authentication,
  public-read design already anticipates a server-side resolution step.
- **SEO and the public lobby/landing surface matter for a consumer
  casino brand** (organic acquisition, jurisdiction-specific landing
  pages) in a way that doesn't apply to the Back Office or Partner
  Console — those two are legitimately SPA-appropriate (per
  `36-backoffice-and-partner-console-architecture.md`, authenticated
  staff tools with no SEO need). This is the one respect in which the
  B2C surface's technology needs genuinely differ from the other two
  product surfaces.
- **Per-request, per-tenant rendering is a natural fit for SSR's request
  lifecycle**: the same deployable serves every brand, resolving brand
  config fresh (within its documented TTL, per doc 02's "no service
  caches tenant config longer than its documented TTL" rule) on each
  request rather than baking brand data into a client bundle at build
  time — reinforcing §1.2's "no compiled-in brand list" rule at the
  infrastructure level, not just the code-review level.
- **Partial hydration/streaming** keeps authenticated, interactive
  surfaces (wallet actions, KYC upload, casino launch) fully dynamic
  while the mostly-static lobby/marketing shell stays cheap to render
  and cache per brand.
- Any framework meeting these three properties (SSR-by-default,
  per-request tenant resolution, partial hydration) satisfies this
  recommendation; React/Next.js is named as the concrete, currently most
  operationally mature choice, not a locked-in requirement — the human
  or a later `architect` review may substitute an equivalent without
  reopening this rationale.

### 3.4 RG / compliance-sensitive UI considerations

These are **regulatory requirements this platform must satisfy, not
design preferences a build team may trade off against conversion
metrics.** CLAUDE.md's compliance section is explicit that enforcement
is our code, not the vendor's, and that jurisdiction-pluggable RG rules
must vary without a rewrite — the frontend's obligation is to never
narrow, hide, or delay what the backend already enforces.

- **Self-exclusion and RG status must be reachable from account
  navigation at all times**, at a consistent, unhidden location — never
  nested behind a "settings > advanced > more" pattern, never requiring
  a support-chat request, never gated behind a retention-flow
  interstitial ("are you sure? here's a bonus to stay" is a dark pattern
  this platform must not ship). This is a direct UI expression of
  `internal/rg`'s own design stance: "a player has an inherent right to
  self-exclude... RG (not RBAC) is what authorizes it."
  self-exclusion.
- **A self-exclusion action must be irreversible from the player's own
  UI**, matching the backend: `rg_routes.go`'s own comment states
  "end restriction early" is explicitly not exposed to any actor via
  this API today. The frontend must never imply, via copy or a disabled-
  but-visible "cancel my exclusion" control, a capability that doesn't
  exist server-side; the RG status screen shows the backend's real
  status only.
- **No engagement mechanic may compete with an RG control for visual
  priority.** Since bonuses/promotions/CRM/gamification are excluded
  from this MVP entirely (§2, §3.1), this risk is structurally absent at
  MVP launch — but it becomes live the moment any of those domains lands
  in a future stage, and this document records the standing rule now so
  it is not re-litigated per-feature later: an RG-adjacent screen
  (deposit, withdrawal, session) may never surface a bonus/promotion/
  gamification element in the same view once those exist.
- **Deposit limits and reality checks are the one place this MVP is
  honestly incomplete against Blueprint expectations for a licensed
  operator**, because `internal/rg` does not implement them yet (§2).
  This is recorded here, not silently absorbed into "responsible gaming:
  done" — the MVP's account/RG screen must state plainly (to the human
  reviewing release criteria, and eventually in-product once those
  controls exist) that only self-exclusion is live. Shipping the MVP
  without them is a scope decision for the human at the stage-gate, not
  a frontend judgment call, since it touches licence-relevant capability
  claims (CLAUDE.md: "software capability and legal/regulatory/licensing
  approval are different things").
- **KYC status must never be overstated.** The frontend renders
  `internal/kyc`'s literal verification state; it must not add
  UI copy implying a legal "you're cleared to withdraw any amount" claim
  beyond what the withdrawal policy/KYC tier actually gates server-side.

### 3.5 Testing approach

- **Component/unit tests** for the `BrandTheme` resolver and every
  config-driven render path (theme tokens, locale, currency list),
  including a test asserting the CI brand-literal lint rule (§1.2)
  itself catches an intentionally-introduced brand-name branch —
  a regression guard on the guard, matching this project's established
  pattern (e.g. the `player_locked` migration guard's own regression
  test in Stage 4H-B0-R7).
- **Integration tests against the real backend API** (not mocked
  responses) for every in-scope flow: register → login → view lobby →
  launch mock game → deposit → withdraw → cancel withdrawal →
  self-exclude → confirm subsequent login/play is blocked per RG's own
  enforcement (never asserted by the frontend itself — the test proves
  the frontend surfaces the backend's denial correctly, not that the
  frontend enforces it).
- **Multi-brand rendering tests**: at least two distinct `brands` rows
  (differing theme, locale, jurisdiction currency set) rendered through
  the same build, asserting no shared state leaks and no code path
  differs — the direct test of §1's core principle.
- **Accessibility and RG-visibility tests**: automated checks that the
  self-exclusion/RG-status entry point is present and reachable within a
  fixed number of interactions from any authenticated screen — a
  compliance-relevant regression class, not an ordinary UX nice-to-have,
  gated the same way security-sensitive functionality requires
  `security` review before being marked complete (CLAUDE.md).
- **No test may fabricate a passing bonus/CRM/gamification flow** against
  a stub — those feature areas have no backend, so no test exercises
  them; a skipped/absent test suite for them is the honest state, not a
  gap to paper over with a mock.

### 3.6 Release criteria

An MVP release is `IMPLEMENTED` only when all of the following hold,
each independently verifiable, not self-attested:
1. Every in-scope feature area (§3.1) works end-to-end against the real
   (mock-provider-backed) backend in a dev/sandbox environment — no
   fixture data standing in for a live API call.
2. Zero brand-specific code paths — verified by the CI lint rule (§1.2)
   passing and by the multi-brand rendering test (§3.5) passing for at
   least two brands with materially different config.
3. RG self-exclusion and status are reachable and functionally correct
   per §3.4, reviewed and signed off by `security` (compliance-sensitive
   UI, per CLAUDE.md's security-review requirement) — not merely coded.
4. No client-side authorization or business-rule enforcement exists
   anywhere in the frontend (§4) — verified by code review confirming
   every mutating action's only client-side effect is calling a
   server API and rendering its response, never a local eligibility
   check that gates whether the call is made.
5. KYC/RG status displays are reviewed against CLAUDE.md's "software
   capability ≠ legal approval" language rule — no UI copy implies
   licence/legal clearance the backend hasn't actually asserted.
6. `qa` has signed off on the testing approach in §3.5 as sufficient for
   this stage's actual scope (mirroring the project's standing practice
   of `qa` gating "sufficient," not the implementing specialist).
7. Bonuses/promotions/CRM/gamification/sportsbook are demonstrably
   absent from the shipped build (no dead code, no disabled-but-present
   API client for a domain that doesn't exist) — a `code-reviewer` check
   against scope creep, per CLAUDE.md's no-uncontrolled-scope-expansion
   rule.

Labeling: per CLAUDE.md, the released MVP is labeled `IMPLEMENTED` for
its in-scope feature set, `PROVIDER DEPENDENT` where it fronts a
mock-only backend capability (casino games, KYC verification, PSP
deposit/withdrawal rails), and `NOT IMPLEMENTED` — never silently
absent — for every excluded feature area, referencing this document's
§2 table as the reason.

## 4. Permissions and business rules are never enforced client-side

This frontend follows the identical discipline `CLAUDE.md`'s Security
section states for the whole platform, applied with zero exception for
being "just the player-facing site": **authorization is enforced
server-side only, scoped by tenant, and never inferred from the UI or
trusted from the client.** Concretely, for this surface:

- Every wallet, deposit, withdrawal, RG, KYC, casino-launch, and
  account action is a call to an already-authenticated,
  already-permission-checked backend endpoint (`internal/httpserver`'s
  `auth.Middleware` + `auth.RequireTenantScope`/`RequirePermission`
  chain, or RG's RLS-based self-authorization) — the frontend never
  encodes "can this player withdraw this amount," "is this player
  self-excluded," or "is this KYC tier sufficient" as client-side logic
  that decides whether to attempt the call. It always attempts the
  call and renders whatever the server decides (success, denial,
  validation error).
- The frontend never receives, stores, or asserts a `tenant_id` or
  `player_account_id` value that it sends back to the server as a claim
  of identity — every identifying field the backend routes already
  resolve server-side from the authenticated session (see each route's
  own doc comments in `internal/httpserver/*_routes.go`, e.g.
  `financial_routes.go`'s explicit "every identifying field is resolved
  server-side" note). The frontend has nothing to trust client-side in
  the first place.
- Disabling a button, hiding a menu item, or showing a "you must verify
  your KYC first" message client-side is a UX affordance, never the
  authorization mechanism — the corresponding server call remains
  independently gated and the UI test suite (§3.5) must never assert
  security by "the button was hidden," only by "the server rejected the
  bypassed call."
- This applies identically to the theme/brand config resolution in §1:
  the frontend does not decide which brand's data a player may see —
  `GetBrandBySlug`'s public-read policy and every authenticated read's
  server-side tenant scoping already fully determine that, and the
  frontend's job is limited to rendering what the server hands back.

## 5. Summary for the roadmap gate

This document defines the B2C Brand Frontend's target architecture and
an honestly-scoped MVP stage, ready to be slotted into
`35-product-surfaces-roadmap.md`'s skeleton once that document lands. It
makes no Human Decision Register selection and authorizes no
implementation. The two real open items requiring another specialist's
action before the MVP stage can start cleanly are §3.2.1's dependency
requests (a jurisdiction-config read endpoint; a structured `brands.theme`
contract) — both small, both routed through `integration-protocol.md`
rather than built around.
