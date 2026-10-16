# Sports-data adapter: conformance test specification

Status: **SPECIFICATION (`NOT IMPLEMENTED`).** **DEFERRED with sportsbook** (owner decision 2, 2026-10-15). No test is
written by this package. There is **no existing feed conformance suite** because there is no feed interface (see
`README.md`). The cases below specify the suite the first feed adapter must bring, derived from ADR 0117 Part C.6 and
the invariants SBF-1..SBF-10, plus the existing suites the consuming paths already have. Case ids `SD-C-nn` are new;
existing test names are cited as they exist at base `4a3c85f`.

Rules for the suite author:

- A case that cannot run for a real adapter must **fail**, never skip (precedent: `internal/casino/conformance_skip_to_fail_test.go`).
- Vendor behaviour is taken from vendor-documented examples or sandbox captures with a citation. A test must not encode
  an assumed vendor behaviour (`U-SD-nn` items stay open until cited).
- Fixtures contain synthetic data only. No licensed official data is committed to the repository unless the data licence
  allows it (an owner/legal input); recorded fixtures are minimal and synthetic.
- A feed message never posts money. Money tests end with the ledger invariants and the `sportsbook_settlement` stream clean.

## 1. Existing suites that apply unchanged

| Suite | Where | Applies as | Notes |
|---|---|---|---|
| Catalogue fetch/sync boundary | `internal/sportsbook/catalogue_io_boundary_integration_test.go`, `catalogue_write_authorization_integration_test.go`, `provider_reference_bound_test.go`, `provider_reference_bound_integration_test.go`, `provider_reference_integration_test.go` | mandatory for the catalogue part of any feed adapter | `FetchCatalogue` refuses under `txscope.Held`; one bad `ExternalRef` rejects the fetch; `SyncCatalogue` needs the platform-service scope. These pin today's last-writer-wins behaviour; SD-C-12 pins its replacement |
| Acceptance gates | `orchestrator_integration_test.go`, `market_guard_integration_test.go`, `jurisdiction_catalogue_integration_test.go`, `internal/httpserver/sportsbook_flow_integration_test.go` | run unchanged | acceptance reads catalogue state; SD-C-30..34 add staleness |
| Settlement entry points | `settlement_*_integration_test.go`, `sportsbook_settlement_sole_writer_test.go` | run unchanged | the dispatch consumer must call the same sole writers; the static sole-writer test must still pass |
| Reconciliation | `internal/reconciliation/sportsbook_settlement_integration_test.go` | run unchanged | a fed settlement must keep the stream clean |
| Webhook scheme conformance | `webhookauthtest.RunSchemeConformance` SC1..SC13 + known-answer vector, `conformanceManifest` | mandatory if the feed pushes | platform-scope credential; PSP G-01/G-02 apply |
| Generic HTTP client boundary | `internal/providers/httpclient/client_test.go`, `conformance/conformance.go` | mandatory for polling | success, timeout, transport failure, 4xx/5xx, malformed body, flaky-then-succeeds, duplicate-call observation; redirects refused |
| Production guard | `internal/providerkind` completeness scan, `cmd/platform-api/no_mock_binaries_test.go` | mandatory | `sb_feed_sources.kind` allows only `mock` until an ADR admits a real kind |

## 2. Binding rows (known open items to close or accept before the first real feed adapter)

| Row | Source | Condition | Case |
|---|---|---|---|
| ADR 0117 Part C slices C1..C4 built and reviewed | ADR 0117 | at least C1 (MOCK) before any adapter; C2 and C3 before feed-driven bets/settlement; C4 before push or live | gate D0 |
| S-B2 ingest seals; "stolen-credential SQL path can bypass platform-service GUC identities" residual | ADR 0117 s D | launch-blocking for any non-MOCK feed | SD-C-40, gate D5 |
| C0 lock class as an ADR 0082 R8 exception; `FOR SHARE` grants on `sb_*` | ADR 0117 C.2.7e | amendment recorded by the orchestrator; grants confirmed with `security` | SD-C-33 |
| Mandatory result hold thresholds | ADR 0117 OQ-C2 | owner/risk supply per-tenant per-asset values; none configured disables feed settlement | SD-C-24 |
| Dead-heat / fractional outcomes | ADR 0117 C.3 | NOT IMPLEMENTED: rejected with alert | SD-C-26 |
| X-G01 credential decision; PSP G-01/G-02 | `../README.md` | decided | gate D0 |
| Sportsbook operating model OQ-SB-01 | `../sportsbook/` | decided | gate D0 |
| ALERT-DELIVERY-1 | readiness report | production blocker (watchdog alerts depend on delivery) | gate D8 |

## 3. New cases

### 3.1 Transport, authentication, SSRF

| Id | Case | Needs DB |
|---|---|---|
| SD-C-01 | Push and poll of the same recorded stream produce the same catalogue and result state | yes |
| SD-C-02 | Poll endpoint comes only from compiled adapter config or dual-control platform config with an HTTPS host allowlist; private address ranges, link-local and metadata ranges denied; cross-host redirects refused; response size and time caps enforced; a cursor or any feed field used as a URL is refused (SSRF set) | no |
| SD-C-03 | Known-answer vector for the push scheme verifies; tamper matrix; expired/future timestamp refused (`ReplayTimestampWindow`) | no |
| SD-C-04 | Admission: a bad signature, replayed timestamp or non-allowlisted source is refused before any work; a flood of feed traffic cannot exhaust tenant admission buckets (own pre-auth bucket) | yes |
| SD-C-05 | The feed credential is a platform-scope secret-store handle: never in the database, logs, audit or errors; rotation and revocation take effect on the next message | yes |

