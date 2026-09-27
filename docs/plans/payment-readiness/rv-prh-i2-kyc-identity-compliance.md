# RV-PRH-I2 (KYC) — `identity-compliance` domain ruling on N-1's fix, the review_required forward-move policy item, and the ReviewVerification CAS

- Reviewer: `identity-compliance` specialist
- Date: 2026-09-27
- Reviewed at: HEAD `9324189`, branch `claude/focused-wright-jw88w9`
- Subject: ADR 0096 §20.2 (cross-account overlay narrowed to each other account's latest FINAL
  row), the `KYC-REVIEWREQ-FORWARD-1` registry policy item, and ADR 0096 §20.3 (`ReviewVerification`
  CAS / 409 on lost race)
- Scope: a domain ruling only. No code changed by this review.

## Method

Read ADR 0096 (full, including §19-§20), the security review chain
(`docs/plans/payment-readiness/rv-prh-i2-kyc-security.md`, including its re-verification section
that raised N-1), and the implementation: `internal/kyc/enforcement.go`
(`crossAccountRejectedOverlay`, `finalStatusesSQL`, `evaluateWithdrawalStructuralRule`,
`readLatestVerificationByPlayerAccount`), `internal/kyc/provider.go` (`statusRank`,
`applyForwardOnlyStatus`, `isTerminal`), `internal/kyc/verification_service.go`
(`ReviewVerification`, `ErrVerificationStatusConflict`), and
`docs/decisions/0028-kyc-provider-abstraction-and-verification-model.md` §2 (the verification
state machine) to establish what `expired` actually means as a *status value* versus
`kyc_verifications.expires_at`.

Ran the existing regression suite and one uncommitted scratch probe on a private database:

- Created `ic_rv_kyc_review` via `TEST_ADMIN_DATABASE_URL`, applied `deploy/init-app-role.sql`
  (skipping its `CREATE ROLE igaming` statement — that role and `igaming_runtime` already exist
  cluster-wide in this sandbox — and with the database name substituted throughout), migrated to
  105 with `cmd/migrate`, dropped afterward. Working tree left clean throughout (verified with
  `git status --short`).
