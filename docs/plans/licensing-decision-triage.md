# Licensing, jurisdiction, residence, market and brand-activation: decision triage (2026-10-19)

Read-only audit. Branch `licaudit3` from `claude/licensing-jurisdiction-completion`. No code, migration, ADR, governance file or HANDOVER was edited. Nothing here is a legal opinion, a licence/jurisdiction approval, or a launch authorisation. No jurisdiction other than what the register records is assumed approved.

## 0. Method and limits

- Read: `CLAUDE.md`, `docs/governance/human-decision-register.md`, `task-registry.md` (rows MARKET-EVIDENCE-KYC-RESIDENCE, F5-F6, A8 R1-R3, ADR0112-LAUNCH-AUTHORISATION, later rows), `docs/plans/final-platform-closure-report-2026-10-17.md` s4, `final-closure-list-2026-10-17.md`, ADR 0006, 0041-0047, 0083 (s8.2), 0096, 0112, 0115, `stage-4i-exit-register.md`, `jur-record.md`, `a6-rg-enforcement-record.md`, `a7-aml-record.md`, `a8-market-blocking-record.md`, `market-evidence-kyc-record.md`, runbooks (`launch-and-suspension`, `market-blocking-configuration`, `rg-enforcement-activation`, `aml-officer-bootstrap`), and the code cited per row.
- Run: `go test -count=1 ./internal/launchgov ./internal/marketgate ./internal/jurisdiction` (unit only): all `ok`. **Not run:** any `-tags integration` test (no private DB harness used). Every "tested" claim about DB behaviour below rests on reading the test and migration, not on running them. The Back Office was read (grep of `backoffice/src`), not run.
- Where a claim rests only on a document it is marked `(doc only)`.
- Labels: IMPLEMENTED-VERIFIED (code + migration/test read), IMPLEMENTED-UNTESTED, DB-FIELD-ONLY, API-ONLY, UI-ONLY, MOCK, DOC-ONLY, NOT IMPLEMENTED.
- CLASS: A provider-independent implementation required now; B human legal/compliance decision; C business evidence/configuration; D provider-dependent; E AWS-dependent; F implemented and verified.

## 1. Headline findings (things the previous summaries do not say plainly)

1. **Activation is not blocked by missing licence evidence, by design.** Only hard preconditions H-1..H-3, H-6 refuse (`internal/launchgov/hard.go:150-245`). Every readiness check (`R-LIC-VALID`, `R-MARKET-CONFIG`, `R-RG-CONFIG`, `R-AML-*`, `R-PROVIDER-TIER`, `R-ALERTS`...) is acknowledgeable: a non-pass must be acknowledged, not fixed (`internal/launchgov/request.go:144`, `execute.go:105-112`). `R-AML-MONITORING` always fails (`readiness.go:774`). Owner decision 15 (register, 2026-10-15) makes legal/licensing a manual per-brand decision. So "fail closed" for a brand means: (a) four-eyes human decision with acknowledgements, plus (b) runtime gates that refuse money/game actions when config is absent (below). Software does not stop an approver acknowledging an expired or absent licence.
2. **Three configuration tables the activation path needs have NO sanctioned production writer** (new finding, rows L-24, L-25, L-22): `tenant_jurisdiction_configs` (hard precondition H-3 and R-JUR-CONFIG, R-RULESETS, R-CURRENCIES, R-PAY-METHODS read it), the operating-market rows (`licence_country_ceilings`, `operating_country_policies`), and `licences.status/issued_at/expires_at`. The runbook `launch-and-suspension.md:60` says "partner console / API; nothing here is code" for the jurisdiction configs, but no route, library writer, seed or runbook step exists (`grep tenant_jurisdiction_configs` over Go/SQL/sh: only migrations, readers in `launchgov`, `withdrawal/policy.go`, tests). `cmd/seed-market` and `cmd/seed-staging-world` refuse production (`cmd/seed-market/main.go:21-29`). Until closed, the first brand cannot be made activatable in production without table-owner SQL or an ad-hoc Go call.
3. **Production market evidence is currently unobtainable for any player**: the gate's only production evidence is KYC-verified residence (`internal/marketgate/kycresidence.go`, `internal/kyc/market_evidence.go:50`), and there is no production KYC vendor (record s3, `PROVIDER DEPENDENT/BLOCKED`). So deposit, casino launch and sportsbook placement refuse every production player today. Safe, but it makes market config untestable end-to-end in production.
4. **KYC enforcement thresholds are dormant by default and have no route**: `not_required` allows when no policy row exists (ADR 0096 s3.2, lines 474-488, 554-557); `CreateEnforcementPolicy/Activate*` are library-only (`internal/kyc/enforcement_admin.go:57,141`; no handler). Withdrawal is not market-gated (`market-blocking-configuration.md:1`, "Not gated by design").
5. **The brief's "HDR-A6-3 (RG jurisdiction source licence-vs-market)" is not what the register's HDR-A6-3 says.** The register's HDR-A6-3 is "stuck deposit intents block deposits" (register line 141, decided 2026-10-16 item 4). The licence-vs-market RG question is the "Architect question (open, cross-domain)" in `a6-rg-enforcement-record.md:425` (SR6R-1). Both are covered below (L-17, L-19).
6. **MKT-DUAL-1 trigger (b) already fired.** The exit register (`stage-4i-exit-register.md` s6) names "wiring `ResolveOperatingCountryPolicy` into any consuming domain" as a trigger requiring dual control first. A8 did that wiring (`internal/marketgate/open.go:32`), and MKT-DUAL-1/HDR-M-1 remain OPEN with only a "two humans by process" interim (`market-blocking-configuration.md` s2).
7. **Registry/doc staleness found:** register lines 139-142 (HDR-A6-1..4) still read OPEN although the 2026-10-16 owner row (line 143) accepted them except floor values. `final-closure-list` IF-4 (`TODO(jurisdiction)`, "in flight") is closed: zero `TODO(jurisdiction)` markers remain in Go (grep), resolved to design notes and pin tests per `jur-record.md`; the player-scoped resolver wiring itself stays `NOT IMPLEMENTED` (L-4).

## 2. Table 1: item-by-item triage

Columns: ID | Exact current text / status (cite) | What the code does today (cite) | CLASS | SAFE INTERIM (what fails closed) | Decision request (B/C) or minimal scope (A).

### 2.1 Player jurisdiction and residence

