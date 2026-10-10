# Open owner questions - exact wording (2026-10-09)

Requested by the human in `CONTINUOUS-GOVERNANCE-EXECUTION-CYCLE-2026-10-09` (item 7: "DO NOT DECIDE YET ... extract the EXACT questions").
**Nothing here is decided. No answer is inferred from an identifier.** Each entry gives: (1) the exact question text as it stands in
the repository (quoted verbatim, with its source), (2) why it is still open, (3) the implementation state, (4) any
security / ledger-finance / architect recommendation, (5) the exact decision required, (6) what engineering it unblocks.

**Identifier collision to be aware of.** `O-1` / `O-2` are used for TWO different pairs of questions:
(A) ADR 0095 s45.8 (the receipt-attribution repair, `PAY-RECEIPT-ANOMALY-APPLIED-1`), and
(B) the receipt-site cascade gate (ADR 0095 s45.2(1) / s47.4; labelled O-1/O-2 in the review reports and registry rows).
Both pairs are reproduced below as O-1(A), O-2(A), O-1(B), O-2(B). Questions already formally decided elsewhere are identified.

---

## Q-HSEC-1

1. **Exact text** (ADR 0111 s16, the HSEC implementation notes): "Open OWNER question (not decided here; the design fails closed around it; no production policy row is added by this change). Q-HSEC-1: should an engaged payment kill switch for the tenant also block `release_hold_to_player` execution? Implemented as "no" (the switch stops outbound provider dispatch; this movement returns the player's own funds and sends nothing). If the answer is "yes" it is a one-line addition to the executor's preconditions plus the migration-side recheck."
2. **Why open.** Owner decisions 13-18 (ADR 0095 s44) say the kill-switch semantics (inactive tenant/brand = no normal payout submission) stay intact; they do not say whether the switch also blocks the governed release.
3. **State.** IMPLEMENTED as "no" (release is not blocked by the kill switch). No production policy row exists, so nothing can execute in production.
4. **Recommendation.** `ledger-finance`: "no" is financially sound (nothing leaves the platform, no attempt is created, released cash stays behind the payment-initiation and gameplay gates). "Yes" only if the switch is meant to freeze ALL money movement during an incident.
5. **Decision required (owner):** should an engaged tenant payment kill switch also block `release_hold_to_player` execution - yes or no?
6. **Unblocks.** "No": nothing further to build; a production policy row for the HSEC release can be considered. "Yes": one precondition plus the migration-side recheck (small).

## Q-HSEC-2

1. **Exact text** (ADR 0111 s16): "Q-HSEC-2 (scope of decisions 13-18). Do the owner decisions cover only `approved` holds on a non-active tenant or brand (as implemented), or also holds in `requested` / `pending_review` on a non-active tenant? Implemented: approved only; the other states keep their hold until reactivation (ADR 0107 CT-PRE territory, not built)."
2. **Why open.** Decisions 13-18 name the "approved hold" (HD-CTF-6); they do not mention `requested` / `pending_review` holds.
3. **State.** Approved-only is implemented. Holds in other states keep their hold until reactivation. A single staff Reject or a player Cancel can still release a `requested`/`pending_review` hold on a non-active tenant (existing behaviour, not changed).
4. **Recommendation.** `ledger-finance` (HSEC review): the owner should confirm that 13-18 cover approved holds only, or route the rest to ADR 0107 CT-PRE. Not a blocker for the current branch.
5. **Decision required (owner):** do decisions 13-18 cover only `approved` holds, or also `requested` / `pending_review` holds on a non-active tenant?
6. **Unblocks.** "Approved only": closes the question; the other states stay under ADR 0107 CT-PRE. "Also other states": a new governed mechanism (design + implementation) for those states.

## Q-HSEC-3