### 3.2 Mapping, ordering, duplicates

| Id | Case | Needs DB |
|---|---|---|
| SD-C-10 | Every documented entity type maps to a canonical shape or is explicitly dropped with a reason; vendor ids are bounded (1..255 bytes, UTF-8, no controls), namespaced by `feed_id` | yes |
| SD-C-11 | Unmapped reference in a live message is recorded and never creates an entity; the pre-provisioning workflow creates it | yes |
| SD-C-12 | Out-of-order and duplicate messages: a message with `Version <= feed_version` is `stale_dropped`; a duplicate `(feed_id, message_id)` is `duplicate`; a finished event or closed market is never reopened by an older or replayed message; a re-sync never re-parents an entity | yes |
| SD-C-13 | The adapter declares `entity_sequence` or `timestamp_sequence`; a feed offering neither is refused at registration | no |
| SD-C-14 | Feed restart / sequence reset / gap: documented behaviour (full resync) leaves no stale entity open and never lowers a version | yes |
| SD-C-15 | Malformed, oversize, unknown-kind and out-of-enum messages are `rejected_invalid`, alerted, and change no state | yes |
| SD-C-16 | Odds conversion is exact (numerator/denominator); a non-exact or non-positive price is refused; `odds_version` increments only on a real change | yes |

### 3.3 State, staleness, suspension

| Id | Case | Needs DB |
|---|---|---|
| SD-C-30 | Heartbeat loss beyond `suspend_after` suspends the feed's live markets, writes `sb_state_changes` with source `watchdog`, raises an alert; an unauthenticated message cannot resume; a fresh authenticated state message or a staff action can | yes |
| SD-C-31 | Acceptance with a stale or unconfigured feed is refused (`RejectionFeedStale`); an unconfigured staleness bound also refuses | yes |
| SD-C-32 | Pre-match market past `start_time` is refused; live events are refused until the live rules exist | yes |
| SD-C-33 | C0 race: a message applying concurrently with an acceptance cannot produce an acceptance at a price/status that was never visible; lock-order harness including C0 passes; `accept_better` freezes the live price for exposure and risk; the static R8-exception check passes | yes |
| SD-C-34 | Staff trading operations (suspend, void event, manual result) are actor-proof operations; manual results need four-eyes | yes |

### 3.4 Results and settlement

| Id | Case | Needs DB |
|---|---|---|
| SD-C-20 | A result names exactly the market's own selections; otherwise `rejected_invalid` and alerted; an unsealed result is refused (SBF-9) | yes |
| SD-C-21 | Each hold trigger fires individually: total payout over threshold, single payout over threshold, correction raises payout, result before `start_time`, market not suspended/closed under C0; a held result posts nothing and the stake stays in `player_locked_cash` | yes |
| SD-C-22 | Confirmation of a held result without four-eyes actor proof is refused; with it, settlement proceeds | yes |
| SD-C-23 | Fan-out: two tenants plus an inactive tenant with open bets (refusal recorded, partition not blocked); tenant set read from `tenants`, never from feed data; a crash mid-fan-out redelivers without double posting | yes |
| SD-C-24 | A tenant with no configured threshold has feed-driven settlement disabled (fail closed) | yes |
| SD-C-25 | Corrections: lower version is a no-op; same outcome newer version writes an adoption row; different outcome is rollback(g)+settle(g+1) in one transaction with `g` from the database; correction after a void raises an alert | yes |
| SD-C-26 | Dead heat, fractional or partial outcomes are rejected with an alert and post nothing | yes |
| SD-C-27 | The posted payout equals the bet's frozen odds payout, never a feed-stated amount (SBF-4); the feed carries no money field the settlement path reads | yes |
| SD-C-28 | `driver='feed'` and a NULL staff actor on feed settlements; a `driver='simulation'` row in production is a P1 mismatch (`sb_simulation_driver_in_production`) | yes |
| SD-C-29 | New reconciliation checks: `sb_unsettled_after_result`, `sb_outcome_vs_result`, `sb_settlement_stale_result` | yes |

### 3.5 Ingest integrity

| Id | Case | Needs DB |
|---|---|---|
| SD-C-40 | Ingest seals (S-B2): a row written through a stolen-credential SQL path that bypasses the platform-service identity is detected (seal mismatch) and never settles | yes |
| SD-C-41 | The ingest and watchdog identities are allowlisted, each with a single-call-site static test; NULL-arm fences hold | yes |
| SD-C-42 | Message history (`sb_feed_messages`, `sb_price_history`, `sb_state_changes`, `sb_market_results`) is append-only and RLS/privilege-checked as the runtime role | yes |

## 4. Evidence required per case

- Unit/adapter cases: test name, fixture citation (URL + retrieval date or sandbox capture id), `go test` output.
- DB cases: run as the NOBYPASSRLS runtime role; ledger invariants and the sportsbook settlement stream clean at the end.
- Sandbox cases: run log with message ids, redacted; no licensed official data redistributed in the repository.
- A skipped mandatory case is a failure. A case blocked by an open `U-SD-nn` is recorded `BLOCKED` with the item id.