**L-1 HDR-J-1: fallback to tenant/brand jurisdiction.**
- Status: ADR 0042 Part 1: "No fallback, ever." Unresolved player jurisdiction stays unresolved; tenant/brand jurisdiction never substituted. Legal review flagged Yes. Register line 90 lists residuals only.
- Code: `jurisdiction.Resolve` returns `unresolved(no_signal)` for every player-scoped call and never selects `platform_fallback`/`staff_supplied` (`internal/jurisdiction/resolver.go:25-71`, `:176-202`); `TestResolve_PlayerScopedOperationAlwaysUnresolvedNoSignal` and `TestResolve_SecondJurisdictionIsConfigurationOnly` (`jur-record.md` s3). Label IMPLEMENTED-VERIFIED (unit + integration tests read, not run).
- CLASS: F (decision answered and enforced). Wiring of a player-scoped resolver is row L-4.
- Safe interim: every jurisdiction-dependent player operation with a jurisdiction-scoped rule/blocklist/asset authorisation fails closed: casino launch `jurisdiction_unresolved` (`internal/casino/orchestrator.go:964-972`, `types.go:132`), risk `ErrMissingJurisdiction` (`internal/risk/evaluator.go:109,233`), bonus eligibility denial (`internal/bonus/eligibility.go:177`), sportsbook (`internal/sportsbook/jurisdiction.go:106`), withdrawal policies pinned jurisdiction NULL by CHECK (`migrations/0033`:79).
- Decision: none.

**L-2 HDR-J-2: precedence when signals disagree.**
- Status: ADR 0042: operation-specific; verified residence authoritative for identity/KYC; physical location may only add restriction; more restrictive prevails; historical determinations preserved. "Versioned, auditable... subject to legal/compliance validation for each operating jurisdiction."
- Code: pure function `DeterminePlayerJurisdiction` (`internal/jurisdiction/precedence.go:100`) with unit tests; **zero production callers** (grep outside the package and tests is empty). The market gate does NOT use it; it reads KYC-verified residence directly (`internal/kyc/market_evidence.go:50`). Label: library IMPLEMENTED-VERIFIED (unit), not wired = NOT IMPLEMENTED as a product behaviour.
- CLASS: B (per-jurisdiction legal validation of the precedence content) then A (wiring, row L-4).
- Safe interim: precedence function unreachable; market gate uses residence only and fails closed without it.
- Decision request: see L-7/L-8/L-9 (same wiring phase). No separate question.

**L-3 HDR-J-3 (3a-3h): collection of residence/location/nationality.**
- Status: ADR 0042: location signal yes (point-in-time, not persisted); declared residence yes (brand account); verified residence yes (reviewer only); nationality NO; lawful basis and retention "must be validated by qualified counsel before production use"; retention by data type/jurisdiction; KYC issuing country corroboration only.
- Code: declared residence player route (`internal/httpserver/player_residence_handlers.go`, player-only), per-tenant default-OFF collection switch (`internal/jurisdiction/evidence_collection_active.go`; `PUT /v1/admin/jurisdiction-evidence-collection/{evidenceType}` critical, golden table line 83), verified residence recorded through `POST /v1/admin/kyc/verifications/{id}/review` with step-up `critical` when the body carries a residence (`internal/httpserver/staff_stepup.go:350-363`). No nationality column/route (grep). No location capture (no vendor). Label: IMPLEMENTED-VERIFIED for 3b/3c mechanism (read); NOT IMPLEMENTED for 3a; 3e/3f legal content DOC-ONLY.
- CLASS: B (3e lawful basis/privacy notice; 3f retention periods; 3h permitted corroborating document types) ; D for 3a.
- Safe interim: collection OFF per tenant until a platform/tenant staff member with permission switches it on; without a recorded verified residence the market gate refuses.
- Decision request (one question): "For the first brand, what lawful basis, privacy notice wording and retention period apply to declared and verified residence, and which KYC document types may corroborate it?" Options: (a) counsel supplies the three items per jurisdiction before the switch is turned on; (b) collect verified residence only (leave declared residence OFF). Recommendation: (b) for the first brand, since only verified residence feeds the market gate. Minimum information: privacy-counsel sign-off reference, retention schedule by data category, permitted document types.

**L-4 Player-scoped jurisdiction resolver wiring (closure report s4 E first sentence; jur-record s5).**
- Status: `NOT IMPLEMENTED, owner/legal dependent (HDR-J-1, HDR-J-3)` (`jur-record.md` s5). Closure report s4: "until then: fail closed when a jurisdiction-scoped rule exists".
- Code: see L-1/L-2; `resolver.go:176-202` returns no_signal; `licence_validity.go` header notes the resolver does not call licence validity.
- CLASS: B (J-7/8/9 and J-3 content) then A.
- Safe interim: as L-1. Single-jurisdiction first brand is unaffected as long as no game has a non-empty jurisdiction blocklist, no jurisdiction-scoped risk rule and no jurisdiction-restricted bonus asset exists (a configuration choice, not enforced by code): any such config makes launch/bet/bonus refuse for all real players.
- Minimal scope when unblocked (A): in `Resolve`, for player-scoped params call `DeterminePlayerJurisdiction` with the evidence accessors (`evidence.go`), the activated `EvaluationPolicy` (`evaluation_policy.go:135`) and `RequiredPurposes` (`operation_purpose.go:80`); keep `unresolved`/`refused` split; no tenant fallback; tests that every caller still fails closed on unresolved. No migration expected (tables exist: migrations 0071-0075).

**L-5 HDR-J-4: record of authority for in-flight obligations after a jurisdiction change.**
- Status: ADR 0042: determination applicable at event time is authoritative; later one kept as context; no rewriting; SAR/data-residency need legal review.
- Code: casino sessions persist the jurisdiction code at launch and `postBet` reuses it (`jur-record.md` s1); no cross-change reconciliation logic found. Label: partial (session pin only).
- CLASS: B (legal review of SAR/data residency) - no engineering action until a second jurisdiction exists.
- Safe interim: single jurisdiction; no change event can occur through any route (no licence/jurisdiction change writer, see L-22).
- Decision request: deferred; ask only when a second jurisdiction or a licence change is scheduled.

