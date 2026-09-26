# Stage 10.3 planning gate — Real Provider Trust & Casino Financial Readiness

**Status: PLANNING ONLY. Nothing in this document is implemented. Stop point: the Stage 10.3
implementation gate (human approval required).**

Baseline: branch `claude/focused-wright-jw88w9`, Stage 10.2 complete at `957a3e8`. AWS staging torn
down (see `docs/governance/staging-teardown-2026-09-26.md`); staging status **OFF**.

Specialist working papers (this folder is the evidence; this document is the synthesis):

| Paper | Owner | Covers |
|---|---|---|
| `docs/plans/stage-10.3-planning/00-roadmap-reconciliation.md` | orchestrator (research) | master roadmap reconciliation, discrepancies |
| `…/01-provider-trust-analysis.md` | `architect` | WH-VENDOR-SCHEME-1, credential resolver/secret store, MOCK-ADAPTER-PROD-1, PAYWH-*, other provider blockers, diagrams |
| `…/02-casino-financial-analysis.md` | `ledger-finance` | CAS-CAP-ROLLBACK-1, casino reconciliation, other casino financial gaps |
| `…/03-kyc-reason-bound-analysis.md` | `identity-compliance` | KYC-REASON-BOUND-1, other KYC provider-readiness gaps |
| `…/04-review-*.md` | reviewers | reviews of this proposal (§17) |

## 1. Executive summary

Stages 10.1–10.2 made every webhook tenant-bound and verify-first — **for the MOCK providers only**.
Before the platform can connect its first real PSP, KYC vendor or casino aggregator, five things must
exist that do not today:

1. a way for each real adapter to verify its **own vendor's** signature scheme without weakening the
   ADR 0022 §3 contract (WH-VENDOR-SCHEME-1);
2. a **real credential resolver** backed by the secret store, with per-tenant handles, rotation and
   fail-closed behaviour;
3. a **production guard** that refuses to start with any synthetic (mock) adapter wired;
4. a **casino capability contract** that never strands existing exposure (CAS-CAP-ROLLBACK-1), plus the
   multi-bet round defect (G-1) found during analysis;
5. **casino reconciliation**, so provider/platform divergence and missing events are detected.

KYC-REASON-BOUND-1 (bounding provider free text) is small and included. PAYWH-TS-1 is recommended
**closed** (superseded by the mandatory timestamp conformance case); PAYWH-BRAND-1 and PAYWH-RL-1 stay
deferred with explicit triggers.

Everything proposed can be built and tested **locally** (unit, integration, RLS, concurrency, conformance)
with staging OFF. Only the AWS Secrets Manager wiring drills are **STAGING REQUIRED**, and they are
deferred to the single future governed staging deployment.

## 2. Exact scope (proposed)

| ID | Item | Wave | Blocks first real adapter? |
|---|---|---|---|
| WH-VENDOR-SCHEME-1 | per-adapter `VerificationScheme` (Extract/Verify/Properties); orchestrator-enforced verify; shared preamble selects the scheme by provider; conformance suite `webhookauthtest` SC1–SC13 incl. mandatory timestamp window; remaining non-mock conformance skips → failures | W1a | Yes (all three domains) |
| MOCK-ADAPTER-PROD-1 | `Synthetic` marker on all eight mocks (payments, KYC, casino, sportsbook catalogue, malware scanner, …); pure startup guard refusing `APP_ENV=production` with any synthetic component; AST test that every mock carries the marker | W1b | No (pre-launch gate) |
| CAS-CAP-ROLLBACK-1 | capability/status gates **new bets only**; wins, rollbacks, replays, tombstones never gated; gate moves into `postBet` using the session's `BrandID`; unseen-original rollback always tombstoned; late original after tombstone → named rejection (not 500); `postRollback` takes L0.1 on the original reference; migration: `CHECK (NOT supports_bet OR (supports_win AND supports_rollback))` | W1c | Yes (casino) |
| G-1 (new, casino) | a round with two cash bets returns 500 on any win (`ErrAmbiguousMultiOriginRound`) — characterise, then fix | W1c | Yes (casino go-live) |
| KYC-REASON-BOUND-1 | provider `reason` bounded (512 bytes, control chars stripped) at the adapter boundary; raw text **staff-only**; player sees a closed `reason_code` (see HD-10.3-3); migration: length CHECK + `reason_code` column with pre-flight | W1d | Yes (KYC, player-facing safety) |
| Credential resolver | FORCE-RLS `provider_credential_handles` (no secret values; pinned secret version + fingerprint); `Resolver(tx)`; in-memory and dev-file secret-store backends; cache keyed by pinned version; rotation overlap (`verify_only`, `not_after`); fail-closed uniform 401; admin handle API with audit + OpenAPI | W2a | Yes (all three) |
| PROV-OUTBOUND-CRED-1 (new) | outbound provider calls carry tenant + resolved credential; replace the process-wide static key in the HTTP client with an `Authenticator` | W2a | Yes |
| KYC provider selection (O4) | remove hard-coded `Provider("mock")` in `kyc_handlers.go` | W2a | Yes (KYC) |
| Casino reconciliation — internal | `casino_consistency` stream C1–C7 on the existing run/mismatch tables; verified-but-rejected callback record (append-only); P1 on drift, never auto-correct | W2b | Yes (casino go-live) |
| Casino reconciliation — statement | provider-neutral `CasinoStatementSource` + MOCK source; `casino_statement` stream | W3a | Interface + MOCK yes; a real source is PROVIDER DEPENDENT |
| Secret-store AWS backend (code) | `awssm` backend behind the store interface, tested against an SDK-interface fake | W3b | Yes, for staging/production |

