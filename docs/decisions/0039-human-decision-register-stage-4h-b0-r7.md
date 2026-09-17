# 0039 — Human Decision Register (Stage 4H-B0-R7)

Status: **Record only. No decision is selected in this document.** Owner:
`product-owner-proxy`, Stage 4H-B0-R7 Workstream F ("a formal Human
Decision Register for the three human decisions that remain unmade after
Stage 4H-B0-R6"). This document organizes three pre-existing, already-
raised human decisions into one place for a non-technical decision-maker
to answer. It does not introduce a fourth decision, does not restate any
technical architecture already frozen elsewhere, and does not recommend
an answer to any of the three — per this workstream's explicit mandate,
recommending would cross into `security`'s, `identity-compliance`'s, or
`bonus-engine`'s domain expertise, or into a legal/compliance judgment
this role has no authority to make.

Each decision below already exists in the architecture record; this
document's only original content is the organization, the plain-language
question framing, and the "what is actually blocked" analysis in each
decision's final subsection. Where prior stages named specific candidate
options, this document uses those exact names — it introduces no new
option names.

---

## Decision 1 — `OpenBetSelfExclusionPolicy` platform-wide default

### The question

When a jurisdiction, tenant, or brand has configured **no explicit
value** for what happens to a self-excluding player's currently-open
sportsbook bets, what should the platform apply as the fallback default:
**`SETTLE_NORMALLY`** or **`VOID_ON_SELF_EXCLUSION`**?

This is a fallback-only decision. Any jurisdiction may still be
configured later with its own explicit, different value once that
jurisdiction's legal/compliance requirement is known (a jurisdiction's
configured value is always a floor a tenant/brand may tighten but never
loosen — ADR 0034 §14.2). This decision only governs what happens where
nobody has yet supplied a jurisdiction-specific answer.

### The options (named exactly as ADR 0034 §14.1 names them)

- **`SETTLE_NORMALLY`** — the bet proceeds to its natural settlement (WIN/
  LOSS/push/partial-settlement) exactly as if self-exclusion had not
  occurred.
- **`VOID_ON_SELF_EXCLUSION`** — the bet is voided immediately when
  self-exclusion becomes effective; the full stake is returned to the
  player.

### Financial consequences

- **`SETTLE_NORMALLY`**: the bet's outcome (win or loss) is realized
  normally through the existing settlement posting path (ADR 0038 §5) —
  no stake is returned early, and the player may still win or lose money
  on a bet placed before they self-excluded. Whether the player can then
  *withdraw* a resulting winning balance is a separate, currently
  unaddressed question (ADR 0034 §14.6 confirms no rule links
  self-exclusion status to withdrawal eligibility today) — not decided by
  this policy either way.
- **`VOID_ON_SELF_EXCLUSION`**: the full stake is unwound and returned via
  the existing void posting shape (`Dr player_locked / Cr
  player_cash`/`player_bonus`, ADR 0038 §8.1) — the house never realizes
  the win or loss on that specific bet. For a bonus-funded stake, there is
  a known, separately-tracked gap (the wagering-progress debit taken at
  lock time is not netted against this reversal, so the player keeps
  wagering credit for a stake never actually risked) — this gap exists
  independently of this decision, but choosing this as the platform-wide
  default would make it fire at materially higher volume.
- Both values are ledger-balanced by construction; neither risks
  `SUM(DEBITS) == SUM(CREDITS)`.

### Technical consequences

**Both values are already fully expressible by the existing architecture
with no further design or code change required to select one over the
other as the default** (ADR 0034 §14, confirmed): the configuration is a
scoped, versioned row resolved fresh at the instant self-exclusion
becomes effective, and `SETTLE_NORMALLY` requires literally zero new
financial code path while `VOID_ON_SELF_EXCLUSION` reuses the existing
void posting shape verbatim with a new sub-reason tag. Setting the
default is a configuration-value decision, not a schema or code decision,
regardless of which value is chosen.