**L-6 HDR-J-5: BYOL tenant jurisdiction authority.**
- Status: ADR 0042: platform resolution authoritative for players; tenant licensed jurisdiction authoritative only for the tenant's licensing context; "Final BYOL policy requires legal/compliance validation before the first BYOL tenant is onboarded."
- Code: tenant-subject resolution only (`resolver.go:60-71,150-200`), licence bind by platform admin only, DB composite FK `tenants_licence_matches_model` (migration 0007) and RLS (0077); `PUT /v1/admin/tenants/{tenantID}/licence` critical (golden line 157).
- CLASS: F for the mechanism; B for the pre-onboarding legal validation (table 3).
- Safe interim: no BYOL tenant exists in the registry docs; a tenant cannot bind another tenant's licence (0077).
- Decision: see table 3.

**L-7 HDR-J-7: which jurisdiction determination each operation class requires.**
- Status: `docs/decisions/0044...md:21`; register line 90 and `stage-4i-exit-register.md:518`: OPEN, "Before any code branches on OperationClass to select a Purpose".
- Code: `RequiredPurposes` (`operation_purpose.go:80`) is a seam with unit tests; no production caller.
- CLASS: B.
- Safe interim: no code branches on it; player-scoped = unresolved (L-1).
- Decision request: "For each operation class (registration, deposit, play, withdrawal, bonus, reporting), which determination is required: identity (verified residence), market-access (residence plus location), or both?" Options: per ADR 0044 s7: (a) identity only, (b) market-access only, (c) both. Recommendation: none from engineering; counsel decides per licensing jurisdiction. Minimum information: licence conditions of the first jurisdiction on residence vs location per operation. Needed only before L-4 wiring.

**L-8 HDR-J-8: physical-location signal required / advisory / undecided per jurisdiction and operation.** Same as SRME-4 (L-12), plus `evaluation_policy_admin.go` mechanism. Code: `ResolveEvaluationPolicy` (`evaluation_policy.go:135`) and admin library exist, no route (golden: `staffRoutesClassifiedOnArrival` line 252 "jurisdiction evaluation-policy writes"), default OFF. CLASS: B (+ D for any vendor). Safe interim: no location evidence; residence-only gate. Request: see L-12.

**L-9 HDR-J-9: maximum age of a location signal.** Depends on L-8 answer being "required" or "advisory". CLASS: B, conditional. Code: same as L-8. Request: ask only if L-8 = required/advisory; one value per jurisdiction/operation; minimum info = licence condition text.

**L-10 HDR-M-1 and MKT-DUAL-1: governance model for tenant country enablement.**
- Status: exit register s6: "Classification C. HUMAN DECISION REQUIRED"; register line 90 OPEN. Runbook: "two humans by process", OWNER decision.
- Code: `operatingmarket.CreateLicenceCountryCeilingVersion` / `CreateOperatingCountryPolicyVersion` require a non-empty `AuthorizationReference` to enable, none to disable (migration 0076 and `internal/operatingmarket/policy_admin.go`, `ceiling_admin.go`); append-only versions; no HTTP route, no console, no four-eyes. Label: library IMPLEMENTED-VERIFIED (tests read), governance NOT IMPLEMENTED.
- CLASS: B then A (row L-25).
- Safe interim: empty tables refuse everything ("designed state", runbook s1); disabling is immediate and needs no authorisation reference (emergency stop).
- Decision request (one question): "Who must approve enabling a country for a brand?" Options: (a) four-eyes inside the platform (platform admin requests, different Person approves, signed actor proof, like `launchgov`); (b) (a) plus compliance-role sign-off recorded as an evidence reference; (c) keep out-of-band process with a ticket. Recommendation: (a), reusing the ADR 0112 pattern; widening only, disabling stays single-actor. Minimum info: roles that hold approval authority, whether the authorisation reference must be verifiable.

**L-11 HDR-M-2: players after a country is disabled for their tenant.**
- Status: exit register line 522 OPEN; "same wiring phase as HDR-M-1".
- Code: effect is on the next request: registration/deposit/launch/sportsbook refused; casino new bets refused via `BrandOpen` (`internal/casino/market_exposure.go:83`) once the brand has no open country, and for open sessions only until `CASINO_SESSION_MAX_LIFETIME` (default 4h) (`orchestrator.go:1817`); wins/rollbacks/settlements/withdrawals never blocked (runbook s5). Note `BrandOpen` is brand-level not per-player (record s7 item 6).
- CLASS: B.
- Safe interim: players keep access to their funds (withdrawal not gated), cannot add money or start play.
- Decision request: "After a country closes, may existing players withdraw?" Options: (a) yes always (as built); (b) yes after a per-jurisdiction hold; (c) no. Recommendation: (a) as built unless counsel says otherwise. Minimum info: licence conditions on exit of excluded-market players.

### 2.2 Market evidence and gating

**L-12 SRME-4: residence is not physical location.**
- Status: closure list E-1; task-registry row MARKET-EVIDENCE-KYC-RESIDENCE: "owner+legal: is residence-only evidence enough for Anjouan? else vendor geolocation is a launch prerequisite."
- Code: gate reads KYC-verified residence only; request IP ignored (`marketgate.go` Locate; `kycresidence.go`); no geolocation vendor exists (not invented).
- CLASS: B (legal), D if the answer is "location required".
- Safe interim: no residence evidence = refuse (`MKT_EVIDENCE_UNAVAILABLE`, `marketgate.go:337-348`); one identical 403 body for all causes.
- Decision request: "Does the Anjouan licence (and each later licence) require blocking by physical location, or is verified residence sufficient?" Options: (a) residence sufficient; (b) location required, so a geolocation vendor becomes a launch prerequisite (D); (c) location advisory, add a restriction only. Recommendation: none without counsel; engineering note that (b)/(c) need a vendor not yet selected. Minimum info: written licence condition text or counsel opinion reference.

**L-13 SRME-5 / MKT-WIRE-CREDIT-1: deposit credits and open-session bet callbacks not gated per player.**
- Status: closure list E-2; record s10: "Decide, before real-money production, whether open game sessions are revoked (or lifetime bounded) when a player's evidence is withdrawn, and the hold/refund policy for credits of a player who lost evidence. ledger-finance + owner."
- Code: deposit gate at initiation only (`internal/httpserver/deposit_handlers.go:197`, `internal/payments/deposit_v2.go:109,147`); credits/PSP callbacks/sweeper not gated; casino bets gated brand-level plus session lifetime cap (`orchestrator.go:1817`, `market_exposure.go:83-88`). Label: partial, residual accepted-by-doc only.
- CLASS: B (policy) then A (implementation).
- Safe interim: exposure bounded by session lifetime (default 4h, range 1m-72h) and short deposit transactions; settlements never blocked.
- Decision request: "When a player's residence evidence is withdrawn mid-session or mid-deposit, what happens?" Options: (a) accept bounded exposure as built; (b) revoke open casino sessions on evidence withdrawal; (c) (b) plus hold deposit credit for review. Recommendation: (a) for the first brand with the 4h cap, revisit when real PSP exists. Minimum info: acceptable maximum exposure window.
- Minimal scope if (b): session revocation on evidence change (`casino_launch_sessions` revoke path exists, migration 0108) behind the evidence read; if (c): credit-time gate in the sweeper with a held state (ledger-finance review).

