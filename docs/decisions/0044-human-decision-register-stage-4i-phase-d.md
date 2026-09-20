# 0044 — Human Decision Register: Stage 4I Phase D Items (HDR-J-7, HDR-J-8, HDR-J-9)

Status: **Record only. No decision is selected in this document.** These
three items are **NEW**, registered by the Stage 4I Phase D architect
design ruling ("Stage 4I Phase D: Jurisdiction Policy Configuration &
Operational Semantics"). This is a companion to
`docs/decisions/0041-human-decision-register-stage-4i-jurisdiction.md`
(HDR-J-1 through HDR-J-6) and `docs/decisions/0042-human-decision-
response.md` (the human's recorded answers to HDR-J-1 through HDR-J-6) —
neither of those documents contains these three items, and this document
does not restate or alter anything they already recorded. There is **no
`HUMAN ANSWER:` section for any item below** — these are open questions
only, exactly as the architect ruling wrote them, reproduced verbatim.

Following `0041`'s and `0039`'s discipline: this document does not
recommend an answer to any of the three items — doing so would cross into
`identity-compliance`'s, `risk`'s, `security`'s, or a legal/licensing
domain this document has no authority in. Options are named exactly as the
architect ruling names them.

## HDR-J-7 — Which jurisdiction determination each operation class requires

**Identifier:** HDR-J-7 — Which jurisdiction determination each operation
class requires.

**Exact question:** For each of the platform's four jurisdiction-consuming
operation classes — placing a casino bet (`play`), deciding whether a
game/asset is offered at all (`catalogue_availability`), granting a bonus
(`bonus_issuance`), and converting completed bonus funds to withdrawable
cash (`bonus_conversion`) — which determination must the platform make
before that operation may proceed: (a) a **residence-based identity
determination** (the player's KYC-verified residence governs; a real-time
physical-location signal is recorded but may never restrict the
operation), (b) a **real-time market-access determination** (the player's
current physical location may impose an additional restriction, and —
subject to HDR-J-8 — its absence may block the operation), (c) **both**
independently, or (d) **neither** (the operation is not
jurisdiction-dependent for this purpose at all)? The answer may differ per
operation class and must be given for all four.

**Why engineering cannot determine it:** the choice does not follow from
what the code does. Each of the four call sites makes an
availability/restriction decision, but "which evidence hierarchy is
legally authoritative for that restriction" is a different question:
option (a) makes a geolocation signal legally incapable of restricting the
operation; option (b) makes it capable of restricting it and, under
HDR-J-8, capable of denying a lawful player whenever a geolocation vendor
is unavailable. Both are coherent regulatory designs and are in force in
different jurisdictions. HDR-J-2's recorded answer establishes that the
two regimes exist and that precedence is operation-specific, but
deliberately does not say which operation falls in which regime. Guessing
(a) silently deletes a geolocation control that a licence may require;
guessing (b) invents a denial condition no regulator asked for. There is
no fail-safe default direction.

**Options:** per operation class, exactly one of: identity determination
only / market-access control only / both / neither.

**Security and regulatory impact if left open:** none today —
`RequiredPurposes` (`internal/jurisdiction/operation_purpose.go`) fails
closed for all four classes, `DeterminePlayerJurisdiction` has zero
production callers, and `resolver.go` is unchanged. If answered wrongly in
the permissive direction, the platform may accept play from a
physically-restricted location under a licence that forbids it (a
licensing exposure). If answered wrongly in the restrictive direction,
lawful players are denied on geolocation-vendor degradation and
unnecessary KYC friction is introduced.

**What is blocked until answered:** any wiring of
`DeterminePlayerJurisdiction` into `internal/casino`, `internal/bonus`,
`internal/assetregistry` or `internal/risk`; any per-operation-class
jurisdiction enforcement; the `jurisdiction_resolutions.reason` CHECK
widening (Correction 2, `docs/decisions/0043-jurisdiction-evaluation-
policy-configuration.md`). Not blocked: the Phase D configuration
infrastructure (`jurisdiction_precedence_configs`'s widened shape,
`ResolveEvaluationPolicy`/`CreateEvaluationPolicyVersion`).

---

## HDR-J-8 — Whether a physical-location signal is required, advisory, or undecided, per licensing jurisdiction and operation class

**Identifier:** HDR-J-8 — Whether a physical-location signal is required,
advisory, or undecided, per licensing jurisdiction and operation class.

**Exact question:** For each jurisdiction under which this platform (or a
tenant) is licensed, and for each operation class within it, must a usable
real-time physical-location signal be present for the operation to
proceed at all ("required": if the signal is missing, stale, inconclusive,
unavailable, or the vendor errored, the operation is refused even when the
player's verified residence is known and permitted), or may the operation
proceed on verified residence alone with the location signal applying only
when it happens to be usable ("advisory")?

**Why engineering cannot determine it:** this is the threshold at which a
licence conditions play on real-time geolocation. It is set by the
regulator/licence terms of each jurisdiction, not by the platform.
Choosing "required" without that basis invents a denial condition that
will refuse lawful players every time a geolocation vendor degrades;
choosing "advisory" without that basis silently removes a control a
licence may mandate. HDR-J-2's recorded answer establishes only that a
location signal "may impose an additional restriction and must not be
treated as a substitute for verified residence" — it does not say for
which jurisdictions or operations it must be present.

**Options:** per (licensing jurisdiction, operation class): `required` /
`advisory` / leave `unset` (the operation then cannot be evaluated at all
— fails closed).

**Security and regulatory impact if left open:** none today — the
configuration table (`jurisdiction_precedence_configs`) holds zero rows,
no code reads it into an enforcement path, and both the key-level and
field-level unset states fail closed (`ErrPolicyNotConfigured` /
`ErrPolicyUnset`). Left open indefinitely, it blocks activation of any
player-jurisdiction market-access enforcement.

**What is blocked until answered:** activation (`status = 'active'`) of
any evaluation-policy row intended to govern a real operation; any
production market-access evaluation. Also dependent: selection of a
geolocation vendor and its own security/privacy review (HDR-J-3a's
recorded condition), which is a separate decision and is not part of this
item.

**Related:** answering this is meaningless without HDR-J-9 (staleness),
because "required" is undefined without a freshness bound. The two should
be answered together, per (jurisdiction, operation class) pair.

---

## HDR-J-9 — Maximum age of a physical-location signal before it is stale, per licensing jurisdiction and operation class

**Identifier:** HDR-J-9 — Maximum age of a physical-location signal before
it is stale, per licensing jurisdiction and operation class.

**Exact question:** For each (licensing jurisdiction, operation class) for
which HDR-J-8 answers "required" or "advisory", what is the maximum age of
a physical-location observation before it must be treated as stale and
therefore unusable? Sub-question: should the platform impose its own
maximum ceiling on this value (so that no configuration can functionally
disable the freshness gate), and if so what is it?

**Why engineering cannot determine it:** the number is a
regulatory/licence parameter combined with the operational characteristics
of a geolocation vendor that has not been selected (HDR-J-3a's recorded
condition explicitly defers vendor selection and its availability
posture). Engineering can rule out incoherent values (zero, negative,
sub-second) and does — the database (`max_location_signal_age_seconds >
0` CHECK) and the write API (`CreateEvaluationPolicyVersion`) reject them
— but cannot supply a real one. Guessing short denies lawful players on
ordinary vendor latency; guessing long defeats the freshness bound that
makes a "real-time" location control real.

**Options:** a positive whole number of seconds per (licensing
jurisdiction, operation class), or leave unset (the operation then cannot
be evaluated at all — fails closed with `ErrPolicyUnset`).

**Security and regulatory impact if left open:** none today — the column
holds no rows and nothing reads it into an enforcement path. Left open, it
blocks the same activation HDR-J-8 blocks.

**What is blocked until answered:** identical to HDR-J-8; the two are
jointly required for any `active` market-access policy.

---

## Summary table

| # | Item | Question (one line) | Blocking today? |
|---|---|---|---|
| J-7 | Operation class → Purpose mapping | Which determination (identity/market-access/both/neither) does each of the four operation classes require? | No engineering blocker — `RequiredPurposes` fails closed for all four; blocks any future wiring of `DeterminePlayerJurisdiction` into a consuming domain |
| J-8 | Location-requirement threshold, per (jurisdiction, operation class) | Is a fresh location signal required, advisory, or unset? | No engineering blocker — mechanism (four-state fail-closed distinction) is built; blocks activation of any market-access evaluation policy |
| J-9 | Location-staleness bound, per (jurisdiction, operation class) | What is the maximum age before a location signal is stale? | No engineering blocker — mechanism (positive-whole-seconds validation) is built; blocks the same activation J-8 blocks, jointly |

## Cross-references

- `docs/decisions/0043-jurisdiction-evaluation-policy-configuration.md` —
  the Stage 4I Phase D ADR recording the mechanism these three items are
  blocked in front of.
- `docs/governance/stage-4i-canonical-model.md` §14.6 (PC-GAP-1/PC-GAP-2/
  PC-GAP-3, "status after Phase D" notes) and new §15 (Phase D summary).
- `docs/decisions/0041-human-decision-register-stage-4i-jurisdiction.md`
  and `docs/decisions/0042-human-decision-response.md` — HDR-J-1 through
  HDR-J-6, already answered or registered; none of those six items is
  restated or altered here.
- `internal/jurisdiction/operation_purpose.go` (HDR-J-7's seam),
  `internal/jurisdiction/evaluation_policy.go` /
  `evaluation_policy_admin.go` (HDR-J-8/HDR-J-9's mechanism).

## Ownership and scope note

Owned by `product-owner-proxy`, per `0041`'s and `0039`'s identical
convention. This document selects no answer to any of the three items
above. Once a human answers any of them, the recording of that answer
belongs in `docs/governance/stage-4i-canonical-model.md` or the ADR that
formally supersedes it for the relevant section — not in this register,
which exists to organize the questions, not to hold their answers.
