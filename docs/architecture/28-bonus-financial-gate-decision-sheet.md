# 28 — Bonus Financial Gate: Human Decision Sheet

**Status: RESOLVED (Stage 4H-B0-R3).** DS-1, DS-2, and DS-3 below were
answered by the human and independently validated against the platform's
existing architecture by `ledger-finance`, `bonus-engine`, `risk`,
`architect`, `security`, and `qa` — no contradiction, financial problem,
precision problem, reconciliation problem, or unsafe consequence was
found. **The recorded answer**: DS-1 = round-half-up (ties away from
zero); DS-2 = round once, at the final monetary boundary, full precision
until then, via an explicit function, never an implicit database cast;
DS-3 = one platform-wide rule by default, with room for a future
per-asset/jurisdiction override if genuinely required. The authoritative,
full text of the recorded decision — including the exact algorithm, where
the applied rule/version must be stored, and a small number of
non-blocking implementation-time clarifications found during
validation — lives in `docs/decisions/0021-multi-asset-accounting.md`'s
"Rounding and precision — RESOLVED" section; this document is retained
below, unmodified, as the historical record of the question as originally
put to the human. **Stage 4H-B1 is now READY FOR HUMAN AUTHORIZATION
after completion of the separate `bonus_conversion` Risk dependency
(§6 below, still NOT STARTED)** — see this stage's completion report.

Originally written Stage 4H-B0-R2, when its status was "awaiting human
decision" and documentation-only. Retained below exactly as originally
written.

**Owner of this document: Master Orchestrator, synthesizing input from
`ledger-finance`, `bonus-engine`, `risk`, `architect`, `security`, `qa`.**
No specialist selected an answer to any decision below — that is
deliberate. This document exists so a human business owner can make the
decisions that remain, with the full financial and technical consequences
laid out in plain language.

---

## 1. How to read this document

Stage 4H-B1 (building the Bonus Engine's first five bonus types) is
**CONDITIONALLY READY** — the design is finished and reviewed, but
production implementation cannot start until:

1. **You** decide how the platform rounds money when a bonus percentage
   doesn't divide evenly (§3 below — three linked questions, DS-1/DS-2/DS-3).
2. **Engineering** finishes a well-scoped, already-fully-specified
   checklist inside the Risk system (§6 below) — this is NOT a decision
   for you to make; it's listed here only so you can see it's a bounded,
   known amount of work, not an open question.
3. Nobody finds a third, unrelated problem before implementation starts
   (§7 below — checked this stage: none found).

Sections 2-5 explain the rounding decision you need to make. Section 6
explains the engineering checklist (informational only). Section 7
confirms nothing else is blocking. Section 8 restates exactly what stays
blocked until you decide.

---

## 2. Why this decision exists at all

Three of the five bonus types the platform is about to build compute a
bonus amount as **a percentage of money**:

- A **deposit bonus** — e.g. "we'll add 50% of your deposit as a bonus."
- A **reload bonus** — the same mechanic for an existing player's
  follow-up deposit.
- **Cashback** — e.g. "we'll refund 7.3% of what you lost this week."

A computer can only work with whole cents (or whatever the smallest unit
of a currency/crypto asset is). When 50% of a deposit, or 7.3% of a
loss, doesn't land exactly on a whole cent, **something has to decide
which way to round** — and that "something" has real, cumulative,
player-facing and legal consequences if the platform doesn't answer it
consistently everywhere. This document is that decision, laid out for
you to make once, in full, rather than leaving it for an engineer to
guess under time pressure later (which is exactly how inconsistent,
support-desk-generating bugs happen).

**This is not a one-off setting.** The same choice also governs: the
generic wagering bonus and coupon types' wagering-requirement/contribution
tracking (a smaller effect — see §4), agent commission payouts (a
separate future retail feature), and currency-conversion arithmetic
elsewhere in the platform. Whatever you decide here becomes the
platform's one shared rule, used everywhere money gets divided by a
percentage.

---

## 3. The decision — three linked questions

You need to answer all three. They are listed as DS-1, DS-2, DS-3 because
each is genuinely separable, but in practice most business owners will
answer DS-1 and DS-3 in one conversation, then let engineering apply
DS-2 as a technical consequence.

### DS-1 — Which way do we round?

**Question**: When the exact bonus amount isn't a whole cent (or whole
minor unit of whatever currency/crypto is involved), which way does the
platform round it — including the exact tie case (e.g. exactly half a
cent)?