## 3. Non-scope (explicit)

- No real vendor, no real credential, no vendor contract (human decision; not needed for W0–W3).
- **No AWS action.** `deploy/` IAM/KMS/egress changes for the task role are **excluded** unless the
  human authorizes them (HD-10.3-2); even then, applying them is a separate human-authorized staging
  deployment.
- PAYWH-BRAND-1 (trigger: a tenant with separate merchant accounts per brand at one provider),
  PAYWH-RL-1 (pre-launch; edge + app rate limits) — deferred.
- LEDGER-MANUAL-ADJ-4EYES-1 (four-eyes manual adjustment / mismatch resolution API) — platform-wide,
  blocks real-money go-live; proposed as its own later stage, not 10.3.
- Casino hardening G-3 (DB backstop for one reversal per original, via LEDGER-REV-UNIQ), G-4
  (cross-domain provider-id namespace rule), G-7 (stranded exposure on deregistration) — deferred.
- Free rounds, jackpots, bonus-funded casino stakes — not required; a conformance rule forbids mapping
  them to wins.
- Hosted-KYC session token (J12), sanctions/PEP vendor interface — registered, not in 10.3.
- Any AI implementation (ADR 0089 stays architecture only). B2B, retail, cashout — out.

## 4. Current architecture (verified at `957a3e8`)

- `internal/webhookauth`: provider-neutral Credential/Resolver/Inbound/Reason/AuthError, the platform
  MOCK `Scheme` and per-domain parameters; `internal/httpserver/webhook_preamble.go` hard-requires the
  MOCK header format before any adapter runs (the WH-VENDOR-SCHEME-1 constraint).
- Payments, KYC and casino orchestrators: route tenant → credential resolve → verify raw bytes → parse →
  tenant-scoped work; strict I1 for KYC/casino; mock resolvers wired only under
  `TestSupportRoutesEnabled()` (`cmd/platform-api/wiring.go`).
- Real resolvers: NOT IMPLEMENTED. Secrets Manager is used today only for platform secrets (DB, JWT).
- Casino capability check runs before the event-type switch in `ReceiveCallback`, loading the
  tenant-wide row (`brandID = uuid.Nil`).
- Reconciliation: `internal/reconciliation` has sportsbook streams only.

## 5. Dependency graph and implementation waves

