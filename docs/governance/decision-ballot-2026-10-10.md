# Human decision ballot (2026-10-10)

Purpose: one concise ballot of the open questions that block (a) authorising the first sandbox PSP or (b) provider/non-MOCK
implementation, with the **exact source wording**, the current state, the existing recommendation, the owner, and a
classification. Prepared from the repository at `dc01aef`+ (ADR 0111 sections 10, 15, 16, 23-25; ADR 0095 s45-s48; the
decision register; `open-owner-questions-2026-10-09.md`). **No answer is inferred or decided here.**

Classification legend (per question; the strictest class is the one shown first):
**SANDBOX** = must be decided before the first sandbox PSP can move (synthetic-money) funds in the stated scope;
**NON-MOCK** = must be decided before any non-MOCK money movement with real customers/real money;
**PRODUCTION** = must be decided before production; **NOT A BLOCKER** = recorded, none of the above.

Sandbox scope assumed for the classification: ONE sandbox PSP, a synthetic non-real-money tenant, synthetic data only
(owner YES in principle, register `SANDBOX-BEFORE-ALERT-DELIVERY-1`). If the owner scopes the first sandbox to **deposits
only** (no payouts), items marked "SANDBOX (payout only)" drop to NON-MOCK.

## Already decided (do NOT ask again)
Q-R21-1 (RR-1 at destination_mismatch), NET recovery, instrument state does not gate settlement, destination_integrity_failure
four-eyes exit (M4 not-paid only), mandatory echo declaration, D-7 evidence standard, no raw player credential registration,
B14-B18 and AWS not authorised (ADR 0095 s48). Also resolved: HD-R15-8 (sealed imports), D-4 (A-12), HD-CTF-10 (read-only
credential where supported). Q-R32-1 and Q-R32-2 closed.

## A. Questions that block the FIRST SANDBOX PSP (decide these to authorise it)

| ID | Exact question (source) | State | Recommendation | Owner | Class |
|---|---|---|---|---|---|
| **HD-R15-1** | "Verification max-age / re-verification cadence per jurisdiction. **Launch-blocking** for any sandbox or real payout." (ADR 0111 s10.1, s10.3) | Fails closed: with no max-age config row a real verification is unusable (A-6); expiry/sweep implemented; MOCK uses a configured row in tests | none recorded beyond the ADR; jurisdictions are Anjouan (first tenant) then EU/LATAM | owner (+ compliance) | SANDBOX (payout only); NON-MOCK; PRODUCTION |
| **HD-R15-2** | "Further ownership assertions that count as 'verified' (card proven by own deposit, PSP rails, custodian attestation, e-wallet login)." (s10.1) | Only `account_holder_matches_verified_identity` + the Person's KYC verified counts for real sources (A-5). **Also needed, not a question:** a non-Synthetic verifier must exist to verify an instrument for a non-Synthetic adapter (tiering refuses `synthetic` for real adapters); only the MOCK verifier exists | keep A-5 for the sandbox | owner + identity-compliance | NOT A BLOCKER if A-5 is acceptable; SANDBOX (payout only) if the sandbox needs another assertion |
| **HD-R15-5** | "Disposition of an `approved` withdrawal whose instrument becomes permanently unusable: parked (current), four-eyes release, or four-eyes rebind. **Stranded funds: launch-blocking before any non-MOCK payout (LF L-7).**" (s10.1, s10.3) | Parked: request stays `approved`, every submit refused; T1p refusal writes a denial audit only | `ledger-finance`: stranded funds are launch-blocking; no option chosen. Note decision 4 (integrity exit) does NOT answer this | owner + ledger-finance + security | SANDBOX (payout only, literal reading of "any non-MOCK payout"); NON-MOCK; PRODUCTION |
| **D-REG-1** | "is a raw bank-account/IBAN or e-wallet identifier submitted to the platform API a 'raw sensitive payment credential' under decision 8? If YES, the raw-detail route should be disabled outside Synthetic environments now (small change, recommended); if NO, it may remain until the provider flow exists." (`player-instrument-registration-brief.md`) | The route `POST /v1/me/payout-instruments` accepts client-supplied `detail` (PAN refused); no registration UI exists | brief recommends disabling the raw route outside Synthetic environments if YES | owner + security | SANDBOX (payout only: decides how sandbox test instruments are registered); NON-MOCK; PRODUCTION |
| **`Unsupported` echo acknowledgement** | "Whether `Unsupported` is acceptable for a given provider is a per-provider OWNER ACKNOWLEDGEMENT, still to be defined." and "(security recommendation) refuse production startup for a non-Synthetic payout adapter that declares `Unsupported` unless an explicit per-provider acknowledgement is configured." (ADR 0111 s24.6 items 1, 7) | `Unsupported` is explicit, cannot be a default, logs a WARN at startup; compensating control = statement-level M4/reconciliation evidence | security: refuse production startup without an explicit acknowledgement | owner + security + ledger-finance | SANDBOX (conditional: only if the chosen provider cannot echo a destination); NON-MOCK; PRODUCTION |
| **Tenant visibility for the adapter echo (B13B-8)** | "A multi-tenant adapter still does not receive the tenant in `Withdraw` / `QueryStatus`, so a real `Supported` adapter cannot build the tenant-bound fingerprinter today." (s24.6 item 2) | interface gap; `Supported` exercised only by MOCK | none beyond architect ownership | architect | SANDBOX (conditional: only if the provider can echo) |
| **Sandbox authorisation itself** | "Start ONE sandbox-only PSP adapter (B14-B18) for a synthetic non-real-money tenant, with the provider named." (authorization request, this cycle) | not authorised | see `sandbox-psp-authorization-request-2026-10-10.md` | owner | SANDBOX |