This does **not** mean the surrounding mechanism is finished. Independent
of which default is chosen, `security` has already flagged two open P1
gaps against the mechanism itself (`docs/security/security-
architecture.md`, Stage 4H-B0-R5 section): **S-8** (the policy version
must be anchored to self-exclusion's own effective timestamp, not
resolved fresh against `now()` at listener-run time — a tampering
window as currently specified) and **S-9** (a single per-bet audit
record cannot prove the open-bet enumeration was complete — a dropped
event leaves no evidence anything was missed). `sportsbook` has also
flagged that `VOID_ON_SELF_EXCLUSION` is not fully specified for a
multi-leg bet caught mid-partial-settlement (ADR 0038 "Open decisions
referred upward," item 7). None of these three gaps is evidence for or
against either default value — they are pre-existing engineering tasks
that must close before either value can be safely enforced in production.

### Regulatory / compliance implications

This is a responsible-gaming control with real regulatory weight, not a
purely commercial or UX choice. ADR 0034 §14.9 is explicit that it has no
visibility into Anjouan's, or any candidate Europe/LATAM jurisdiction's,
specific regulatory text on this exact question, and does not assume an
answer either way — this document takes the same position. Per CLAUDE.md's
"When to stop and ask" section, jurisdiction-specific legal interpretation
of what a self-excluding player's open positions must do is outside
engineering's (and this role's) competence and needs jurisdiction-specific
legal input before being finalized for any jurisdiction beyond the
platform-wide fallback.

### Where implementation actually stands

**Nothing is blocked on the engineering side merely because this value is
unset.** The configuration architecture, both financial code paths, the
audit trigger, and the jurisdiction-floor override mechanism are already
specified regardless of which value ends up as the default. What this
decision actually gates is **launch/feature-enablement**, not code: per
`security`'s own framing, "a permissive default combined with absent
configuration resolving to it" is the kind of thing that needs a human's
eyes before sportsbook self-exclusion handling is switched on for any
jurisdiction that has not been given its own explicit configuration.
Separately, the S-8/S-9/multi-leg gaps above are real open engineering
work items for `identity-compliance`/`sportsbook` that exist regardless of
this decision and are not resolved by making it.

---

## Decision 2 — Terminal-Grant settlement-credit resolution rule

### The question

When a settlement or void credit arrives against a bonus Grant that has
**already gone terminal** (expired, cancelled, or forfeited) — because a
portion of that Grant's value remained locked inside an open sportsbook
bet whose settlement or void timing didn't line up with the Grant's own
lifecycle — what should the platform do with that credit?

