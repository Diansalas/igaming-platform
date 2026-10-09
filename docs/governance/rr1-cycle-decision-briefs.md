# Decision briefs and gate status - cycle RR1-FRONTEND-RESOLVE (2026-10-09)

Status: **decision material only. Nothing here is decided and nothing here is implemented.** Prepared by the
orchestrator after inspecting the actual governance records (ADR 0095 s44, ADR 0111 s2-s4 and s15-s18, the
human-decision-register, the task registry, HANDOVER). Each brief names the smallest human/owner decision that is
required; unrelated work continues.

## Gate status summary (inspected, not assumed)

| Item | Governance state found | Action this cycle |
|---|---|---|
| RR-1, R-1 matched decline references, silent join fallback | Ledger-finance ruled RR-1 (re-review 2026-10-09); R-1 and the join are security LOW conditions on an APPROVED merge, refusal-direction only | IMPLEMENTED on `gov-r21-rr1` (see registry) |
| s4.8 post-resolution cells (F-1) | Fully specified in ADR 0111 s4.8; RESOLVE-1 "everything else including s4.8 cells" authorised to start (revision 4 co-ruling); provider-independent (MOCK statuses) | IMPLEMENTED on `gov-r22-cells` (see registry) |
| B2C payout-instrument flow | Backend contract is implemented (B13-B); frontend follow-up named by the B13-B reports | `gov-r23-b2c` (see registry) |
| Instrument state gates settlement? | **DECIDED 2026-10-09 (ADR 0095 s48 decision 3; ADR 0111 s22): it does not.** Formerly: **NOT decided.** Only an architect design statement (ADR 0111 s2.4 "polls/evidence are not blocked by instrument state", by analogy with LF95-C10(d)) plus security and ledger-finance RECOMMENDATIONS. ADR 0111 is still `PROPOSED`. No DECIDED row in the register | Brief 1; current behaviour (not gated) left as merged, no policy change |
| `destination_integrity_failure` exit | **NOT defined** by any ADR or ruling. M2 excludes it; the 0125 M4 scope covers only `destination_mismatch`; ledger-finance M-3 flags the stranded hold | Brief 2 |
| Adapter destination echo declaration | Mechanism decided (ADR 0111 s2.6: `EchoesDestinationFingerprint` in the manifest; callbacks only compare). **Whether a non-Synthetic adapter MUST declare is NOT decided**; B13B-15 residual depends on it | Brief 3 |
| ALERT-DELIVERY-1 / HD-PRH2-4-OPS | OPEN in the register (no recipients, on-call or channel). Not approved | Left blocked; nothing invented |
| M-3 (fingerprint ownership claim), L-1 (client-supplied card_token fields), L-2 (several fingerprints per destination) | Recorded as launch-blocking for non-MOCK instruments (ADR 0111 s15.5). M-3 needs an owner/architect choice; L-1/L-2 depend on a real PSP/verifier | Brief 4; nothing enabled |
| L-7 (Synthetic marker inherited through embedding) | Closed for MOCK by the B13-B repo-wide embedding scan, including interface embedding (ADR 0111 s18, B13B-10) | Verified, residual: test doubles are not scanned (by design) |
| B14-B18 | Defined only as "adapter acceptance criteria / sandbox readiness gate items" (HANDOVER, deferred). They depend on the sandbox PSP adapter, which is NOT authorised; B13 does not approve them | Left blocked |

---

## Brief 1 - Should the instrument's current state gate settlement of an already-sent payout?

> **DECIDED (2026-10-09, ADR 0095 s48 decision 3): instrument state MUST NOT gate settlement; implemented/verified and tested, see ADR 0111 s22.** The text below is the original brief, kept unchanged.

**Current state.** Implemented as merged (B13-B): the settlement/evidence paths (sync phase C, poll, callback/receipt)
check snapshot integrity and the destination echo, NOT the instrument's current state. A suspension or revocation
after the provider call therefore does not block recording the provider's outcome. This follows the design statement in
ADR 0111 s2.4 but that ADR is `PROPOSED` and the owner decisions 1-8 (ADR 0095 s44) do not speak to it.

**Why this is a question.** Blocking settlement would strand a payout the PSP already executed (the ledger would
disagree with the PSP and reconciliation would drift). Not blocking means a destination later found bad can still be
recorded as paid.

**Options.** (A) Keep as merged: evidence is never gated by instrument state (security and ledger-finance
recommendation). (A+) Keep A and add a non-blocking compliance signal (audit row plus P1) when evidence settles an
attempt whose instrument is suspended/revoked at that moment; it changes no state. (B) Gate settlement on instrument
state (not recommended).

**Must never happen automatically.** Parking or reversing money that the PSP has already sent; releasing a hold on
the strength of instrument state alone.

**Four-eyes / B13.** None of the options touches binding or the snapshot. (A+) is raise-only.