**L-14 SRME-6 / PD-MKT-REG-1: registration for excluded-market visitors.**
- Status: record s2.2, s7 item 5: "OWNER decision to confirm or reverse"; closure list E-3.
- Code: registration allowed iff the brand's market config permits registration for at least one country (`internal/httpserver/auth_routes.go:127`, `marketgate.Check` line 320, `AnyCountryPermits` `open.go:32`); a registered account cannot deposit/launch/bet (HTTP test `market_gate_kyc_residence_integration_test.go`, read).
- CLASS: B.
- Safe interim: unconfigured or closed brand refuses registration; registered-but-unverified accounts cannot reach money or games.
- Decision request: "May a visitor from an excluded market create an unverified account?" Options: (a) yes (as built); (b) require a declared country at sign-up (courtesy filter, API change); (c) disable registration per brand until counsel confirms. Recommendation: (a) with a legal confirmation, else (c) (config-only). Minimum info: licence terms on account opening.

**L-15 SRME-1 maker-checker for residence determinations.**
- Status: closure list E-4; registry: "SRME-1 maker-checker ... still open".
- Code: one staff reviewer with `PermVerificationReview` records `verified_residence_country`; step-up `critical` when present (`staff_stepup.go:350`); durable p2 alert `kyc.residence_into_open_market` raised in the review transaction (`internal/httpserver/kyc_admin_handlers.go:321`, migration 0170); no second approver. Label: IMPLEMENTED-VERIFIED for step-up + alert (read); maker-checker NOT IMPLEMENTED.
- CLASS: B then A.
- Safe interim: alert exists but delivery to a person depends on a configured p2 route (ALERT-DELIVERY-1 OPEN); a colluding reviewer can admit a player.
- Decision request: "Must a second person approve a residence determination before it becomes market evidence?" Options: (a) no, step-up plus alert is enough; (b) yes for determinations into an open market; (c) yes for all. Recommendation: (b). Minimum info: compliance staffing (is a second reviewer available).
- Minimal scope if (b)/(c): a pending/approved state on the determination with a different-Person approval and actor proof; evidence read only counts approved; migration plus `ReadMarketResidenceEvidence` change plus tests.

**L-16 D-MKT-EVIDENCE (register line 143 item 3) and closed SRME items.**
- Status: DECIDED 2026-10-16: "market evidence = KYC-verified residence as the current source, vendor geolocation later, no invented provider, registration/deposit/launch stay fail-closed without evidence."
- Code: matches (L-12 code); SRME-2/3/7/8, R2-1 closed with tests (record s11, registry F5-F6 row); startup guards `requireMarketEvidenceInProduction` / `requireMarketEvidenceBound` (`cmd/platform-api/registrations.go:537,556`).
- CLASS: F. Open residuals SRF5-1/3 and alert severity/volume are owner items (closure list E-5): CLASS B (low), question: "Is p2 right for `kyc.residence_into_open_market`, and who receives it?" Recommendation: keep p2, route to compliance; depends on ALERT-DELIVERY-1.

### 2.3 Responsible gaming, KYC, AML (jurisdiction-sourced)

**L-17 HDR-A6-1..4 (register lines 139-142; decided 2026-10-16 item 4).**
- Status: register line 143: "HDR-A6-1 four interpretations accepted as recorded; first brand wager/loss windows <= 7 days; stuck deposit may block up to 31 days; ... compliance MUST supply floor values". The rows on lines 139-142 still read OPEN (stale text; later row supersedes). HDR-A6-3 (stuck intents) and HDR-A6-2 (monthly windows) are therefore decided for the first brand; HDR-A6-2 says "If compliance rules that monthly wager or loss limits are legally required... the rollup becomes a launch prerequisite".
- Code: not re-verified line by line (doc only); enforcement switch is one-way and refused unless floors exist (`internal/rg/enforcement_activation.go:82`, `launchgov.RGActivationReadiness` `readiness.go:830-878`).
- CLASS: B (conditional question to compliance: monthly wager/loss limit legally required for the first jurisdiction?), A only if answered yes (ledger-derived rollup).
- Safe interim: RG enforcement default OFF; with the switch ON and no floor every deposit/launch/wager is refused `rg_config_missing` (`rg-enforcement-activation.md` s1).
- Decision request: "Does the first jurisdiction legally require monthly wager or loss limits?" Options: yes (schedule rollup before launch) / no (keep <= 7-day windows). Minimum info: licence condition or counsel statement.

**L-18 OQ-2: RG floor values.**
- Status: ADR 0115 s9: "compliance supplies the Anjouan values... Engineering supplies no values. Until then, Anjouan is `rg_config_missing`."; closure list E-7; register line 143 "compliance MUST supply floor values... no invented regulatory values".
- Code: floor writes via `POST /v1/admin/platform/rg/limit-config-change-requests` (platform, critical, four-eyes: different Person approves) (golden lines 101-105; runbook s3 step 1); API-ONLY as far as read (Back Office has an `rg` feature folder; whether it includes floor change requests was NOT verified).
- CLASS: C (business/compliance evidence) + B (what values).
- Safe interim: no floor = RG-gated actions refused when switch ON; `R-RG-CONFIG` fails with `floor_missing_<kind>`; but activation can still be approved with that failure acknowledged (headline 1). Note: with the RG switch OFF (default) and `R-RG-CONFIG` acknowledged, a brand can be active with NO deposit/loss/wager limits enforced, contrary to owner decision 4 (limits mandatory). Software does not block that combination.
- Decision request (one question): "Supply, per limit kind (deposit, loss, wager), period and allowed currency, the floor/ceiling values (or attested `none`) and the named signatory for the first licence jurisdiction." Options: values / attested none per cell. Recommendation: none; engineering supplies no value. Minimum info: table of kind x period x currency, attestation reference, effective date.
- Possible A item (owner approval needed to add): make `R-RG-CONFIG` non-acknowledgeable for brands whose first-brand rule says limits are mandatory. Ask: "Should activation of a real-money brand be refused (not just warned) while RG enforcement is OFF?" Recommendation: yes, a hard precondition H-7, since owner decision 4 says mandatory.