## B. Questions that block NON-MOCK money movement (real customers/real money) but NOT the sandbox

| ID | Exact question (source) | State | Recommendation | Owner | Class |
|---|---|---|---|---|---|
| **M-3** | "A fingerprint-ownership claim is permanent (A-2: owner rows are never deleted) and there is no release path. A Person who registers a destination that is not theirs, or one they later lose, blocks every other Person in the tenant from it forever (a denial-of-registration vector) and nothing can correct a wrong claim. A governed (four-eyes, proof-bound) release or re-assignment writer is required before any real customer data." (s15.5) | pinned by tests as current behaviour | `security` options (none chosen): (i) claim ownership only at the first non-synthetic verification success; (ii) ignore owners whose instruments for that fingerprint were all rejected/never verified; (iii) a governed release mechanism | owner + architect + security | NON-MOCK; PRODUCTION; NOT a sandbox blocker (synthetic data) |
| **`evidence_ref_hash` binding** | "ledger-finance proposes requiring `evidence_ref_hash` to equal a platform-computed digest over (provider_id, R, amount, asset, merchant_reference), shown to the approvers. security prefers blind entry: the requester and each approver independently type R (no prefill), the server compares each entry to the verdict's R, and `evidence_ref_hash` is the hash of the portal confirmation artefact; for not paid, the same blind entry of the portal's declined status and reference." (s25.7) | neither implemented; M4 non-MOCK is blocked by the Go gate | two competing proposals (above) | owner + security + ledger-finance | NON-MOCK (M4); PRODUCTION; NOT a sandbox blocker (M4 is unavailable in sandbox) |
| **Unexpected-echo park** | "should a park caused by an unexpected echo from an `Unsupported` adapter get a governed completion route, for example M4 'paid' on positive statement evidence? Today the park has no governed exit." (s24.6 item 6) | no exit; hold stranded | none | owner + architect + ledger-finance | NON-MOCK (only for an `Unsupported` provider); NOT a sandbox blocker |
| **GAP-AAM** | no governed exit for an `amount_asset_mismatch` park: options (a) leave parked with operator investigation (current); (b) admit to M4 NOT-PAID only on positive decline evidence (effectively never opens because a provider-confirmed different amount is a success-triggered park); (c) a dedicated governed resolution accepting the provider's actual amount (new financial design). (`open-owner-questions-2026-10-09.md`) | fail closed; hold kept | none | owner + ledger-finance | NON-MOCK; PRODUCTION; NOT a sandbox blocker |
| **O-2(B)** | "whether a deferred non-interactive `created` child (one that exists for a brand/tenant that later went inactive, or that predates this gate) should expire after a bounded time. Children created before this change, or created by any path that still lacks a gate, can still sit deferred and dispatch after reactivation." (ADR 0095 s47.4). Decision 22 ("No automatic cancellation or fund release occurs solely because the brand becomes inactive") is adjacent but not decisive | no expiry; children dispatch after reactivation | none | owner | NON-MOCK (real PSP cascades); PRODUCTION |
| **O-1(A)** | "The section 42.8 race shape ... is permanently non-repairable by this mechanism ... The owner must choose: (a) ACCEPT the permanent attribution gap in that shape for real-provider acceptance ..., OR (b) define DURABLE APPLICATION EVIDENCE written inside the applying transaction ..." (ADR 0095 s45.8) | repair function is guarded and refusing | none | owner | NON-MOCK (real-provider acceptance); PRODUCTION |
| **D-9** | "M4 reuses `payment_force_resolve:*`, so every in-force M2 grant authorises M4. The safer alternative is a distinct capability pair, at the cost of kind-dependent proof operations; security/owner to choose." (s10.2) | M4 reuses the M2 capability | none | owner + security | NON-MOCK (M4); PRODUCTION |
| **HD-R15-6** | "A confirmed misdirected payout (retained today)." (s10.1) | retained; no recovery path | none | owner + ledger-finance + legal | NON-MOCK; PRODUCTION |
| **HD-R15-3** | "Return-to-source (D2)." (s10.1) | not built | none | owner + compliance | NON-MOCK (if the licence requires it); PRODUCTION |