**Why it matters**: This determines, on every single bonus calculation,
whether the player or the platform "wins" the fraction of a cent that
can't be paid exactly. Over millions of transactions this adds up to real
money, and some options are much easier to explain to a support agent or
a regulator than others.

**Available options** (none is recommended below — this is your call):

| Option | Plain-language description | Trade-off |
|---|---|---|
| **A. Round half up** | If it's a tie, round up. Otherwise round to the nearer cent. | Simple, easy to explain to players and support staff. On exact ties, slightly favors whoever is *receiving* the money (the player on a bonus, the operator on a fee). |
| **B. Round half to even ("banker's rounding")** | On a tie, round to whichever neighboring cent is even. | No systematic bias over many transactions — used in some accounting systems. Confusing to explain to a support agent ("why did mine round down and my friend's round up?"), and easy for different engineers/programming languages to implement inconsistently by accident. |
| **C. Always round down** | Never round up, ever — the player/operator always gets slightly less than the exact mathematical amount. | Fully predictable, the platform never pays out more than it advertised. But it means bonuses are always a hair smaller than advertised, which can look bad to players or attract regulatory attention over "advertised vs. paid" amounts. |
| **D. Always round up** | The mirror of C — always round in the generous direction. | Costs the operator a small amount on every bonus and cashback payout; rarely chosen as a blanket rule. |
| **E. Always round in the player's favor** | Round up when crediting the player, round down when taking money from the player. | The safest, most player-friendly and most regulator-friendly option. Most expensive for the operator, and the hardest for engineering to implement correctly everywhere (it requires classifying every single money movement as "to the player" or "from the player," including complex multi-part bonus transactions). |
| **F. Round down, but track and pay out the leftover fraction over time** | Pay the rounded-down amount now; keep a running tally of the leftover fractions; once the tally adds up to a full cent, pay that extra cent on a future payout. | The only option where, added up over time, nobody gains or loses anything to rounding. But it requires building genuinely new plumbing — a running ledger of fractions-owed, per player, that itself needs the same rigor (audit trail, no double-counting, safe under concurrent transactions) as the main financial ledger. It also raises a follow-up question (see the callout below) about what happens to an unpaid fraction if the player's bonus is cancelled or the player self-excludes before the fraction is ever paid out. |

> **If you choose Option F**, there is a second, smaller decision that
> comes with it and is not yet answered: what happens to a leftover
> fraction if the bonus it came from is cancelled, reversed, or the
> player self-excludes before that fraction is ever paid out? (Return it
> to a pool? Write it off with an audit note? Something else?) This isn't
> a blocker to choosing F — it's a follow-up detail engineering will need
> from you before F can be built.

**What's typical elsewhere** (an observation, not a recommendation):
Option A is the most common choice in consumer payments and gaming.
Rounding in the player's favor (E) is a common add-on specifically for
promotional credits. Option B is more common in back-office accounting
systems than in player-facing amounts. Option F is usually reserved for
situations where the leftover fractions are large enough to matter
(e.g. interest calculations, high-value crypto) — for a typical bonus
program the leftover fractions are very small.

**Recommended review owners**: you (the business owner), with input from
finance/compliance on how "advertised vs. paid" bonus amounts are treated
in your target jurisdictions.

---

### DS-2 — Where and how do we round?

**Question**: Given the direction you chose in DS-1, is that rounding
applied **once, at the very end** of a calculation (e.g. after applying
the bonus percentage, then the maximum-payout cap), or **at every
intermediate step** along the way? And separately: is the small leftover
fraction absorbed as a cost the platform bears immediately, or carried
forward the way DS-1's Option F describes?