**Decision required (owner, with security and ledger-finance concurrence):** choose A, A+ or B. Until decided, behaviour
stays A and no code changes.

## Brief 2 - Governed exit for a `destination_integrity_failure` park

> **DECIDED by owner decision 4 (ADR 0095 §48).** Implementation and design: ADR 0111 §23 (M4 not-paid admitted by migration 0127, `IMPLEMENTED` against MOCK; M4 paid refused; "resume" DESIGN ONLY). The text below is the pre-decision brief, kept as history.

**Current state.** A bound withdrawal whose snapshot is missing, unsealed or inconsistent parks its attempt
`disputed` / `destination_integrity_failure` (hold kept, B12 P1, reconciliation standing finding). M2 does not admit the
reason and the 0125 M4 scope admits only `destination_mismatch`, so there is no governed way out: the hold is
stranded (fail closed). The destination hint says explicitly that no M4 route exists.

**Why parked.** The attribution itself is untrustworthy (possible tampering or corruption), so neither "paid" nor
"not paid" evidence can be taken at face value without first re-establishing which destination (if any) was used.

**Possible safe exits (not chosen).** (1) Investigation-only: keep the hold; an operator repairs the snapshot evidence
through a separate, fully audited, four-eyes, platform_acting-only procedure after an integrity investigation, then the
attempt is re-evaluated under the normal cells. (2) Admit the reason to M4 not-paid only (positive decline evidence on
the bound reference, sealed import, platform floor), never M4 paid. (3) PSP recall/return plus off-platform recovery
tracked through the existing investigation status, with a governed compensating entry only under the existing K2 rules.

**Must never happen automatically.** Release of the hold, completion against the hold, re-pointing or re-creating a
snapshot, treating a provider echo as the destination, or any single-staff action.

**Four-eyes / B13.** Any exit must be platform_acting four-eyes with the full audit, must not weaken destination
binding (a snapshot is write-once), and must not give a provider callback any authority over the destination.

**Decision required (architect + security + ledger-finance, then owner):** which exit (1, 2, 3 or a combination) and
its evidence standard. Until then the park remains and the hint keeps saying no M4 route.

## Brief 3 - Must non-Synthetic payout adapters declare destination-echo semantics?

**Current state.** Decided and implemented: callbacks, polls and statements can never set or change a destination; a
provider echo is evidence compared with the platform-authoritative snapshot; mismatch parks `disputed` with no
progression (ADR 0111 s2.6, A-8). The manifest carries `EchoesDestinationFingerprint`. Residual (B13B-15): an adapter
that does NOT declare the echo settles on ordinary evidence, so decision 5 then depends on the vendor. The echo must be
computed inside the adapter under the tenant-bound fingerprinter; a multi-tenant adapter does not receive the tenant in
`Withdraw`/`QueryStatus` today (B13B-8, an architect interface question).

**Recommendation (preserves the safety direction in the instruction; not a decision).** Replace the boolean by an
explicit, mandatory declaration for every non-Synthetic payout adapter, registered fail-closed at startup:
`echoes_destination_fingerprint` (full semantics) or `no_destination_echo` (explicit, owner-acknowledged per provider,
with a compensating control such as statement-level destination evidence). An adapter with no declaration must not
register. This keeps callbacks as evidence only and stops silent equivalence between providers.

**Must never happen automatically.** An adapter becoming eligible by default, or a missing echo being read as a match.

**Decision required (owner + architect + security):** (a) is an explicit declaration mandatory for non-Synthetic payout
adapters? (b) is `no_destination_echo` ever acceptable, and under which compensating control? (c) the tenant-visibility
interface change for real echo computation (architect). Provider dependent for the real computation.

## Brief 4 - B13-A launch flags (nothing enabled)

| Flag | Meaning | State | Decision / dependency |
|---|---|---|---|
| M-3 | A fingerprint-ownership row is written at registration, never deleted, so anyone can permanently claim another person's destination in a tenant | Pinned by tests as current behaviour; launch-blocking for non-MOCK instruments | Owner/architect: claim ownership only at the first non-synthetic verification success; or ignore owners whose instruments for that fingerprint were all rejected/never verified; or a governed release mechanism |
| L-1 | `card_token` `psp_card_fingerprint`, `last4`, `network` are client-supplied | Launch-blocking for non-MOCK card_token | Provider dependent: must come from the PSP/verifier server side |
| L-2 | Several fingerprints can exist for one real destination (optional routing code, email vs account id) so the cross-person conflict check can be dodged | Launch-blocking for real verifiers | Provider/verifier dependent: canonicalisation per rail |
| L-7 | Synthetic marker inherited through embedding | Closed for MOCK (repo-wide scan incl. interfaces) | Residual: test doubles are not scanned (by design) |

No non-MOCK behaviour is enabled by any of these.