## C. Questions that block PRODUCTION only (not sandbox, not non-MOCK piloting unless the owner says so)

| ID | Exact question (source) | State | Recommendation | Owner |
|---|---|---|---|---|
| **Q-HSEC-1** | "should an engaged payment kill switch for the tenant also block `release_hold_to_player` execution? Implemented as "no" (the switch stops outbound provider dispatch; this movement returns the player's own funds and sends nothing). If the answer is "yes" it is a one-line addition to the executor's preconditions plus the migration-side recheck." (ADR 0111 s16) | implemented "no"; no production policy row | `ledger-finance`: "no" is financially sound | owner |
| **Q-HSEC-2** | "Do the owner decisions cover only `approved` holds on a non-active tenant or brand (as implemented), or also holds in `requested` / `pending_review` on a non-active tenant? Implemented: approved only; the other states keep their hold until reactivation (ADR 0107 CT-PRE territory, not built)." (s16) | approved only | `ledger-finance`: confirm approved-only or route the rest to ADR 0107 CT-PRE | owner |
| **Q-HSEC-3** | "Is it acceptable that a tenant's stricter policy rows are ignored for this operation while the tenant or brand is non-active (a suspended tenant cannot raise the number of platform approvers)? Implemented: ignored, platform baseline only." (s16) | ignored | none (K2-1 precedent) | owner |
| **O-1(B)** | "Decision 19 does not say what 'checked at creation' yields for a refused brand. Followed the SAME precedent as the tenant ...: no child is created, the final decline stands ... If the owner prefers 'create the child but leave it deferred', that is a new decision" (ADR 0095 s45.2 item 1) | no child; intent ends `declined`; money-neutral | `ledger-finance`: money-neutral, player-visible only | owner |
| **O-2(A)** | "A scheduled or operator trigger needs four-eyes governance plus an ADR 0110 signed actor proof; not decided. `RepairReceiptAttribution` has zero non-test callers ..." (s45.8) | no invoking path | none; moot unless O-1(A) chooses (b) | owner |
| **D-OPS-1 / D-OPS-2** | D-OPS-1: "is an operator console for the governed flows wanted before any real provider, or can operators use the API (scripts/tooling) for MOCK and early sandbox?" D-OPS-2: "which roles may see park reasons and attempt state (finance, compliance, platform_admin)." (`operator-console-governed-flows-brief.md`) | no console; no admin read route for attempts/parks/findings | brief recommends API-only until a sandbox provider exists, then read-only views first | owner + security |
| **ALERT-DELIVERY-1 / HD-PRH2-4-OPS** | "Real recipients, on-call, channel" (register) | OPEN; alerts raise but are not delivered | none | owner/ops (needs AWS-hosted channels) |
| **HD-CTF-1..9** | Closed-tenant player-funds policy (ADR 0107 s12) | mechanism design only | none | legal/compliance + owner |
| **ACCESS-ANALYZER-CHECK-1, HD-10.3-2, DEPLOY-FPKEY-1** | AWS account/IAM/key-delivery actions | OPEN | none | owner / AWS admin |
| **CI-BILLING-1** | GitHub billing (green CI) | local evidence only | do not modify | owner/account |

## D. NOT A BLOCKER (recorded)
Q-R32-3 ("Is the interim equivalent of 23.3 sufficient, or is an in-place resume (R-B) wanted?" - ledger-finance: interim
sufficient), HD-R15-4 (crypto destinations, ADR 0008: disabled for real use), HD-R15-7 (non-blocking), HD-R15-9 (recommendation,
deferred), HD-PRH2-3/HD-PRH2-8 (non-blocking), D-1/D-2/D-3/D-5/D-6/D-8 (deviations for the human to see; acknowledgement
requested, no block), L-1/L-2 (provider dependent, not questions).

## E. Minimum to authorise the first sandbox PSP
Answer **Section A** (HD-R15-1, HD-R15-5, D-REG-1, and the `Unsupported`/tenant-visibility pair once the provider is chosen;
HD-R15-2 only if A-5 is not acceptable), name the provider, and sign `sandbox-psp-authorization-request-2026-10-10.md`.
Section B and C are NOT required for the sandbox on a synthetic tenant; they remain gates for non-MOCK and production.