- `go test -tags integration -count=1 -race ./internal/kyc/... -run 'TestEvaluateEnforcement_N1|TestEvaluateEnforcement_OrphanAfterApproval|TestEvaluateEnforcement_OrphanOnAnotherAccount|TestReviewVerification_ConcurrentSubmissionDuringReview'` — **all pass**, confirming the committed §20.2/§20.3 tests behave as described.
- Full `go test -tags integration -count=1 ./internal/kyc/...` — **pass** (5.5s).
- One uncommitted scratch probe (`internal/kyc/zzz_probe_expired_overlay_test.go`, written, run,
  and deleted — not part of this commit) reproducing the sequence in Ruling 1 below: account A
  approved, account B rejected (overlay denies A's withdrawal, confirmed), then B is moved to
  `expired` — not `approved` — by a fresh terminal provider outcome. Result:
  `Outcome:passed Allowed:true`. **The overlay's deny lifted on a status that is not a pass.**
  This confirms Ruling 1 empirically rather than by code-reading alone.

## Ruling 1 — the final-row rule is the right *shape*, but the implementation as committed is not fully correct: `expired` must not lift a rejection

**The final-row rule (restricting the cross-account overlay to each other account's latest
FINAL/terminal row, excluding `pending`/`review_required`/orphans) is correct and closes N-1's
reproduced exploit.** N-1 exploited the fact that a merely-in-progress re-verification (`pending`)
counted as "decided." Narrowing to `approved`/`rejected`/`expired` closes that specific hole, and
the four `TestEvaluateEnforcement_N1_*` tests pin it precisely (pending, review_required, and
orphan all confirmed non-lifting; a later final `approved` confirmed lifting). That part of §20.2
is sound and I concur with it.

**However, `expired` should never have been added to the set of statuses that can *supersede* a
rejection, and as implemented it silently does.** Two facts settle this:

1. **`kyc_verifications.status = 'expired'` is not "an approval whose clock ran out."** ADR 0028
   §2's own state diagram has `expired` as a sibling terminal outcome reachable directly from
   `review_required`/`pending`, exactly like `approved`/`rejected` — it is a distinct provider
   decision (`ProviderExpired`, `internal/kyc/provider.go:28,438-439`), not a status the platform
   ever derives from `expires_at` passing on an approved row. §2.6(b) is explicit that an approved
   row whose `expires_at` has lapsed keeps `status='approved'` in the database forever unless a
   provider delivers an actual `expired` outcome — confirmed by grep: no scheduled job in this
   codebase ever writes `status='expired'`; the only writer is a provider/callback outcome
   mapping. So a `expired` *status* row on account B means "a re-verification *attempt* on B
   reached a vendor's own terminal 'could not complete/lapsed' outcome," not "B's prior valid
   approval aged out." It carries no positive evidence that the person who was earlier rejected on
   B has since been cleared.
2. **Every other place this ADR's own design touches `expired`, it is treated as equivalent to
   `rejected`/never-verified, never to `approved`.** §2.3 states plainly: "`failed`... Covers
   'never verified', `rejected`, and `expired` uniformly." `OutcomeFailed`'s own doc comment
   (`enforcement.go` — the `OutcomeFailed` constant) folds `rejected` and `expired` into the same
   deny bucket, deliberately, precisely because neither is a pass. The cross-account overlay's
   `finalStatusesSQL` breaks that uniformity: by putting `expired` in the same "latest final row"
   slot as `approved`, it lets a fresh `expired` row overwrite the *memory* of an earlier
   `rejected` row for the purposes of "is this OTHER account currently the source of a deny" —
   exactly the class of neutralization N-1 closed for `pending`, reopened for `expired`.

**Reproduction (confirmed on the scratch DB, see Method):** Person P, account A approved, account
B rejected → withdrawal from A denied (correct). Player calls `CreateVerification` on B again; this
time the (mock) provider's terminal outcome is `expired` rather than `pending`/`approved`. B's
latest FINAL row is now the `expired` one, not the earlier `rejected` one. The overlay's `EXISTS`
predicate requires `v1.status = 'rejected' AND v1.id = (latest final row's id)` — since the latest
final row is `expired`, not `rejected`, this now returns false. Withdrawal from A becomes
**allowed**. This is functionally identical in shape to the N-1 exploit security found (a status
change on the rejected account, achieved through an ordinary API call, silently clears the deny),
just reached through `expired` instead of `pending`.

**Ruling: a rejection on another account of the same Person must be lifted only by a later
genuine `approved` decision on that same account — never by `expired`, and (as already correctly
implemented) never by `pending`/`review_required`/an orphan.** The correct predicate distinguishes
"the account's latest final row is `approved`" (lifts) from "the account's latest final row is
`rejected` or `expired`" (both must continue to deny if a rejection exists in that account's
history and has not been superseded by a genuine approval) — it must not simply ask "is the
account's single most-recent final row literally `rejected`." Concretely, the fix direction I am
recording for the `architect`/implementer (I have not made this change — CLAUDE.md's stage-gate
and specialist-authority rules mean I record the ruling, not silently patch shared enforcement
code): the subquery should determine whether B has ever been rejected and, if so, whether that
rejection has since been cured by a later `approved` — not merely find "the latest row among
`{approved, rejected, expired}`" and test whether that one row happens to say `rejected`. One
sufficient formulation: deny if `EXISTS` a `rejected` row on B whose `created_at`/`id` is not
superseded by any *later* `approved` row on B (an `expired` row, wherever it falls, never counts
as superseding evidence and never needs to be selected at all for this purpose).

**Required test, to accompany the fix:** `rejected(B) → later fresh CreateVerification on B whose
terminal outcome is expired (not approved) → withdrawal from approved A still denies` — the
`expired` mirror of `TestEvaluateEnforcement_N1_LaterFinalApprovedLiftsRejection`. I have written
and confirmed this scenario fails against the current implementation (scratch probe, not
committed, per Method above); it is not yet a committed regression test.