```
W0  paper: ADR 0092 (Stage 10.3 definition) · ADR 0093 provider credential model & secret store ·
    ADR 0022 §3 amendments (pt 2 key-id overlap, orchestrator-enforced verify, pt 9 handle-read
    allowance, new pt 10 timestamp window) · ADR 0085 §1 (synthetic guard) · ADR 0025 + ADR 0082
    (capability contract, L0.1 in postRollback) · ADR 0028 (reason bound) · registry updates
 │
 ├─► W1a WH-VENDOR-SCHEME-1 ───────────────┐
 ├─► W1b MOCK-ADAPTER-PROD-1 (independent) │
 ├─► W1c CAS-CAP-ROLLBACK-1 + G-1 ─────────┼──► W2b casino_consistency + rejection record ─► W3a casino_statement (MOCK)
 └─► W1d KYC-REASON-BOUND-1                │
                                           ▼
                        W2a credential resolver + handles + outbound creds + O4
                                           │
                                           ▼
                        W3b awssm backend (code, local fake)
                                           │
                                           ▼
            [STAGING REQUIRED, human-authorized] IAM/KMS/egress apply + rotate/outage drills
```

- Critical path to a first real adapter: W0 → W1a → W2a → W3b (→ staging drills).
- W1a–W1d are independent and can run in parallel (disjoint packages; W1a touches all three
  orchestrators' verify path, so W1c/W1d merge after W1a or rebase on it).
- Each wave closes with its own reviews and green CI; no wave merges on red.

## 6. Migrations (provisional numbers, assigned in merge order)

| # | Wave | Content | Pre-flight | Down |
|---|---|---|---|---|
| 0094 | W1c | casino capability CHECK `NOT supports_bet OR (supports_win AND supports_rollback)` | the constraint build itself (a `count(*)` pre-check sees zero rows under FORCE RLS); refuses rather than auto-editing config | drop constraint |
| 0095 | W1d | `kyc_verifications.reason` length CHECK (512) + `reason_code` column with CHECK over the approved enum | normalise existing rows (synthetic data only) inside the migration | drop both |
| 0096 | W2a | `provider_credential_handles` (tenant_id, domain, provider_id, key_id, secret_ref, pinned version, fingerprint, status `active|verify_only|revoked`, not_after), FORCE RLS, composite FKs, no platform read policy | n/a (new table) | drop table (refuses if rows exist unless explicitly forced by runbook) |
| 0097 | W2b | append-only `casino_callback_rejections` + new reconciliation mismatch kinds | n/a | refuses once evidence exists (as 0091) |

No migration edits history; every migration is reversible on a fresh DB and covered by the
reversibility CI step and the chain-tip pin tests.

## 7. APIs

- Webhook routes unchanged in shape; each adapter's own headers documented per provider in OpenAPI when
  a real adapter lands; the MOCK entries stay byte-identical.
- New staff API (W2a): create/rotate/revoke provider credential handles (tenant-scoped, platform-admin or
  tenant-admin permission to be ruled by `security`), audit with before/after, never returns secret
  material; OpenAPI + contract test.
- KYC (W1d): player responses gain `reason_code`, lose any provider free text; staff responses keep the
  bounded raw text.
- Casino (W1c): new named rejection for a late original after its tombstone (409 or `declined`; `casino`
  rules); win/rollback no longer 503 on a disabled capability.
- Reconciliation (W2b/W3a): read-only admin endpoints follow the sportsbook pattern.

## 8. Security model and webhook trust boundary

```
 provider ──HTTP──► edge ──► preamble: provider id charset, body size, tenant slug (platform-wide)
                               │
                               ▼  WithTenant(route tenant)                       [RLS context]
                  orchestrator: handle row read (pt 9 allowance, read-only)  ──► Resolver(tx)
                               │                                                   │ secret store
                               ▼                                                   ▼ (pinned version)
                  scheme := adapter.VerificationScheme()   Verify(raw bytes, cred, timestamp window)
                               │  (orchestrator calls Verify; adapter cannot skip it)
                     fail ─────┴──► uniform 401 + allow-listed log; nothing else ran
                               ▼ ok
                  adapter.Parse → tenant-scoped domain work → ledger (idempotent)
```

- Points 1–9 of ADR 0022 §3 remain binding; amendments (W0) add: orchestrator-enforced verify; the
  read-only handle lookup as the only pre-verification tenant-scoped statement for KYC/casino; key-id
  overlap for vendors that send none; point 10, mandatory timestamp tolerance for every real scheme.
- Secret values never enter the database, logs, errors, OpenAPI or git; the DB holds references and
  fingerprints only. Fingerprint mismatch or store outage → fail closed (uniform 401).
- `security` must review: the scheme interface, conformance self-test, handle table + RLS, resolver
  cache, admin handle API permissions, the SDK dependency, the synthetic guard, and the casino
  kill-switch change (disabled capability no longer stops settlement; emergency stop becomes
  credential revocation).

## 9. Provider credential model

```
 tenant ─┬─ domain (payments|kyc|casino) ─┬─ provider_id ─┬─ key_id (active)       ─► secret_ref@version
         │                                │               ├─ key_id (verify_only)  ─► secret_ref@version  (until not_after)
         │                                │               └─ key_id (revoked)      ─► —
 platform secrets (DB, JWT) stay separate (ADR 0086); dev/test: memory + dev-file backends;
 staging/production: AWS Secrets Manager (already the accepted platform store) — STAGING REQUIRED to prove
```

Rotation = add new key as `active`, old as `verify_only` with `not_after`; revocation is immediate
(handle read per request inside the caller's transaction). Four-eyes on credential changes: `security`
ruling in W0.

## 10. Casino reconciliation model

- **`casino_consistency`** (internal, always-true checks C1–C7): round binding; posting shape; orphan
  wins; rollback linkage; tombstone conflicts; provider-asserted events the ledger lacks (from the
  rejection record); tombstones later matched by an original.
- **Loss-by-silence:** casino has no loss callback, so ageing cash rounds are a metric, not a P1.
- **`casino_statement`:** ledger vs a provider-neutral statement (MOCK source today; real = PROVIDER
  DEPENDENT).
- Same advisory lock, append-only evidence, P1 log line and no auto-correct as sportsbook; compensation
  is a human four-eyes action — the mechanism is LEDGER-MANUAL-ADJ-4EYES-1 (out of scope, go-live
  blocker).

## 11. Financial invariants (unchanged, re-asserted per wave)

Double-entry, debits = credits; append-only ledger; idempotency by `(tenant, provider_id, provider_tx_id)`;
no balance UPDATE; compensating entries only; tombstone-before-original rejection; ADR 0082 lock order
(W1c adds L0.1 in `postRollback` — ADR 0082 amendment, `ledger-finance` owner); rejected callbacks have
no financial effect; hourly projection drift check untouched. Every financial change ships the CLAUDE.md
test list (normal, duplicates, concurrency, retries, partial failure, rollback, settlement,
reconciliation, callbacks, idempotency, authorization, auditability).

## 12. RLS

New tables (`provider_credential_handles`, `casino_callback_rejections`) carry `tenant_id`, FORCE RLS,
composite FKs, runtime role grants minimal (handles: read by resolver, write by admin API only; no
platform read policy). Integration tests run as the NOBYPASSRLS runtime role; cross-tenant read/write
attempts fail; statement-capture proves the handle read is the only pre-verification tenant-scoped
statement.

## 13. Failure modes

| Failure | Behaviour |
|---|---|
| secret store unavailable | cached pinned version serves; no cache → uniform 401; alarm |
| fingerprint mismatch / revoked handle | uniform 401, allow-listed log, security alert |
| vendor sends no key id | overlap rule: try `active`, then `verify_only` within `not_after` (amended pt 2) |
| timestamp outside window | 401 (`timestamp_out_of_window`) |
| synthetic adapter in production | process refuses to start |
| disabled casino capability | new bets declined/503; settlement of existing exposure proceeds |
| late original after tombstone | named rejection, no posting |
| reconciliation drift | P1 log + mismatch row; no auto-correct |

## 14. Rollback

Each wave is independently revertible by `git revert` plus its migration's down path (all reversible on
a fresh DB; evidence-holding tables refuse down by design, as 0091). The synthetic guard can be
bypassed only by removing the synthetic component, never by a flag. No wave rewrites git history.

## 15. Testing matrix (local unless marked)

| Area | Unit | Integration (NOBYPASSRLS) | Concurrency | Mutation-kill | Conformance | Staging |
|---|---|---|---|---|---|---|
| VerificationScheme + orchestrator verify | ✓ | ✓ | — | ✓ | SC1–SC13 + broken-scheme self-test | — |
| Synthetic guard | ✓ (matrix) | — | — | ✓ | AST marker scan | — |
| Capability contract + G-1 | ✓ | ✓ event × capability table | late original vs rollback | ✓ | casino conformance | — |
| KYC reason bound | ✓ | ✓ migration pre-flight | — | ✓ | KYC conformance | — |
| Credential resolver | ✓ | ✓ RLS, rotation, revoke | cache/revoke races | ✓ | — | **STAGING REQUIRED**: IAM, network path, rotate/outage drills |
| Casino reconciliation | ✓ | ✓ C1–C7, idempotent runs | concurrent runs (advisory lock) | ✓ | — | — |
| Migrations 0094–0097 | — | ✓ up/down/pre-flight | — | — | — | — |

Gates per wave: gofmt, vet, golangci-lint, race unit, 3× race integration, reversibility, OpenAPI
contract tests, secrets scan, specialist reviews.

## 16. Production blockers after 10.3 (carried)

Real vendors and contracts; production credentials; LEDGER-MANUAL-ADJ-4EYES-1; PAYWH-RL-1; raw-payload
retention (O7); hosted-KYC session token; sanctions/PEP interface; sportsbook production-readiness items;
jurisdiction items gated by HDR-J-*; licensing/legal; production configuration checklist; the single
future governed staging deployment and its full acceptance; launch authorization. Full list:
`00-roadmap-reconciliation.md` §7.

## 17. Specialist reviews

Required before the implementation gate is presented as ready: `security`, `qa` (test plan binding),
`product-owner-proxy` (scope/overengineering). Recorded as `docs/plans/stage-10.3-planning/04-review-*.md`
with the Orchestrator rulings in §19. During implementation: `casino` and `ledger-finance` (W1c, W2b,
W3a), `identity-compliance` (W1d), `payments` (W1a/W2a), `architect` (W0, W1a), `code-reviewer` per wave.

## 18. Human-decision register for Stage 10.3

| ID | Decision | Needed for | Default if not decided |
|---|---|---|---|
| HD-10.3-1 | Approve Stage 10.3 scope (this document), in whole or by wave | any implementation | nothing starts |
| HD-10.3-2 | Include the `deploy/` task-role IAM/KMS/egress **code** change for Secrets Manager in 10.3 (apply remains a separate human-authorized staging deployment) | W3b staging readiness | excluded; W3b ships the backend code with a local fake only |
| HD-10.3-3 | Player-facing KYC `reason_code` values and wording (jurisdiction/compliance call) | W1d player surface | players see **no** provider reason at all (status only); bound + staff-only raw text still ship |
| HD-10.3-4 | Settlement semantics for a **suspended tenant's** verified casino callbacks (may carry licensing weight) | W1c edge case | unchanged: suspended tenant's callbacks rejected (exposure may strand; disclosed) |

Engineering decisions (not human): scheme shape, conformance suite, handle table/RLS, Secrets Manager as
store (already accepted), cache/TTLs, amendments to ADRs 0022/0025/0082/0085/0028 (with the owning
specialists' concurrence), wave order, TS-1/BRAND-1/RL-1 dispositions.

**Carried human decisions (unchanged, not decided here):** ADR 0009/AUP; HDR-J-6, HDR-J-7, HDR-J-8,
HDR-J-9; HDR-M-1, HDR-M-2; HDR-SB-1; OB-1; licensing, legal, vendor and retail decisions; and the others
listed in `00-roadmap-reconciliation.md` §5. G-2 Terminal-Grant credit, the open-bet self-exclusion
default and mixed/bonus-funded cashout were answered in ADR 0042 (implementation outstanding).

## 19. Review record and Orchestrator rulings

(Filled after the §17 reviews.)

## 20. Staging requirements

Staging is **OFF** and stays off throughout 10.3. Items marked **STAGING REQUIRED** (Secrets Manager IAM,
network path, rotation/outage drills, plus the Stage 10/10.1/10.2 acceptance that never ran on AWS —
`00-roadmap-reconciliation.md` §8) are deferred to the single future governed staging deployment from
the final approved commit. No deployment is proposed here.

## 21. Implementation gate

**Stage 10.3 implementation does not start until the human explicitly approves HD-10.3-1** (and rules on
HD-10.3-2..4 or accepts their defaults). On approval: record ADR 0092 as ACCEPTED, register the scope rows
in `docs/governance/task-registry.md`, then execute W0 → W1 → W2 → W3 with per-wave gates, stopping at the
Stage 10.3 completion gate.
