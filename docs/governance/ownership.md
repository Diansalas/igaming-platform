# File / Domain Ownership

Permanent project governance document (Stage 4G, Part A). Defines which
specialist owns which files/directories. An agent MUST NOT silently
modify a file outside its own ownership — see `integration-protocol.md`
for the dependency-request procedure when cross-domain changes are
needed. When ownership is ambiguous, the Master Orchestrator decides
before any edit occurs, and records the decision in `task-registry.md`.

## Ownership table

| Domain | Owner | Paths |
|---|---|---|
| Cross-domain architecture | architect | `docs/architecture/*`, cross-cutting ADRs |
| Financial / Ledger | ledger-finance | `internal/ledger`, `internal/wallet`, `migrations/00{19-23}*`, `migrations/*ledger*`, `migrations/*wallet*` |
| Payments | payments | `internal/payments`, `migrations/*deposit*`, `migrations/*withdrawal*`, `migrations/*provider_capabilit*` |
| Identity | identity-compliance | `internal/identity`, `internal/identityresolution`, `migrations/*person*`, `migrations/*player_account*`, `migrations/*identity*` |
| KYC | identity-compliance | `internal/kyc`, `internal/email`, `migrations/*kyc*`, `migrations/*credential_token*` |
| Responsible Gaming | identity-compliance | `internal/rg`, `migrations/*restriction*` |
| Risk Management | risk | `internal/risk`, `migrations/*risk*` |
| Asset Registry + Authorization | architect | `internal/assetregistry`, `migrations/0044-0045_asset_*`, `migrations/0047_asset_registry_*`, `internal/httpserver/asset_registry_*.go`, `docs/decisions/0037-*.md` |
| Idempotency (shared adapter scaffolding) | integrations | `internal/idempotency` — a reusable primitive; a domain wiring it into a real posting path (casino, sportsbook, payments) owns that call site, not this package |
| Casino | casino | `internal/casino`, `migrations/*casino*`, `internal/httpserver/*casino*` |
| Sportsbook | sportsbook | `internal/sportsbook` (not yet created) |
| Bonus Engine | bonus-engine | `internal/bonus` (not yet created — Stage 4H-B1 AUTHORIZED, Wave 1 design/contract complete, Wave 2 implementation not yet dispatched) |
| Player Segmentation (shared) | architect (interface/contract/schema shape); bonus-engine (first consumer's call sites) | `internal/segment` (not yet created — ratified at Stage 4H-B1 Wave 1 reconciliation, `docs/architecture/29-bonus-implementation-contract.md` §3: a separate, capability-minimal package from day one, justified by segment_ref already being a first-class field in four frozen Stage 4H-A docs (17/18/19/20), not speculative generality. A rule DSL, recompute job, or CRM-shaped engine is explicitly NOT authorized inside this package) |
| Retail / Agent Network — architecture | architect | `docs/architecture/26-retail-operations-architecture.md`, cross-cutting retail ADRs (Stage 4H-B0) |
| Retail / Agent Network — implementation (if authorized) | **OPEN DECISION** — `backend` (adequate while architecture-only) vs. a new dedicated `retail` specialist (architect's recommendation once implementation is authorized) | `internal/agentnetwork` (hierarchy primitive — deliberately not `internal/retail`, since a B2B sub-operator tree or affiliate chain is the same graph and a retail-specific name would guarantee a second consumer duplicates it), `internal/retail` (operational surface) |
| Retail financial accounting | ledger-finance | `docs/decisions/0035-retail-agent-network-accounting.md`, retail account/transaction types |
| Retail RBAC/RLS/audit | security | `docs/decisions/0036-retail-hierarchy-rbac-and-audit.md`, hierarchy-scope RLS mechanics |
| API / HTTP (general) | backend | `internal/httpserver/server.go`, routing/shared middleware, and any handler file not claimed by a more specific domain above |
| API / HTTP (domain handlers) | the domain's own specialist | `internal/httpserver/<domain>_handlers.go`, `<domain>_routes.go` (e.g. `kyc_handlers.go` → identity-compliance, `credential_handlers.go` → identity-compliance) |
| Database / RLS (schema shape, cross-cutting) | architect + security | `internal/db`, tenant-scoping helpers (`WithTenant`/`WithPlayerScope`/etc.) |
| Database / RLS (a specific migration) | the domain that owns the table | Each migration is owned by whichever domain's table it creates — a migration touching multiple domains' tables (rare — e.g. adding a composite FK from a Risk table to `player_accounts`) needs sign-off from every owning domain before merge |
| Security | security | Cross-domain review authority; owns `internal/auth` session/token/permission mechanics directly |
| Audit | shared, no single owner | `internal/audit` is a stable, rarely-changed shared primitive — a change to its own code requires architect + security sign-off; every domain WRITES audit records via `audit.Record` without needing ownership of the package itself |
| Documentation (governance) | Master Orchestrator | `docs/governance/*`, `docs/progress.md`, `docs/active-stage.md` |
| Documentation (domain) | the domain's own specialist | `docs/decisions/<domain's own ADRs>`, `docs/architecture/<domain's own doc>`'s "Implementation status" sections |
| OpenAPI | the domain adding endpoints, reviewed by backend | `docs/api/openapi/platform-api.yaml` |
| CI/CD, observability, infra | devops | `.github/workflows`, `Makefile`, `docker-compose.dev.yml`, `internal/observability` |
| Testing strategy | qa | Cross-cutting test conventions; each domain owns its own `*_test.go`/`*_integration_test.go` files |

## Rules

1. **No silent cross-domain edits.** An agent needing a change in a file
   it does not own files a dependency request (see
   `integration-protocol.md`) rather than editing it directly.
2. **Same-file conflicts are serialized.** If two domains' work would
   touch the same file in one stage (e.g. `internal/httpserver/server.go`
   wiring two new `Deps` fields), the Orchestrator assigns ONE owner for
   that file for that stage - recorded as that owner in the file's
   `task-registry.md` row - and the other domain's need is filed as a row
   in `task-registry.md`'s Dependency Request Log (see
   `integration-protocol.md`) rather than a second, uncoordinated edit.
   The owner applies both changes serially, in one pass, never as two
   agents editing the same file concurrently.
3. **New tables/migrations are owned by the domain that creates them**,
   even if another domain later adds a read-only foreign key into them
   (e.g. Risk's `risk_rules.player_account_id` composite FK into
   `player_accounts` — Risk owns the migration; Identity is not required
   to co-review a foreign key that only reads its table's shape, but IS
   consulted if the FK requires a NEW constraint on `player_accounts`
   itself, per the ownership rule above).
4. **Ambiguous ownership defaults to the Orchestrator**, who assigns it
   explicitly (recorded in `task-registry.md`) before any edit — never
   left to whichever agent gets there first.