1. **Exact text** (ADR 0111 s16): "Q-HSEC-3 (HN-6). Is it acceptable that a tenant's stricter policy rows are ignored for this operation while the tenant or brand is non-active (a suspended tenant cannot raise the number of platform approvers)? Implemented: ignored, platform baseline only; pinned by `PolicyLookup_IgnoresTenantRowsWhenNonActive`."
2. **Why open.** The decisions say four-eyes and a configurable threshold, not whether tenant rows may tighten the platform baseline while the tenant is suspended.
3. **State.** IMPLEMENTED: tenant policy rows are ignored while the tenant or brand is non-active; the platform baseline (two distinct Persons) applies. Fails safe in the sense that it can only be as strict as the platform baseline.
4. **Recommendation.** None recorded beyond `ledger-finance` asking the owner to confirm (HN-6; the K2-1 precedent: tenant rows can only raise the requirement).
5. **Decision required (owner):** is it acceptable that a tenant's stricter policy rows are ignored for the governed hold release while the tenant or brand is non-active?
6. **Unblocks.** "Acceptable": closes it. "Not acceptable": change the policy lookup to honour tenant rows for the tenant (must also decide how a suspended tenant's rows can be trusted) - a small migration-side and Go change plus tests.

## O-1(A) - receipt-attribution repair (ADR 0095 s45.8)

1. **Exact text:** "O-1 (OWNER). The section 42.8 race shape (provider reference bound by Y, merchant reference naming X) is permanently non-repairable by this mechanism, and after LF C1 the S7 cross-provider shape is too. The owner must choose: (a) ACCEPT the permanent attribution gap in that shape for real-provider acceptance (the receipt stays anomalous with the durable signal; the money/state effect was already correct), OR (b) define DURABLE APPLICATION EVIDENCE written inside the applying transaction (a main-path change outside this branch), on which a repair could rely."
2. **Why open.** Owner decision 24-30 chose Option A (one-time locked re-attribution + audit-row repair) but the record contains no durable evidence that "delivery B applied E to X", so no reachable shape is repairable.
3. **State.** Option A is IMPLEMENTED as a guarded, audited, REFUSING function (migration 0122); no production sequence produces a repairable receipt; zero non-test callers.
4. **Recommendation.** None recorded (the implementer presented both options).
5. **Decision required (owner):** (a) accept the permanent attribution gap in that shape for real-provider acceptance, or (b) require durable application evidence written inside the applying transaction.
6. **Unblocks.** (a): closes it; the repair function stays refusing. (b): a main-path change in the receipt apply transaction plus a repair that relies on it.

## O-2(A) - receipt-attribution repair trigger (ADR 0095 s45.8)

1. **Exact text:** "O-2 (OWNER). A scheduled or operator trigger needs four-eyes governance plus an ADR 0110 signed actor proof; not decided. `RepairReceiptAttribution` has zero non-test callers, pinned by `TestStaticWiring_RepairReceiptAttributionHasNoNonTestCallers_L3` (an allow-list entry requires review against this section and ADR 0110). Any operator, scheduler or route entry needs a new owner decision and must replace `ActorSystem` with the authenticated actor."
2. **Why open.** Decisions 24-30 say it is "not a general reassignment mechanism" and "locked, idempotent, fully audited"; they do not authorise any invoking path.
3. **State.** No invoking path exists (function is system-internal; static test pins zero callers).
4. **Recommendation.** None recorded. Moot unless O-1(A) chooses (b).
5. **Decision required (owner):** is any scheduled/operator/route entry to the repair function wanted, and under which governance (four-eyes + signed actor proof)?
6. **Unblocks.** A governed entry point (route or scheduler) for the repair - only meaningful once a repairable shape can exist.

## O-1(B) - receipt-site cascade gate: inactive brand/tenant callback decline (ADR 0095 s45.2(1), s47.4)

1. **Exact text** (ADR 0095 s45.2 item 1): "Decision 19 does not say what "checked at creation" yields for a refused brand. Followed the SAME precedent as the tenant (section 43.2 / B8): no child is created, the final decline stands, own audit action. Consequence: for that decline the intent ends `declined` (a player-visible outcome change relative to a cascade that would have continued). If the owner prefers "create the child but leave it deferred", that is a new decision; the claim-time gate (decision 20/21) already guarantees such a child would never dispatch." (s47.4: "Receipt-path children are not a new decision: the intent ends `declined` for an inactive brand/tenant, exactly as at phase C and the poll path (section 45.2(1) interpretation applies unchanged.")
2. **Why open.** Decision 19 ("Brand eligibility MUST be checked at cascade-child creation") does not say whether a refusal means "no child, final decline" or "child created but deferred". The implementation chose the first, following the tenant precedent.
3. **State.** IMPLEMENTED (phase C, poll, and the receipt/callback site): no child; the deposit intent ends `declined`; one audit row. Money effect is neutral either way (deposits post nothing on a decline).
4. **Recommendation.** `ledger-finance`: money-neutral; what changes is the player-visible outcome, especially for temporary suspensions. For the tenant it follows existing B8 policy; for the brand it is still an interpretation.
5. **Decision required (owner):** for a cascadable decline when the brand (or tenant) is inactive, is the final decline with NO child the intended outcome, or should the child be created and held deferred?
6. **Unblocks.** "No child": closes it. "Deferred child": change creation sites to create-and-defer (reverses the R17 gate) and decide expiry (O-2(B)).

## O-2(B) - stale deferred non-interactive child expiry (ADR 0095 s47.4)

1. **Exact text:** "OWNER QUESTION, unchanged and NOT implemented: whether a deferred non-interactive `created` child (one that exists for a brand/tenant that later went inactive, or that predates this gate) should expire after a bounded time. Children created before this change, or created by any path that still lacks a gate, can still sit deferred and dispatch after reactivation."
2. **Why open / related decision.** Decisions 21-22 say such a child MUST remain deferred, MUST NOT be dispatched, and "No automatic cancellation or fund release occurs solely because the brand becomes inactive." They do not decide a time-bounded expiry, which is a different trigger (elapsed time, not brand status). Whether decision 22 already forbids bounded expiry is an interpretation, NOT a recorded decision - flagged so the human can say "already covered by 22" if that is the intent.
3. **State.** Not implemented. Deferred children can dispatch after reactivation.
4. **Recommendation.** None recorded. Safety note: a deferred child that dispatches long after reactivation may charge the player at another PSP unattended.
5. **Decision required (owner):** should a deferred non-interactive `created` child expire after a bounded time (and if so, what bound and what happens to it), or is decision 22 to be read as forbidding any automatic cancellation including time-based expiry?
6. **Unblocks.** Expiry job design and implementation (with audit and four-eyes where money is concerned), or closing the question.

## M-3 - fingerprint-ownership claim (ADR 0111 s15.5 / s15.3)

1. **Exact text** (ADR 0111 s15.5, "Launch-blocking for non-MOCK instruments / payouts"): "**M-3** A fingerprint-ownership claim is permanent (A-2: owner rows are never deleted) and there is no release path. A Person who registers a destination that is not theirs, or one they later lose, blocks every other Person in the tenant from it forever (a denial-of-registration vector) and nothing can correct a wrong claim. A governed (four-eyes, proof-bound) release or re-assignment writer is required before any real customer data."
2. **Why open.** The B13 decisions do not cover instrument-ownership claims or their release.
3. **State.** Current behaviour pinned by tests: the owner row is written at registration (before verification, no KYC requirement) and never deleted. MOCK only.
4. **Recommendation.** `security` (B13-A review) listed options: (i) claim ownership only when the first non-synthetic verification succeeds; (ii) ignore owners whose instruments for that fingerprint were all rejected or never verified; (iii) add a governed (four-eyes, signed-proof) release mechanism. Recorded as launch-blocking for non-MOCK use. No option was chosen.
5. **Decision required (owner, with security/architect):** which of (i), (ii), (iii) (or a combination) governs ownership claims.
6. **Unblocks.** The ownership-claim change and/or the governed release writer; a prerequisite for accepting real customer destinations.

## HD-R15-5 - approved withdrawal whose instrument becomes permanently unusable (ADR 0111 s10)

1. **Exact text:** "**HD-R15-5** Disposition of an `approved` withdrawal whose instrument becomes permanently unusable: parked (current), four-eyes release, or four-eyes rebind. **Stranded funds: launch-blocking before any non-MOCK payout (LF L-7).**" (s2: "An `approved` withdrawal whose instrument becomes permanently unusable stays parked (HD-R15-5, launch-blocking per s10.3).")
2. **Why open.** The B13 decisions say there is no normal staff override and no rebind by default; they do not choose what happens to the withdrawal.
3. **State.** Parked (current): the request stays `approved`, every submit is refused (T1p refusal commits a denial audit only; no B12 alert because no attempt exists), until a governed path exists.
4. **Recommendation.** `ledger-finance` (L-7): stranded funds are launch-blocking before any non-MOCK payout. No option chosen. Note: the new decision 4 (`destination_integrity_failure`) concerns a parked attempt, NOT this state, and does not answer HD-R15-5.
5. **Decision required (owner, with ledger-finance and security):** for an `approved` withdrawal whose instrument becomes permanently unusable, choose: remain parked, governed four-eyes release (hold returns to the player's own cash), or governed four-eyes rebind to another verified instrument of the same player.
6. **Unblocks.** A governed disposition mechanism (likely reusing the HSEC `release_hold_to_player` machinery for the release option); removes a launch blocker.

---

## Blocking matrix (added 2026-10-10)

Blocking classification is the orchestrator's reading of the existing records (ADR 0111 s10.3 launch flags, s15.5, s16, s23-s25;
ADR 0095 s45-s47); "no" means no record makes it a prerequisite, not that it is irrelevant. The owner may reclassify.

| Question | Blocks sandbox (synthetic tenant) | Blocks real providers / non-MOCK money | Blocks AWS | Blocks production |
|---|---|---|---|---|
| Q-HSEC-1 (kill switch vs hold release) | no (release is MOCK, no production policy row) | no | no | yes before a production HSEC policy row is added |
| Q-HSEC-2 (scope of decisions 13-18) | no | no | no | yes before any production policy row; other states stay held until reactivation |
| Q-HSEC-3 (tenant policy rows ignored while non-active) | no | no | no | yes before a production HSEC policy row |
| O-1(A) (receipt-attribution race shape) | no | yes for real-provider acceptance (attribution gap) | no | yes |
| O-2(A) (repair trigger governance) | no | no (function stays refusing) | no | no, unless O-1(A) chooses durable evidence |
| O-1(B) (inactive brand/tenant callback decline: final vs deferred child) | no | no (money-neutral) | no | yes (player-visible outcome) |
| O-2(B) (stale deferred child expiry; decision 22 adjacent) | no | yes before real PSP cascades (a stale child may charge unattended) | no | yes |
| M-3 (fingerprint ownership claim) | no | yes (before any real customer destination) | no | yes |
| HD-R15-5 (stranded approved withdrawal) | no | yes (ledger-finance L-7: before any non-MOCK payout) | no | yes |
| Per-provider acceptability of `Unsupported` echo (decision 5 consequence) | no | yes for a provider that cannot echo | no | yes |
| Governed completion route for a park from an unexpected echo | no | yes for such a provider | no | yes |
| `evidence_ref_hash` binding (ledger-finance digest vs security blind entry) | no | yes before non-MOCK M4 | no | yes |
| Resume of a `destination_integrity_failure` park (Q-R32-3) | no | no (interim route sufficient) | no | no |
| D-REG-1 (are bank/e-wallet identifiers "raw credentials" for decision 8) | no | yes before accepting real instruments via the raw-detail route | no | yes |
| ALERT-DELIVERY-1 / HD-PRH2-4-OPS | no (owner YES in principle) | no | yes (AWS-hosted channels) | yes |

---

## NEW (2026-10-10): GAP-AAM - no governed exit for an `amount_asset_mismatch` park

1. **Exact finding** (security fidelity review of the OpenAPI documentation, verified against the code on 2026-10-10): a payout attempt
   that the provider confirms with an amount or asset different from the withdrawal request is parked `disputed` with terminal
   reason `amount_asset_mismatch` and the hold is kept. The `resolve` route answers 409 "provider-confirmed amount/asset does not
   match the withdrawal request". `amount_asset_mismatch` is `false` in `PayoutDisputeReasons` (not admitted to M2), is not in
   `payment_m4_in_scope` (not admitted to M4), and the HSEC hold release requires an `approved` withdrawal while this one is
   `submitted`. Therefore **no automatic, M2, M4 or hold-release path admits this reason today.**
2. **Why open.** No owner decision addresses this reason. Decisions 9-12 (RESOLVE-1) and 13-18 (HSEC) name other parks;
   decision 4 (2026-10-09) covers only `destination_integrity_failure`.
3. **State.** Fail closed: the hold stays, B12 alert raised, reconciliation raises the bound `pay_captured_unposted` finding.
   Recovery today is operator investigation / PSP recall / off-platform.
4. **Recommendation.** None recorded. Options for the owner/ledger-finance: (a) leave parked with operator investigation (current);
   (b) admit it to M4 NOT-PAID only on positive decline evidence on the bound reference (same standard as the other bound parks;
   a provider-confirmed different amount is a success-triggered park, so the durable success record would make not-paid
   `contradictory`: the exit would effectively never open); (c) a dedicated governed resolution that accepts the provider's
   actual amount (a partial/over payout) with four-eyes and an adjustment, which is a new financial design.
5. **Decision required (owner + ledger-finance):** choose (a), (b) or (c) for `amount_asset_mismatch` parks.
6. **Unblocks.** (a): closes it as accepted. (b)/(c): a governed exit (migration + tests + reviews).
7. **Blocks.** Sandbox: no. Real providers / non-MOCK money: yes (a real PSP will eventually confirm a different amount, e.g. fees).
   AWS: no. Production: yes.