**L-19 RG jurisdiction source: licence vs market (A6 record s SR6R-1, line 425).**
- Status: "Architect question (open, cross-domain): Under the hybrid licensing model (ADR 0006), which jurisdiction governs RG limits: the licence jurisdiction or the market jurisdiction of the player or brand? A6 takes the strict reading (floors needed in both)."
- Code: enforcement reads the licence jurisdiction only (`internal/rg/limits_service.go:270-295`); activation guard and `R-RG-CONFIG` require floors in the licence jurisdiction AND every in-effect config jurisdiction (`readiness.go:635-676,830-878`); tenant with no licence jurisdiction: `no_licence_jurisdiction` / enforcement refuses.
- CLASS: B (architect + compliance), no urgency for a single-jurisdiction first brand.
- Safe interim: strict reading cannot let a halt through; a tenant without a licence has every RG-gated action refused.
- Decision request: "Which jurisdiction's RG floors apply to a player: the licence's or the player's market?" Options: (a) licence jurisdiction only (as enforced); (b) market/residence jurisdiction of the player (needs L-4); (c) stricter of both. Recommendation: (a) for the first brand (single jurisdiction, no player-scoped resolver); decide (c) vs (b) before a second jurisdiction or a B2B tenant. Minimum info: whether the first licence conditions import player-residence RG rules.

**L-20 HD-KYC-1..8 (ADR 0096 s4) and KYC policy authoring.**
- Status: register line 88 OPEN; closure list E-6; default `not_required` = allow (ADR 0096 lines 474-488, 554).
- Code: `kyc.EvaluateEnforcement` fail-closed on lookup error (`unavailable`), allow when no policy; policy admin is library-only (`internal/kyc/enforcement_admin.go:57,141`), no route (grep: no handler); deposit path blocked in practice by the market gate needing an approved KYC verification.
- CLASS: B (thresholds/trigger types per jurisdiction) then A (authoring route, four-eyes already modelled in the library comment `enforcement_admin.go:44`).
- Safe interim: deposit/launch/sportsbook need an approved KYC verification (market evidence). Withdrawal and casino stake are NOT KYC-gated by threshold until policy rows exist.
- Decision request: "For the first jurisdiction, at what thresholds/triggers must KYC be passed before play and before withdrawal?" Options: per ADR 0096 s4 HD-KYC-1..8 items. Recommendation: at minimum require passed KYC before first withdrawal (HD-KYC row on first-withdrawal rule; ADR 0096 line 544 says it carries no safe default value, so a human value is required). Minimum info: licence KYC conditions, thresholds with currency.

**L-21 AML items touching licence (HD-AML-1..9, D-AML-TIP-1 wording, AML-LAUNCH-CRITERION-1).**
- Status: `a7-aml-record.md:133`: "monitoring is a standing failing item; enforcement of Q1 (blocking) is not built"; D-AML-TIP-1 decided (c) now, legal wording pending (`aml-adjustment-approver-confidentiality.md`); closure list E-11, B-21 "Named gate: before any `under_platform_licence` tenant launches".
- Code: `R-AML-QUORUM` (two distinct live officer Persons, `readiness.go:774-790`), `R-AML-SCREENING` (provider registered, MOCK allowed outside production, policy per referenced jurisdiction, `amlScreeningCheck`), `R-AML-MONITORING` always `fail`. Officer grant has no dedicated route (runbook `aml-officer-bootstrap.md` header).
- CLASS: B (legal wording; launch criterion), D (screening vendor), A for none.
- Safe interim: holds fail closed; activation requires explicit acknowledgement of `monitoring_not_implemented`.
- Decision request: "May a real-money brand activate while transaction monitoring is NOT IMPLEMENTED?" Options: (a) no (make `R-AML-MONITORING` blocking); (b) yes with an interim manual monitoring procedure recorded as a determination reference. Recommendation: (b) only with counsel sign-off recorded; else (a). Minimum info: AML programme document reference, named AML officer (two Persons).

### 2.4 Licence registry, expiry, activation

**L-22 / MKT-LICSTATUS-1 + MKT-EXPIRY-1 + licence expiry handling.**
- Status: exit register s3 and s7: no write path for `licences.status`; `issued_at`/`expires_at` have no writer; half-open `[issued_at, expires_at)` is "a disclosed engineering default, not a confirmed legal determination"; trigger "the first real `licence_country_ceilings` row" (= the first brand).
- Code: `POST /v1/admin/licences` creates `active` rows only with licensee, number, permitted products/markets (`internal/jurisdiction/registry_admin.go:311-349`, handler fields `jurisdiction_admin_handlers.go:180-186`); no `UPDATE licences` in non-test Go (grep). `EvaluateLicenceValidity` (`licence_validity.go:68-110`) is consumed by the market resolver (`operatingmarket/resolve.go:259-275`: suspended/expired => `not_permitted_by_licence`; unbound/not found => `licensing_unknown`, closed) and by `R-LIC-VALID` (`readiness.go:399-413`). NULL `issued_at` falls through (not "not yet issued"). No scheduled expiry check, no alert on approaching/passed expiry (grep `licence.*expir` in non-test Go outside the resolver/launchgov: none). The licence row has no evidence/document reference column (`migrations/0002`:15-26).
- CLASS: A (write path and expiry alert) + B (boundary confirmation, MKT-EXPIRY-1).
- Safe interim: an expired/suspended licence (only settable by table-owner SQL today) closes the market gate for registration/deposit/launch/sportsbook/casino new bets; brand status stays `active`; withdrawals and settlements continue. No automatic suspension of the brand. A launch request goes stale if the licence expires between approval and execution (readiness hash recomputed at execution, `execute.go:105-112`).
- Minimal scope (A): (1) governed write for licence `status`, `issued_at`, `expires_at`, plus an `evidence_ref` text column (reference only, no document), platform admin, step-up critical, audit, four-eyes for reinstatement, tests; (2) a scheduled read that raises a p1/p2 alert N days before `expires_at` and on expiry for licences bound to non-closed tenants; (3) do NOT auto-suspend (keep suspension a human act).
- Decision request (B): "Is a licence valid on its stated expiry date, and on its issue date?" Options: (a) invalid on expiry date, valid on issue date (as built, conservative); (b) valid through the expiry date. Recommendation: (a) as built, no grace period invented. Minimum info: licence terms or counsel confirmation. Also: "How many days before expiry should an alert fire, and to whom?" (C).

