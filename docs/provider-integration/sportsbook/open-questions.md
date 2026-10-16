# Sportsbook package: minimum decisions and inputs

Each item: the question, why it is needed, and `sportsbook`'s recommendation. The owner decides. **All items are parked
until a sportsbook brand is scheduled** (owner decision 2, 2026-10-15); OQ-SB-02 is the only one that is needed to lift
the deferral. Nothing here is decided by this package.

| Id | Needed by | Question | Recommendation |
|---|---|---|---|
| OQ-SB-01 | S0 | Operating model: (a) the platform books against a sports-data feed and an odds source (it accepts bets in-house at catalogue odds and carries the liability, as the code does today), or (b) a sportsbook vendor accepts/prices/settles and the platform is the wallet and gate (as with casino), or (c) a hybrid | Decide before any vendor conversation. (a) matches the code and ADR 0117 but makes the tenant carry book risk and requires a trading/odds source and a liability limit policy, which the platform does not own. (b) shifts risk and trading to the vendor but needs the placement/pending/settlement-with-vendor-payout work (SB-G01..G03). Recommendation: (b) for a first brand because the platform "does not own odds/trading" (CLAUDE.md), unless the owner wants to carry risk |
| OQ-SB-02 | S0 | Is a sportsbook brand scheduled, when, and for which licence/jurisdiction? | Not now (owner decision 2). Revisit after the first casino brand is live and reconciled. Without it no vendor contact should happen |
| OQ-SB-03 | S1, S2 | First-brand product scope | Pre-match, fixed-odds singles, cash-funded only, no live, no cashout, no multiples, no bonus/free bets. This is exactly what the platform supports today and avoids SB-G05 |
| OQ-SB-04 | S1 | If model (a): who supplies prices and results (a sports-data provider, see `../sports-data/`) and who is responsible for pricing and suspension? | A licensed odds/data provider with contractual SLAs; the platform never prices. Decide together with the sports-data package; the settlement-result trust rules (mandatory hold, four-eyes confirmation) are in ADR 0117 C.2 |
| OQ-SB-05 | S2/S5 | Provider-keyed settlement identity (SB-G02): how will the vendor's event be told from a resend? | Require the vendor to supply a per-bet occurrence ordinal or version (U-SB-07); if it cannot, reject the vendor for provider-driven settlement or accept manual settlement. Do not infer ordinals from timing |
| OQ-SB-06 | S5 | Vendor payouts that differ from stake x frozen odds (SB-G03: dead heat, deductions, boosts, cashout) | Refuse them in the first brand (existing anti-minting). If the vendor's product requires them, `ledger-finance` designs an explicit adjusted-payout settlement with evidence and four-eyes; no silent acceptance |
| OQ-SB-07 | S5/S8 | Open-liability reporting (SB-G08) and exposure arming (HDR-SB-1) before real money | Build the open-liability line before S8; set exposure limits for any book the tenant carries. `risk` + `ledger-finance` + `data-analytics` propose; the owner approves the values (no invented limit) |
| OQ-SB-08 | S5 | Sportsbook statement source (SB-G07): fetcher shape, key mapping, coverage anchor, mismatch kind | Require a per-bet report with the vendor's bet reference and a stated cut-off; build the registry/stored copy/real mismatch kind with the first source. `ledger-finance` approves any weaker semantics explicitly |
| OQ-SB-09 | S8 | Player cancel and self-exclusion with open bets (SB-G09) | Decide per licence with compliance; the default today is no auto-void and settlement continues |
| OQ-SB-10 | S6 | One operator account per tenant? | Yes for any tenant that goes live; required for callback tenant binding |

## Inputs (not decisions) the owner must eventually supply

Listed in `integration-gates.md` ("Exact inputs needed"): the scheduling decision, vendor documentation, sandbox terms and
credentials via the secret store, synthetic events and scenarios, written vendor answers to the open documentation
items, and later (S9 only) the signed contract and production credentials. None is requested now.