### The options (named exactly as `docs/architecture/10-bonus-engine-
architecture.md` §5 and `docs/architecture/ledger-accounting-model.md`
§5's open item 7 name them)

- **(a) Re-forfeit the credit immediately** on arrival against the
  terminal Grant.
- **(b) Route it to `player_cash`** as a mechanical settlement of an
  already-earned entitlement.
- **(c) Hold it for manual review** in a staff queue pending a per-Grant
  human decision.

### Financial consequences

- **(a)**: the newly-arrived amount posts `Dr player_bonus / Cr
  promo_liability` (ADR 0032 §5's existing forfeiture shape) — the
  player never receives the value; it is retained as promotional
  liability, consistent with how an ordinary expired/cancelled Grant's
  outstanding balance is already forfeited.
- **(b)**: the credit becomes withdrawable cash. This mirrors a precedent
  this same ADR already adopted for the closely related case of an
  already-fully-satisfied wagering requirement completing after
  self-exclusion — where auto-forfeiting a fully-wagered-through balance
  the instant a terminal condition hits was explicitly rejected as
  creating a perverse incentive (delaying self-exclusion to avoid losing
  the balance).
- **(c)**: no immediate final disposition; the value sits in an
  intermediate/held state until a staff member decides, with the eventual
  outcome being either (a) or (b) posted later, plus an audit trail of
  who decided and why.
- All three reuse an existing, already-proven-balanced posting shape;
  none has been shown to threaten `SUM(DEBITS) == SUM(CREDITS)`.

### Technical consequences

**Options (a) and (b) are close to decision-agnostic from a mechanism
standpoint** — each reuses a posting shape that already exists elsewhere
in the ledger (ADR 0032 §5's forfeiture posting for (a); the
already-adopted mechanical-entitlement-settlement pattern for (b)). For
both, the primary remaining engineering work is the same regardless of
which is chosen: defining the missing Grant state-machine transition
itself (doc 10 §1.2 currently has no transition at all for "a terminal
Grant receives a late credit").

**Option (c) is not fully symmetric with (a)/(b) on the evidence
currently available.** Doc 10's own Open Questions (item 5) states that
"the manual-adjustment four-eyes-approval workflow... is a requirement,
not a design — belongs to a future backoffice/RBAC implementation stage."
That workflow does not yet exist as a designed mechanism. Choosing (c)
would therefore additionally depend on that not-yet-designed backoffice
capability being built and operable, on top of the same state-machine
transition work (a)/(b) also need. This is a materially different
technical consequence than "none, the mechanism already supports either,"
and this document states it precisely rather than assuming symmetry.

**Caveat on this section's currency**: at the time this register was
written, Workstream C's Terminal-Grant technical contract (this same
Stage 4H-B0-R7 round, reserved migration `0051`) had not yet landed in the
repository. The technical-consequence analysis above is grounded in the
existing cross-reference notes (doc 10 §5, `ledger-accounting-model.md`
§5 item 7 and §6.3/§6.4) rather than a completed technical contract for
this exact gap. It should be re-confirmed against that contract once it
lands, rather than treated as final.

### Regulatory / compliance implications

Lower direct regulatory weight than Decision 1 — this is an edge-case
timing gap, not a whole-policy default. It does carry a consumer-
protection dimension (does the platform retain value a player could
argue they legitimately earned, versus returning it) that could interact
with fair-treatment expectations in a given jurisdiction. This document
does not assert a specific legal requirement either way; it flags the
question as being in the same *category* as Decision 1, at smaller scale.

### Where implementation actually stands

This is **closer to a genuine implementation blocker than Decision 1**,
not merely a configuration-value gate. Per Stage 4H-B0-R6's record, two
independent specialists (`bonus-engine`, `sportsbook`) confirmed this gap
— tracked there as gate **G-2** — blocks **bonus-funded sportsbook
wagering directly**, not only the already-deferred mixed-funding case.
`player_locked` phase 2 code for the bonus-funded case is not being built
or enabled until this is resolved. **Cash-only sportsbook wagering is
unaffected and remains unblocked** regardless of how this decision is
answered.

---

## Decision 3 — Mixed cash/bonus-funded sportsbook cashout policy

### The question

If and when the platform ever builds (1) mixed cash-and-bonus funding for
a single sportsbook bet, and (2) an early cashout/buyout feature for
sportsbook bets — neither of which exists today — how should a mixed-
funded bet's cashout proceeds be handled: split proportionally between
cash and bonus wallets, paid entirely to cash, or should such bets simply
not be offered a cashout at all?

### The options (named exactly as `docs/architecture/ledger-accounting-
model.md` §6.3.3.2 case C-cashout names them)

- **Proportional split** — cashout proceeds are split between
  `player_cash` and `player_bonus` in the same ratio as the original
  cash/bonus funding mix (mirrors the lock-time split rule).
- **All-to-cash** — cashout proceeds are paid entirely to `player_cash`
  regardless of the original funding mix, on the theory that a cashout is
  a voluntary buyout of a contract for cash.
- **Not cashout-eligible** — mixed/bonus-funded bets are simply excluded
  from the cashout feature upstream; the split question never arises.

### Financial consequences

- **Proportional split**: part of the buyout proceeds lands in restricted
  (`player_bonus`) funds, remaining subject to any outstanding wagering
  requirement. The candidate posting shown in `ledger-accounting-model.md`
  §6.3.3.2 is proven ledger-balanced (Invariant B1 holds).
- **All-to-cash**: the entire buyout becomes immediately withdrawable
  cash, including the portion tracing back to bonus-origin value. This
  candidate is also ledger-balanced, but `bonus-engine`'s review flagged
  it as a **named wagering-requirement bypass / bonus-abuse vector**: a
  player could time the buyout specifically to convert bonus-origin value
  into cash.
- **Not cashout-eligible**: no proceeds-split posting is ever created, so
  no financial exposure of this kind arises for mixed-funded bets at all.

### Technical consequences

- **Proportional split** and **all-to-cash** both require building
  cashout ledger-posting logic that does not exist today (no sportsbook
  cashout code exists in any mode, external or in-house). Proportional
  split additionally requires an anti-structuring control `bonus-engine`
  has already flagged as necessary before implementation (the same
  concern raised for the related C-win case: a cash-dominant/bonus-sliver
  stake can be structured so the bonus share rounds to zero, effectively
  laundering bonus value into cash on demand).
- **Not cashout-eligible** requires only an upstream eligibility check in
  `internal/sportsbook` — no new ledger posting shape at all.

### Regulatory / compliance implications

Potentially a consumer-protection and bonus-abuse question (per
`bonus-engine`'s flagged concern on all-to-cash), and possibly a
jurisdiction-rules question about whether bonus funds may be cashed out
at all in a given jurisdiction. This document does not assert a specific
legal requirement.

### Where implementation actually stands

**Nothing is blocked, on any timeline, by this decision remaining
unmade.** Per Stage 4H-B0-R6's own record, mixed cash+bonus funding is
deferred entirely for the initial implementation (a fail-closed rejection
at bet placement), and no sportsbook cashout code exists in any form.
This decision only becomes relevant if and when the platform separately
decides to build **both** mixed funding and a cashout feature — two
distinct, larger, currently unscheduled pieces of work. This is
registered formally per the stage directive's request, but is explicitly
the lowest-urgency of the three decisions in this register.

---

## Summary table

| # | Decision | Options | Blocking engineering today? |
|---|---|---|---|
| 1 | `OpenBetSelfExclusionPolicy` default | `SETTLE_NORMALLY` / `VOID_ON_SELF_EXCLUSION` | No — architecture is decision-agnostic; blocks launch/feature-enablement for jurisdictions without their own explicit config, not code |
| 2 | Terminal-Grant settlement-credit resolution | (a) re-forfeit / (b) route to `player_cash` / (c) hold for manual review | Partially — independently confirmed (gate G-2) to block bonus-funded sportsbook wagering specifically; cash-only sportsbook unaffected |
| 3 | Mixed-funded sportsbook cashout policy | proportional split / all-to-cash / not cashout-eligible | No — mixed funding is already deferred and no cashout code exists in any mode; relevant only if both are built later |

## Cross-references

- `docs/decisions/0034-bonus-gamification-rg-kyc-identity-integration.md`
  §14 (Decision 1's configurable architecture, in full).
- `docs/security/security-architecture.md`, Stage 4H-B0-R5 section,
  findings S-8/S-9 (Decision 1's surrounding open engineering gaps).
- `docs/architecture/10-bonus-engine-architecture.md` §5 (cross-reference
  note) and its Open Questions item 7 (Decision 2, options as named).
- `docs/architecture/ledger-accounting-model.md` §6.3.3.2 case C-cashout
  and §6.4's Stage 4H-B0-R6 forward pointer (Decision 3, options as
  named; the mixed-funding deferral and cashout non-implementation this
  decision's urgency is scoped against).
- `docs/governance/task-registry.md`, Stage 4H-B0-R7 section (this
  workstream's origin and the reserved Workstream C migration `0051` for
  the Terminal-Grant technical contract this register's Decision 2 will
  need to be re-checked against once it lands).

## Ownership and scope note

Owned by `product-owner-proxy`. This document selects no answer to any of
the three decisions above, and none should be inferred from option
ordering or presentation — options are listed in the order prior stages
introduced them, not in a ranked or recommended order. Once a human
answers any of the three, the recording of that answer belongs in the
document each decision's options were originally drawn from (ADR 0034
§14.9 for Decision 1; doc 10 §5 / `ledger-accounting-model.md` for
Decision 2; `ledger-accounting-model.md` §6.3.3.2 for Decision 3) — not
in this register, which exists to organize the questions, not to hold
their answers.