**L-23 MKT-AUDIT-1, MKT-DORMANT-1, MKT-PM-1 (exit register s4, s5, s8).**
- Status: deferred, non-blocking. Code: licence assignment audit is platform-scope only (`tenant_licence_admin.go` header); `licences.permitted_markets` still written by `CreateLicence` but not read by the gate (exit register s8). CLASS: B/low, fold into L-22 scope; no decision needed now.

**L-24 `tenant_jurisdiction_configs` has no sanctioned writer (NEW).**
- Status: hard precondition H-3 and checks R-JUR-CONFIG/R-RULESETS/R-CURRENCIES/R-PAY-METHODS depend on it (`hard.go:219-243`, `readiness.go:151-300`); runbook `launch-and-suspension.md:60` says "partner console / API".
- Code: table defined `migrations/0002:37-49`; no INSERT/UPDATE in non-test Go, no route in golden table, no seed. Ruleset ids (`kyc_ruleset_id`...) are opaque text; `R-RULESETS` only proves non-empty (`readiness.go` rulesets case), not that a ruleset exists. Label: DB-FIELD-ONLY (readers exist, no writer).
- CLASS: A (writer) + C (content: ruleset ids, currencies, payment methods).
- Safe interim: no row = H-3 refuses activation (`H-3:config_missing_or_not_in_effect`), so a brand cannot be activated without it.
- Minimal scope (A): platform-admin (or tenant compliance) versioned write `PUT /v1/admin/tenants/{tenantID}/jurisdiction-configs` (append new version, close old `effective_to`), step-up critical, audit, RLS-tested, with validation that currencies are active assets and jurisdiction exists; Back Office form optional. No legal values hard-coded.

**L-25 Operating-market rows have no governed production writer (NEW, with L-10).**
- Code/doc: `market-blocking-configuration.md` s2: "no HTTP route and no console... a reviewed Go call (a one-off operator tool or script...)"; `cmd/seed-market` refuses production (`main.go:21-29`); golden table line 252 lists the route class as "classified on arrival" (not built).
- CLASS: B (L-10) then A.
- Safe interim: empty tables = everything refused (designed state); `R-MARKET-CONFIG` shows `no_market_configured`.
- Minimal scope (A, after L-10 answered): routes for ceiling (platform admin tx) and tenant/brand/operation policy versions with `authorization_reference` (enable) and optional four-eyes; read routes already implicit via explain; tests that a tenant cannot exceed the ceiling (`ErrCeilingExceeded`) through HTTP.

**L-26 Licence and jurisdiction decisions beyond Anjouan (register line 95).**
- Status: "Anjouan only", OPEN, blocks production launch (owner + legal). HDR-J-6 (ADR 0042): "Not yet determined. The first B2C permitted-market list must be established through a jurisdiction-by-jurisdiction legal/compliance review before launch. No country is permitted by default... Any jurisdiction not on the approved list must fail closed."
- Code: no jurisdiction/licence/country is seeded in production migrations (grep `Anjouan|KM-ANJ`: migration comments only); markets fail closed when absent.
- CLASS: B + C.
- Safe interim: empty tables refuse all.
- Decision request (HDR-J-6): "List the countries (ISO-3166-1 alpha-2), per product (casino, sportsbook) and operation (registration, deposit, wagering), that the first brand may serve, with the legal-review reference per country." Options: explicit enumerated list; start with a single country. Recommendation: start with the smallest list counsel has approved in writing. Minimum info: per country: counsel reference, licence ceiling confirmation, product scope, effective date.

**L-27 HDR-SB-1 and sportsbook jurisdiction gating (ADR 0083 s8.2).**
- Status: register line 91 OPEN; sportsbook deferred by owner (register 2026-10-15 item 2).
- Code: sportsbook jurisdiction restrictions routes exist (`POST /v1/admin/sportsbook/jurisdiction-restrictions` critical, golden lines 130-132; `internal/sportsbook/jurisdiction.go:106`); sportsbook placement is market-gated (`httpserver/sportsbook_handlers.go:408`; SB-JUR-RUNG2-1 closed by A8 per closure list Appendix A); cumulative exposure ceiling not armed (ADR 0083 s8.2).
- CLASS: B (deferred; ask when a sportsbook brand is scheduled).
- Safe interim: sportsbook not scheduled; bets fail closed on market/jurisdiction.
- Decision request (deferred): "Who carries the sportsbook trading-book liability, and must an exposure ceiling exist before go-live?" (ADR 0083 s8.2.)

**L-28 `TODO(jurisdiction)` payments dimension-2 and jurisdiction-scoped withdrawal policy (jur-record s5).**
- Status: `NOT IMPLEMENTED, second-jurisdiction / PSP-licence-matrix dependent` (`jur-record.md` s5). Pins: `internal/payments/jurisdiction_dimension_pin_test.go`, withdrawal CHECK test.
- CLASS: A only when a second jurisdiction or a PSP licence matrix exists (not now). Safe interim: nothing jurisdiction-dependent exists to skip in payments; deposit market restriction is the A8 gate.

### 2.5 Brand activation governance

**L-29 ADR 0112 owner defaults (registry ADR0112-LAUNCH-AUTHORISATION-2026-10-11).**
- Status: "OWNER DEFAULTS (orchestrator, reversible by owner): brand 1 approver, tenant activation/closure 2, platform_licence brand requested by tenant staff 2; single-actor suspend". Register change log 2026-10-11: "taken by the orchestrator (reversible)". Not recorded as owner-confirmed.
- Code: forced in the DB trigger (`migrations/0128_launch_authorisation_status_governance.up.sql:365-372`); requester != approver by staff id AND Person (`approvability.go:81-105`); approvals max age 24 h, request TTL 72 h (`launchgov.go` constants).
- CLASS: B (owner confirmation).
- Safe interim: as built.
- Decision request: "Confirm or change: first-brand activation needs one approver besides the requester (two humans total); tenant activation needs two approvers." Recommendation: raise first-brand (real money, first licence) activation to two approvers; the DB trigger forces counts so a change is a migration. Minimum info: names/roles of the approvers.