### Addendum — N-1b and the coordinator's ordered fix ("only a later `approved` lifts a rejection")

The coordinator relayed security's re-verification (labeled N-1b, `rv-prh-i2-kyc-security.md`,
reviewed at commit `be423c3`) confirming the same gap this ruling found independently, and has
ordered a fail-closed fix: **only a later `approved` on the other account lifts a rejection.**

**That is the domain-correct fix, and it matches Ruling 1 above exactly — I confirm it, with no
different treatment of `expired` needed beyond simply never letting it supersede.** To be precise
about what "only a later `approved` lifts" should mean mechanically, so the implementation doesn't
trade N-1b for a narrower but still-wrong variant:

- **`expired` must be excluded from the "latest final row" selection entirely for this purpose**,
  not merely excluded from the set of statuses that count as lifting. If the subquery still
  selects `expired` as "the" latest final row and then separately special-cases "only approved
  lifts," a `rejected → expired` sequence on B would correctly *fail to lift* the rejection, but a
  naive implementation could still find "the latest final row is `expired`, which is not
  `approved`, so treat as not-rejected-either" — i.e. drop the deny instead of keeping it. That
  would still be a bypass, just a differently-shaped one (allow instead of the current allow — no
  improvement). The safe formulation is the one in Ruling 1: `expired` is invisible to this
  subquery, so a `rejected(B) → expired(B)` sequence still finds the `rejected` row as B's
  effective latest-final-for-this-purpose row, and continues to deny — not merely "does not lift,"
  but "actively still denies," which is the correct AML posture (a case that entered review again
  and lapsed without a decision is not resolved, and must not read as neutral).
- **A later `rejected` re-decision on B changes nothing observable** (still denies) and needs no
  special handling — it's already covered by "only `approved` lifts."
- **No different treatment of `expired` is warranted beyond this.** I considered and reject two
  alternatives: (a) treating `expired` as its own signal that should *itself* independently deny
  (even absent a prior `rejected`) — out of scope here, `crossAccountRejectedOverlay` is
  deny-only for `rejected` specifically per its own original security prescription (§2.6/§19.2),
  and inventing a new "expired denies too" rule is a scope expansion this document does not make;
  (b) letting `expired` sit "neutral" (neither lifts nor keeps a deny, i.e. falls through to
  whatever the next-older final row says) — this is functionally the same outcome as excluding it
  from selection, which is what I'm recommending, so there is no daylight between "neutral" and
  "invisible to the subquery" as long as the implementation walks back to the nearest
  `approved`/`rejected` row rather than stopping at the newest final row of any kind.
- **This remains a mechanism fix, not a human/regulatory decision** — same reasoning as the rest
  of Ruling 1 and Ruling 4 below: it's an internal correctness question about what counts as
  curative evidence within enforcement code this specialist already owns, not a jurisdiction value
  or legal interpretation.

I validated the recommended fix predicate ("only `approved`/`rejected` are eligible as the
'latest final row'; `expired` excluded from selection entirely") against a second, disposable
scratch database (`ic_rv_kyc_review2`, created/migrated/dropped the same way as the first, per
Method) using a standalone probe query — not a change to `enforcement.go` — that implements
exactly that predicate. Both required sequences passed:

- `rejected(B) → expired(B)`: predicate still finds the `rejected` row as B's effective
  latest-final-for-this-purpose row and **denies** — confirming the walk-back behaves as intended,
  not merely "does not lift" but "actively still denies."
- `rejected(B) → expired(B) → approved(B)`: predicate correctly walks forward to the later
  genuine `approved` row and **allows** — confirming an intervening non-lifting terminal row
  (`expired`) does not permanently freeze the account once a real approval later arrives.

`pending`/`review_required`/orphan-after-rejection were already covered by the four
`TestEvaluateEnforcement_N1_*` tests (Method, above) and are unaffected by this change. This
gives me confidence the coordinator's ordered fix, formulated as "expired excluded from
selection, not merely excluded from lifting," is both correct and implementable without new
schema. The required regression tests named above should assert both the `rejected→expired`
(deny) and `rejected→expired→approved` (allow) sequences.

**Severity/labeling:** this is a residual instance of the same finding class as N-1 (HIGH,
pre-existing) — not a new, independent defect I am naming, but a narrower reopening of the exact
hole §20.2 was meant to close. §20.2's own claim that "this is not a weakening of enforcement...
every genuine deny this ADR's structural rule requires is preserved byte for byte" is **not
accurate** while this gap is open. This blocks treating N-1 as fully closed and should block
production launch on the same basis N-1 already does, until fixed and test-pinned.

## Ruling 2 — `KYC-REVIEWREQ-FORWARD-1`: a staff-set `review_required` must be sticky against an automated forward move to `approved`

The registry item's framing is correct that this is a policy question, not a bug, and it is
squarely inside identity-compliance's authority to decide (an internal enforcement/case-management
control, not a jurisdiction-varying legal threshold). Ruling:

**A `review_required` verification that a STAFF member set must not be automatically advanced to
`approved` by a later provider result or callback alone.** Only another explicit staff decision
(a further `ReviewVerification` call) may move such a case forward. A `review_required` state that
the *provider itself* set (its own "needs manual review" signal, `ProviderReviewRequired` →
`StatusReviewRequired`, with no staff actor involved) is unaffected by this ruling — the existing
forward-only behavior (a later vendor `approved` proceeding automatically) is fine there, because
no human judgment is being silently overridden; it is simply the vendor's own two-step decision
process completing.

**Rationale:** `review_required` set by a compliance officer is a deliberate escalation — the
officer looked at the case and decided it needs more evidence or closer scrutiny before a decision
can be made (this is one of exactly three legal `ReviewVerification` targets, alongside
`approved`/`rejected` — `verification_service.go:496`). Letting a subsequent, purely automated
vendor decision (on a resubmission that may or may not even address what the officer flagged)
silently close out that officer's own open case, with no human ever re-confirming it, defeats the
purpose of having a human-in-the-loop review state at all. This is consistent with CLAUDE.md's
"[the identity-compliance specialist] cannot weaken an RG or KYC enforcement rule to ease a
product flow" and "enforcement (blocking play/withdrawal) is our code, not the vendor's" — a
human's compliance escalation should never be closeable by vendor say-so alone. This ruling
*strengthens* enforcement (it removes a path by which a human escalation can be silently
undone), so it does not need separate human/legal sign-off under the Authority section's own
"exception requires an explicit, recorded decision" language — this document is that record for
identity-compliance's own domain rule, not a new legal or jurisdictional constraint.