**Why it matters**: Rounding once at the end vs. rounding at every step
can produce *different final answers* for the same input — not just a
theoretical difference, but a real one that would show up as "why did my
bonus calculate differently than I expected" if two different parts of
the system round at different points. This has to be written down
explicitly; "the obvious way" is not obvious to two different engineers.

**Available options**:

| Option | Plain-language description |
|---|---|
| **P1. Round once, at the end** | Every calculation step keeps full precision internally; only the final amount that actually gets paid is rounded. |
| **P2. Round at every step** | Each step (e.g. "apply the percentage," then "apply the cap") rounds its own result before the next step uses it. |
| **P3. Round down and carry the leftover forward** | This is DS-1's Option F, restated as the "where" answer — the leftover isn't absorbed anywhere, it's tracked. |

**Financial consequences**: P1 produces the smallest possible cumulative
rounding difference and puts the leftover fraction, if any, in an account
your finance team already reviews (the same "cost of running the bonus
program" account used for the whole bonus). P2 is easier to show a
player a step-by-step breakdown of ("here's your 50% match, here's the
cap applied"), but the deviation compounds across steps, and the exact
final amount then depends on which order the steps happen in — which
itself becomes something that has to be fixed and never silently changed.
P3 is DS-1 Option F's mechanism (see that entry).

**Recommended review owners**: `ledger-finance` can implement whichever
you choose; the decision itself is yours, informed by whether you want a
player-facing "your bonus breakdown" screen to show step-by-step rounded
numbers (favors P2) or whether internal accuracy matters more than a
step-by-step display (favors P1).

---

### DS-3 — Is there one rule, or does it vary?

**Question**: Is there exactly **one** rounding rule for the whole
platform, or can it vary — for example, one rule for one crypto asset
and a different rule for euros, or a different rule required by a
specific country's regulations, or a different rule depending on whether
money is moving to the player or away from them (which is really DS-1's
Option E again, stated as a scope question)?

**Why it matters**: Some countries have specific legal rules about how
cash amounts must be rounded (this matters more for a future retail/cash
feature than for online bonuses, but the same underlying decision governs
both). If you say "it can vary," every variant becomes a permanent rule
that has to be remembered and applied correctly forever — including for
old transactions that must still be checked against whichever rule
applied *at the time*, not today's rule.

**Available options**: (a) exactly one platform-wide rule, simplest to
operate; (b) the rule may vary by currency/asset, by country, and/or by
direction (to-player vs. from-player) — more flexible, but each variant
is a permanent, auditable rule the platform must track forever.

**Recommended review owners**: you, with input from compliance on
whether any target country legally requires a specific cash-rounding
convention (this is more relevant to the future retail/cash feature than
to online bonuses specifically, but the platform uses one shared
mechanism for both, so the two questions are linked).

---

## 4. Which bonus types does this actually affect?

Verified directly against the bonus engine's frozen design
(`docs/architecture/10-bonus-engine-architecture.md`, `docs/decisions/
0032-bonus-accounting.md`) by `bonus-engine` this stage. The effect is
**not identical across all five types** — please read this table rather
than assuming a blanket "all five are affected the same way":

| Bonus type | Uses money × percentage? | Uses rounding? | Reaches bonus conversion? | Posts to the ledger? | Depends on your decision above? |
|---|---|---|---|---|---|
| **Deposit bonus** | Yes — the grant amount itself (e.g. 50% match), plus the wagering-requirement/contribution tracking that follows | Yes | Yes | Yes | **Yes — the grant amount itself.** |
| **Reload bonus** | Yes — identical mechanism to Deposit bonus | Yes | Yes | Yes | **Yes — the grant amount itself.** |
| **Cashback** | Yes — the payout amount (e.g. 7.3% of net loss); does not use a wagering-requirement calculation | Yes | Yes | Yes | **Yes — the payout amount itself.** |
| **Generic wagering bonus** | The grant amount itself is a flat, pre-set amount, not a percentage — **no**. But the wagering-requirement target and per-game contribution tracking that follow it do multiply money by a percentage | Yes, for the wagering-requirement/contribution tracking only | Yes | Yes | **Only for the wagering-requirement tracking, not the grant amount.** |
| **Coupon** | The grant/face amount is a flat, pre-configured amount — **no**. Same wagering-requirement/contribution caveat as the generic wagering bonus applies once it activates | Yes, for the wagering-requirement/contribution tracking only | Yes | Yes | **Only for the wagering-requirement tracking, not the grant amount.** |

**In short**: your decision directly changes the bonus amount a player
receives for Deposit, Reload, and Cashback bonuses. For the generic
Wagering bonus and Coupon, the amount the player receives is unaffected
(it's a flat, pre-set number) — your decision only affects the invisible
internal tracking of how much wagering they still need to complete.

All five types reach "bonus conversion" (the step where a bonus becomes
real, withdrawable money) — confirmed by `bonus-engine`, cross-checked
against `risk`'s independent verification. This is why the separate
engineering checklist in §6 blocks all five types, not just three of
them.

---

## 5. A worked numerical example

To make this concrete rather than abstract (worked by `ledger-finance`
against the platform's actual currency-handling model — money is stored
as whole cents internally, never as a fraction):

**A player deposits €133.33 and is offered a 50% match bonus.**

The exact match amount is €66.665 — exactly halfway between €66.66 and
€66.67. This is the "tie" case DS-1 has to answer.

| If you chose... | The player receives | What happened to the extra half-cent |
|---|---|---|
| A. Round half up | €66.67 | Rounded up — a tiny cost to the operator |
| B. Round half to even | €66.66 | Rounded to the nearer *even* cent |
| C. Always round down | €66.66 | Player always gets slightly less than the exact 50% |
| D. Always round up | €66.67 | Player always gets slightly more |
| E. Always round in the player's favor | €66.67 | Rounded up, because this is a payment *to* the player |
| F. Round down + carry forward | €66.66 now | The extra half-cent is added to a running tally that pays out once it reaches a full cent from future bonuses |

**A second example — cashback paid weekly, showing why Option F needs
new plumbing:** a player loses €16.90 per week, with 7.3% weekly
cashback. The exact cashback is €1.2337 each week — always leaving a
0.37-cent fraction. Under Option F, after three weeks the accumulated
fractions (0.37 + 0.37 + 0.37 = 1.11 cents) cross a full cent, so the
third week's payout is €1.24 instead of €1.23, with 0.11 cents carried
forward again. Added up over time, this is the only option where nobody
gains or loses anything — but notice it requires the platform to keep a
small, per-player running balance of "cents owed," which does not exist
anywhere in the system today and would need to be built with the same
care as the main financial ledger (an audit trail, protection against
being read and updated by two transactions at once, and a clear answer
to what happens to it if the player's account closes).

**A technical note worth knowing**: the database software the platform
uses will, by default, automatically apply Option A (round half up) if a
number like €66.665 is simply stored without an explicit rounding step
written into the code. This means whichever option you choose **must**
be built as a deliberate, explicit step in the code — it cannot be left
to "whatever the database does automatically," or the platform will
silently use Option A regardless of what you decided.

---

## 6. The other blocker: a Risk-system checklist (NOT a decision for you)

This section is included for transparency, not because it needs your
input. `risk` re-verified this stage that a piece of internal plumbing
called `bonus_conversion` — the step where Risk checks a bonus payout
before it becomes real money — has **not been started at all**: zero of
six required engineering steps are done. This blocks all five bonus
types (§4), independent of the rounding decision.

The good news: this is fully scoped, bounded work with no open design
questions — just work that hasn't been scheduled yet.

| # | Step | Who does it | What it touches | Can start before other decisions are made? |
|---|---|---|---|---|
| 1 | Let the database accept this new type of rule | `risk` | A new, additive database migration | Yes |
| 2 | Add the internal code name for it | `risk` | `internal/risk/types.go` | Yes |
| 3 | Let the admin API accept it | `risk` | `internal/httpserver/risk_handlers.go` | Yes |
| 4 | Update the public API documentation (in all three places it's listed) | `risk` | `docs/api/openapi/platform-api.yaml` | Yes |
| 5 | Wire it into Risk's "running total" limit checks | `risk` | `internal/risk/evaluator.go` | Only after the Bonus accounting team (`ledger-finance`) finalizes the related ledger entry types — can be safely postponed to a later slice if the business accepts a documented, fail-safe limitation in the meantime |
| 6 | Actually call the Risk check before releasing bonus money | `bonus-engine` | New code inside the not-yet-built Bonus Engine | Only once the Bonus Engine itself exists — this cannot happen first |

All six steps land together as one authorized change when Stage 4H-B1 is
approved; steps 1-4 could technically be built slightly ahead of time,
but engineering practice on this project is not to build a rule nobody
uses yet, so in practice this checklist completes as part of Stage
4H-B1, not before it.

---

## 7. Anything else blocking implementation?

`architect` performed a focused check this stage across ledger
accounting, the wallet system, duplicate-transaction protection,
concurrent-access safety, Risk, Responsible Gaming, audit logging,
tenant-data isolation, reconciliation, and the database schema itself.

**Result: no additional blocker was found.** Everything needed for the
five bonus types — beyond the rounding decision and the Risk checklist
above — is already fully designed and ready to build.

Two small, non-blocking items were noted for engineering's own tracking
(not gates, not decisions for you):

- `security` found that the bonus system's audit-logging design (which
  records who did what, and when, for compliance) is missing one
  explicit entry for "a bonus was reversed" — everything else is fully
  covered, this is a documentation completeness fix, not a missing
  capability.
- `qa` found a handful of test-plan gaps (e.g. no test yet planned for
  "what if the same coupon code is redeemed twice at the exact same
  moment") that engineering should add to its test plan before
  implementation, but that don't change what's being built or require
  any decision from you.

---

## 8. What stays blocked until you decide

**Stage 4H-B1 (building the Bonus Engine) remains BLOCKED until:**

1. You approve an answer to DS-1, DS-2, and DS-3 above.
2. The engineering checklist in §6 is completed and independently
   reviewed (no action needed from you — this is scheduled as part of
   Stage 4H-B1's own work).
3. Nothing new turns up in a final check before implementation begins
   (checked this stage — nothing found; re-confirmed at the start of
   Stage 4H-B1 as standard practice).

Work that doesn't calculate a bonus amount from a percentage (e.g.
building the basic plumbing for a bonus's lifecycle, or how bonuses are
configured by staff) is not blocked by the rounding decision and may be
scoped independently — but a **complete, working** Bonus Engine cannot
ship without it, since Deposit, Reload, and Cashback are 3 of the 5
required types.

---

## 9. Specialist reviews completed this stage

| Specialist | What they confirmed |
|---|---|
| `ledger-finance` | Numerical worked examples; confirmed no rounding option breaks the platform's core financial rule (every debit must equal every credit); confirmed the five-type first slice is otherwise fully compatible with the existing ledger, wallet, and reconciliation design |
| `bonus-engine` | Exactly which of the five bonus types are affected by the rounding decision, and how; confirmed all five reach bonus conversion |
| `risk` | Re-verified the Risk-system checklist (§6) is still accurate and unstarted; confirmed it is the only Risk-side blocker |
| `architect` | Confirmed no other blocker exists across ledger, wallet, concurrency, Risk, RG, audit, tenant-isolation, reconciliation, and schema design |
| `security` | Confirmed no security blocker; flagged that whichever rounding rule is chosen must be stored as a permission-controlled, audit-logged setting once it's built; found one minor audit-documentation gap (non-blocking) |
| `qa` | Confirmed the core financial test plan is solid; flagged a handful of additional test-plan items to add before implementation (non-blocking, no decision needed) |

No specialist selected an answer to DS-1, DS-2, or DS-3. That decision is
reserved for you.