**L-30 Licensing basis recorded at activation (ADR 0112 s4.1/5.2).**
- Code: four bases (`platform_licence`, `tenant_licence`, `other_manually_approved`, `not_applicable_recorded_determination`); `not_applicable` and `other_manually_approved` need `determination_reference` and `determination_by_role` (`hard.go:150-216`); `platform_licence`/`tenant_licence` need a `licence_id` consistent with `tenants.licensing_model` and bound `tenants.licence_id` (H-2); the references are free text, not verified. Label: IMPLEMENTED-VERIFIED (read; integration tests exist, not run).
- CLASS: F for mechanism; B/C for content (table 2).
- Safe interim: inconsistent record refused (hard); but a licence-backed record carries no evidence reference field at all (only `licence_number` on the licence and free-text `conditions_note` <= 2000 bytes, PII-linted).
- Decision request: "Must the activation request carry a verifiable evidence reference for a licence-backed basis (licence document id, regulator register URL, counsel opinion id)?" Options: (a) optional note (as built); (b) mandatory `determination_reference` for all four bases. Recommendation: (b), small change (A) once chosen.

**L-31 Excluded-by-design legal adjacent items (no engineering action): HQ-E1-1/3/4, HD-CTF-1..10, HD-RET-1, K3 note retention, Hosting AUP, HD-PRH-1.**
- Status: register lines 85-97 OPEN. CLASS: B. Safe interim: closure is refused while holds exist; closed tenant funds stay in the ledger (`readiness.go` notices); KYC outbox failure is terminal-alerted. Jurisdiction relevance: HQ-E1-3 "Jurisdiction deadlines for KYC" (needs a per-jurisdiction value, C). Not re-analysed; ask as part of the first-brand legal pack only.

**L-32 First-deposit slot policy (closure report s4 B/E "C-4").**
- Status: "Cancelled first-deposit grant keeps the slot... product decision (E)". Not jurisdiction-related: the slot is per player/offer (`migrations/0173`; `c1-record.md:46`). Jurisdiction relevance is only bonus eligibility unresolved => denied (`internal/bonus/eligibility.go:177`). CLASS: B (product), out of licensing scope. Safe interim: no double credit.

**L-33 D-REG-1 (payout instrument identifier submitted by a player).** Payments/PCI-PII, not licensing; blocks Option 2 sandbox payouts only (`psp/open-questions.md:11`). CLASS: B/D, out of licensing scope. Listed to close the question.

## 3. Table 2: brand activation preconditions, FIRST brand (own company, Anjouan licence, `under_platform_licence`)

Technical preconditions the platform enforces (all verified by reading code unless marked), and what the owner must supply. "Hard" = request refused; "Ack" = must be acknowledged, can be accepted; "Runtime" = enforced on every request after activation.

| # | Precondition | Enforcement | Cite | Owner / business must supply |
|---|---|---|---|---|
| 1 | Tenant and brand exist in `pending_launch`; only `launchgov` writes status | Hard (H-1, DB guard) | `hard.go:104-120`, migration 0128 | Tenant (licensing_model `under_platform_licence`), brand, slug, reason code |
| 2 | Jurisdiction row exists (code, name, regulatory body, `country_code`) | Prerequisite for H-3 and the licence | `POST /v1/admin/jurisdictions` (API-ONLY, critical step-up) | Jurisdiction code/name/regulator, written by a human; no value seeded |
| 3 | Licence row: jurisdiction, licensee `platform`, `licence_number`, status `active`, `issued_at`, `expires_at`, permitted products | Hard (H-2: record consistent with registry/tenant); validity is Ack (`R-LIC-VALID`) | `registry_admin.go:311-349`, `hard.go:150-216`, `readiness.go:399-413` | Licence number, issue/expiry dates, permitted products, evidence reference. Dates and status cannot be written through the app today (L-22); today only table-owner SQL |
| 4 | Licence bound to the tenant (`tenants.licence_id`) | Hard (H-2 tenant binding), DB composite FK | `PUT /v1/admin/tenants/{id}/licence`, migration 0007, 0077 | Decision which licence the tenant operates under |
| 5 | Licensing record on the request: model, status, licence id; for `other_manually_approved`/`not_applicable` a determination reference and role | Hard | `hard.go:150-216` | Determination reference (document id, not the document), responsible role (not a person), responsible operator name and registration number |
| 6 | At least one jurisdiction referenced with `tenant_jurisdiction_configs` row in effect | Hard (H-3), `R-JUR-CONFIG` | `hard.go:219`; **no writer exists (L-24)** | Ruleset ids for KYC/AML/RG, allowed currencies, allowed payment methods per jurisdiction |
| 7 | Rulesets set; currencies active assets; payment methods plus active payments credential handle | Ack (`R-RULESETS`, `R-CURRENCIES`, `R-PAY-METHODS`) | `readiness.go:200-300` | As row 6; PSP credential handle (PROVIDER DEPENDENT; all providers MOCK) |
| 8 | Catalogue: at least one enabled active casino game | Ack (`R-CATALOGUE`) | `readiness.go` catalogue block | Game availability rows (provider contract) |
| 9 | Operating market open for registration, deposit and casino wagering in at least one country (ceiling on the licence + tenant opt-in, no narrowing) | Ack (`R-MARKET-CONFIG`); Runtime: every registration/deposit/launch/sportsbook bet/casino new bet is refused otherwise | `readiness.go:382-397`, `marketgate/marketgate.go:304`, `open.go:32`; **no governed writer (L-25)** | The permitted-country list with legal-review reference per country (HDR-J-6), authorisation reference per enabling row, market-rung rows |
| 10 | Verified-residence evidence collection switched on for the tenant | Runtime (reviewers cannot record residence otherwise; every player refused) | `PUT /v1/admin/jurisdiction-evidence-collection/{evidenceType}` | Lawful basis and privacy notice for residence (L-3) |
| 11 | Market evidence = KYC-verified residence; no evidence refuses | Runtime | `kycresidence.go`, `market_evidence.go:50`, startup guards `registrations.go:537,556` | A KYC process able to produce approved verifications (PROVIDER DEPENDENT: no production KYC vendor); compliance reviewers |
| 12 | RG: enforcement switch ON; floors for deposit, loss, wager in every allowed currency in the licence jurisdiction and every config jurisdiction; no session-duration control enabled | Ack (`R-RG-CONFIG`); switch refused unless floors exist; Runtime `rg_config_missing` once ON | `readiness.go:614-878`, `rg/enforcement_activation.go:82`, `limits_service.go:270` | OQ-2 floor values and signatory (L-18); owner choice to switch ON per brand; confirmation of L-17; note: software allows activation with the switch OFF if the check is acknowledged |
| 13 | AML: two distinct live `aml_officer` Persons in the owner scope (platform scope for platform licence); screening provider registered (MOCK allowed outside production) and screening policy per referenced jurisdiction; monitoring always Ack-fail | Ack (`R-AML-QUORUM`, `R-AML-SCREENING`, `R-AML-MONITORING`) | `readiness.go:774-830` | Two named AML officers, screening policy with legal-review reference, decision on launching without monitoring (L-21), approved confidentiality wording |
| 14 | Providers production-eligible | Ack (`R-PROVIDER-TIER`, fails everywhere today: all MOCK); production startup refuses synthetic components | `readiness.go:415-428` | Provider contracts (human); no real provider authorised |
| 15 | Alert routes: enabled, wired, human-notification route for p1 and p2 | Ack (`R-ALERTS`) | `readiness.go:447-480` | Recipients, on-call rota, channel (ALERT-DELIVERY-1 OPEN) |
| 16 | Four-eyes: requester != approver by staff and Person; approvals <= 24 h old; readiness hash unchanged since approval; signed actor proof | Hard (DB + service) | `approvability.go`, `execute.go:97-120`, migration 0128 `:365`, 0130 | Two distinct platform staff (platform scope; tenant scope cannot approve); approver identities |
| 17 | Operational: kill switch, backups, runtime-role split, environment anchor | Production startup guards (not launchgov) | `docs/runbooks/production-configuration-checklist.md` (doc only here) | AWS/staging items (class E, separate) |