**Mechanism, for whoever implements this (not done by this review):** `reviewed_by`/`reviewed_at`
on `kyc_verifications` are set ONLY by the staff `ReviewVerification` path — confirmed by reading
`internal/kyc/provider.go`'s own doc comment at `applyForwardOnlyStatus`
("`reviewed_at`/`reviewed_by` are never touched here — this applies a PROVIDER-driven transition,
never a staff one") and by grep finding no other writer of those columns. That is a sufficient,
already-existing signal: `applyForwardOnlyStatus`/`applyCallbackOutcome` should treat a current
status of `review_required` with a non-null `reviewed_by` as a stop, not a pass-through — a
provider/callback result arriving in that state should no-op (`applied=false`, `AND status = ...`
CAS naturally fails or an explicit rank exception is added) rather than moving the row to
`approved`/`rejected` on its own. A subsequent explicit staff `ReviewVerification` call remains the
only path out of that state. This does not change the CAS mechanics of the provider/callback path
in any other respect and does not touch a provider-set `review_required` (no `reviewed_by`).

**Not a human decision.** No jurisdiction value, legal threshold, or regulatory interpretation is
involved — this is an internal case-management control design choice, which this specialist is
authorized to make per the Authority section of its charter and the registry item's own explicit
assignment ("identity-compliance to rule").

## Ruling 3 — `ReviewVerification`'s CAS returning 409 on a lost race is correct

**Confirmed correct, no change needed.** When a staff decision's CAS write loses a race to a
concurrent provider/callback status change, returning `ErrVerificationStatusConflict` (409) rather
than either (a) silently overwriting with the staff decision based on stale context, or (b)
silently retrying/re-reading and applying anyway, is the right posture for a compliance-relevant
human decision:

- A staff approve/reject/review_required call is a one-time judgment about the state the officer
  observed. If that state changed underneath them (a provider or callback delivered new
  information in the same window), the officer's decision was made against data that is no longer
  current — applying it anyway, in either direction, would attribute a decision to a human that
  was not actually based on what the system currently shows.
- This correctly follows the *asymmetry* ADR 0096 §20.3 draws with the provider/callback path's own
  `applyForwardOnlyStatus` retry loop: a provider-driven transition is safely retryable/idempotent
  (it is just a machine re-reading and reapplying the same rule), but a staff decision is not
  something the system should retry on the officer's behalf — the correct behavior is to surface
  the conflict and require the officer to look again and re-decide with current information, which
  is exactly what the 409 does.
- This also serves the audit trail: a silently-dropped or silently-overwritten staff action would
  leave `audit_log` unable to show why the officer's intended decision did not take effect, which
  is exactly the kind of hidden-state problem CLAUDE.md's audit and "no fake completion" rules
  guard against.

`TestReviewVerification_ConcurrentSubmissionDuringReview_ReturnsConflict` (confirmed passing on
the scratch DB) pins this. I concur with the design as implemented and find no gap here.

## Ruling 4 — human decisions

Per CLAUDE.md, I do not invent thresholds or legal interpretations, and I flag precisely what
would need one:

- **None of the three items above require a human/regulatory decision.** All three are internal
  KYC enforcement mechanism/case-management design questions inside identity-compliance's own
  Authority (owns "the identity/person data model and compliance rule configuration schema" and
  "cannot weaken an RG or KYC enforcement rule"). Ruling 1 and Ruling 2 both *tighten* enforcement
  (they close paths by which a deny could be silently lifted); Ruling 3 confirms an already-correct
  design. None of them touch a jurisdiction-specific numeric threshold, a licensing question, or a
  legal interpretation of what KYC/AML law requires — they are about how this codebase's own
  enforcement mechanism behaves once ADR 0096's already-recorded human-decision placeholders
  (HD-KYC-1 through HD-KYC-8, threshold values, "configurable limits" scope, etc.) are eventually
  supplied. Those placeholders remain exactly as ADR 0096 already records them; I am not creating,
  removing, or pre-answering any of them here.
- **What I am explicitly NOT ruling on:** whether any jurisdiction should require KYC before play
  (HD-KYC-8), what the cumulative-deposit/EDD amounts should be (HD-KYC-1/HD-KYC-2), or whether the
  withdrawal structural rule itself should ever be jurisdiction-configurable (HD-KYC-5, which this
  ADR already recommends against). Those remain open human/legal decisions, unaffected by this
  review.

## Labels

- **Ruling 1 (Q1, `expired` vs. `rejected` in the cross-account overlay):** finding, HIGH,
  residual instance of N-1's class — **NOT IMPLEMENTED** (I record the ruling and a reproduced,
  confirmed gap; the fix itself is for the implementer, per specialist role separation). Blocks
  treating N-1 as closed and should block production launch on the same basis, until fixed and
  test-pinned with the `expired`-variant regression test named above.
- **Ruling 2 (Q2, `KYC-REVIEWREQ-FORWARD-1`):** domain ruling recorded — **NOT IMPLEMENTED** (a
  policy decision this document supplies; the mechanism change is a follow-up implementation task,
  not done here).
- **Ruling 3 (Q3, `ReviewVerification` 409 on lost race):** **VERIFIED CORRECT**, no change
  needed.
- **Ruling 4 (Q4, human decisions):** none of the above require one; this document does not invent
  any threshold or legal interpretation, and does not claim any regulatory approval or
  certification (CLAUDE.md "No fake completion" / "Compliance").