Open owner/legal items blocking an informed activation decision for brand 1: L-26 (market list), L-12 (residence vs location), L-14, L-13, L-15, L-18 (floor values), L-20 (KYC thresholds), L-21 (AML monitoring and wording), L-22 (licence dates/evidence), L-29 (approver count). Production deposit/launch/placement additionally need a KYC vendor (D).

## 4. Table 3: hypothetical B2B tenant with its own licence in another jurisdiction (`own_licence`)

| # | Precondition | Enforcement | Cite | Owner / business / counsel must supply |
|---|---|---|---|---|
| 1 | Tenant created with `licensing_model = own_licence`; licence row with licensee `tenant`, in the tenant's own jurisdiction | DB composite FK `tenants_licence_matches_model` (migration 0007); platform admin only (RLS 0077) | `registry_admin.go:311`, `tenant_licence_admin.go` | A new `jurisdictions` row (code, regulator, `country_code`) and licence record (number, dates, evidence) per L-22; the tenant cannot write them |
| 2 | HDR-J-5: platform resolves player jurisdiction; tenant licence jurisdiction is licensing context only | Resolver (L-6) | `resolver.go:60-71` | **Counsel validation of BYOL policy before onboarding** (ADR 0042 HDR-J-5) |
| 3 | Same activation chain as Table 2 rows 1-16, per brand; tenant activation needs 2 approvals, tenant-scope staff can only request brand actions, never approve; a tenant admin can never self-approve | Hard | migration 0128 `:365-372`, `launchgov/service.go:91-120`, `approvability.go:57` | Platform approvers; tenant requesters |
| 4 | `tenant_jurisdiction_configs` for the tenant's jurisdiction (and any other served) | Hard (H-3) | `hard.go:219` (no writer: L-24) | The tenant jurisdiction's KYC/AML/RG ruleset ids, currencies, methods |
| 5 | Licence ceilings for the tenant's licence and per-country tenant opt-in | Runtime market gate; `licence_country_ceilings` keyed on the licence | `operatingmarket/resolve.go:259-275` | Countries the licence allows, counsel reference (L-26 shape per licence); governed writer missing (L-25) |
| 6 | RG floors in the tenant's LICENCE jurisdiction (not the platform's) | Runtime and activation guard (licence jurisdiction is the one read) | `limits_service.go:270-295` | Floors for that jurisdiction by compliance; L-19 becomes live (licence vs market) as soon as the tenant serves other markets |
| 7 | AML: owner scope is the tenant: two Persons with `aml_officer` capability among tenant `compliance` staff, grants approved by a distinct platform principal | Ack (`R-AML-QUORUM`), proof-bound grant flow | `aml-officer-bootstrap.md` owner-scope table | Tenant's named AML officers; screening policy for the tenant's jurisdiction |
| 8 | Provider credentials owned per tenant (own licence = tenant-owned credentials); `R-PAY-METHODS` requires an active handle | Ack | `readiness.go:240-275`; ADR 0006 consequence | Tenant's own PSP/casino contracts and credentials (D) |
| 9 | Isolation: tenant_id from server context, RLS; schema/db-per-tenant only if a partner/jurisdiction requires | Runtime | CLAUDE.md; register DEFERRED row | Whether the partner's jurisdiction mandates data residency (L-5, counsel) |
| 10 | Player residence/market evidence in the tenant's markets | Same KYC-residence source; per tenant evidence-collection switch | `kycresidence.go`; L-12 answer per licence | Whether location (not residence) is required by the tenant's regulator (L-8/L-12); a geolocation vendor if so (D) |
| 11 | Licence validity/expiry for the tenant's licence | Runtime market gate fails closed on suspended/expired; no alert or auto-suspend | `licence_validity.go:68-110` | L-22 write path and expiry alert; boundary confirmation under the tenant's jurisdiction |
| 12 | Registry open items specific to B2B | `B2B own-licence M4 self-service` (register change log 2026-10-09), `HDR-J-5` pre-onboarding validation, `Schema/database-per-tenant isolation` DEFERRED | register lines 109, 128 | Owner decisions listed there; not analysed further here |

## 5. Suggested ask list for the owner (minimal, ordered by what gates brand 1)

1. L-26 / HDR-J-6: the enumerated country list with legal-review references.
2. L-12 / SRME-4 and L-14 / SRME-6: licence conditions on residence vs location and on account opening.
3. L-18 / OQ-2 (+ L-17 monthly-limit question): RG floor values and signatory.
4. L-20: KYC thresholds including withdrawal.
5. L-21: AML monitoring-at-launch decision, named officers, wording.
6. L-22: licence record fields (issue/expiry/evidence) and boundary confirmation.
7. L-10 / L-15 / L-13 / L-29: governance choices (market enablement approvers, residence maker-checker, evidence-withdrawal policy, approver count).

Class A work the platform can start without any human answer: L-24 (jurisdiction-config writer), the licence status/date/evidence writer and expiry alert (L-22 (1)-(3), after only a boundary confirmation for L-22 (a)), and the optional hard-precondition proposal in L-18 if the owner approves it. L-25/L-15/L-13/L-4 wait for their B answer.
