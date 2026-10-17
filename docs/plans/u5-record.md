# U-5 record: float-free money in b2c/ and backoffice/ (branch u5-money)

Status: IMPLEMENTED (frontend only; no Go, no migration, no ADR edit).

## Findings
Wire format (internal/httpserver DTOs and docs/api/openapi/platform-api.yaml):
- Decimal STRINGS of minor units (safe already): manual-adjustment / payment-attempt / hold-resolution / force-resolution
  `amount_minor_units`, reconciliation expected/actual, bonus `amount_at_request` / `remaining_bonus_balance`, RG limit
  `value` / `ceiling`, casino callback rejection `amount`.
- JSON NUMBERS declared `integer int64` in OpenAPI and Go `int64` (can exceed 2^53, JSON.parse silently rounds):
  WalletSummary cash/available/held_for_withdrawal/locked/bonus; DepositIntent.amount; WithdrawalPlayer/WithdrawalAdmin.amount;
  WithdrawalRequestCreate.amount; PlaceBetRequest.stake_amount; Bet stake_amount/potential_return/payout_amount;
  BetSettlementLifecycleEntry.payout_amount; Round bet/win/rollback_amount; WriteProviderCapabilityRequest amount_limits
  min_amount/max_amount; SportsbookExposureLimit.max_open_potential_payout (no UI uses it); casino play stake/win amounts.
  None are declared as strings. Go API not changed (out of scope); this is reported for backend/architect to decide whether to
  move them to strings later. The frontend guard below makes the UI safe either way (a future string wire form needs no change).

Float uses found: b2c formatMoney (`/ 10**e` + toFixed), toMinorUnits (`Number()` + `Math.round(v * 10 ** e)`; also accepted
"1e2", " 5", ".5"), odds estimate (`Math.floor(stake*num/den)`); backoffice formatMoney (`/ 10**e` + toFixed),
PaymentCapabilityForm min/max limits (`Number(text)`). Already exact: RG limit helpers, formatMinorUnits, groupDecimalString.
No client-side balance/limit comparisons exist (enforcement is server-side). Staff adjustment/approval forms do not exist in
the Back Office UI yet; reconciliation views only format strings (exact).

## Design
- `src/lib/amount.ts` (byte-identical in both apps; a test asserts it): parseMinor, parseDecimalToMinor (exact string
  arithmetic, rejects excess fractional digits, garbage, > 38 digits, optional negatives), formatMinor (BigInt, optional
  grouping, no Intl), compareMinor/minorGreater (BigInt), toWireInteger (bigint -> JSON number only if exactly safe),
  parseJsonExact (replaces any unsafe integer literal with the exact digit string before JSON.parse; strings untouched).
- apiFetch in both apps parses through parseJsonExact. Amount response fields are typed `MinorAmount = number | string | bigint`.
- Outbound int64 request fields are still JSON numbers (the DTOs require numbers): toMinorUnits returns null (input refused)
  above 2^53-1 minor units, never a rounded value. PaymentCapabilityForm limits likewise refused above 2^53-1.
- formatMoney (both apps) and formatMinorUnits delegate to formatMinor; non-exact input is shown raw, never scaled.
- Odds estimate multiplies in BigInt; returns a safe number or an exact digit string.
- Static guard `src/lib/noFloatMoney.test.ts` in both apps: bans parseFloat, Number(, parseInt, toFixed, Math.round/floor/ceil/trunc,
  `* 100`/`/ 100`, `10 **` in production source; explicit audited ALLOW list with reasons (all non-money: durations, page
  sizes, priorities, retry-after, odds display, the one lossless bigint->number in toWireInteger); stale-entry check; allow
  entries may not mention amount-like words; scanner self-test.

## Existing tests
No existing assertion changed or weakened. Display output is unchanged for all previously-valid inputs (verified by the
untouched suites). Input parsing is intentionally stricter: toMinorUnits no longer accepts exponent notation, a leading '+',
".5" or "5." (Number() used to); none were exercised by existing tests.

## Verification (worktree .claude/worktrees/u5)
- backoffice: vitest 687 passed (was 644; +43: amount 37, noFloatMoney 4, money 2); tsc --noEmit -p tsconfig.app.json clean; npm run build OK.
- b2c: vitest 185 passed (was 140; +45: amount 37, noFloatMoney 4, AccountPageBigInt 1, rgMoney 2, odds 1); tsc clean; npm run build OK.

## Residuals
- Amounts above 2^53-1 minor units cannot be SENT (int64 DTOs need JSON numbers); they display exactly. Crypto-scale entry
  (exponent 8/18) is deferred with crypto; the clean fix is a backend move of those DTO fields to decimal strings (needs a decision).
- `<input type="number">` is still used for player amount entry; browsers hand back a string, the strict parser rejects forms like "1e2".
- Known-exponent table in b2c is still a client mapping (no exponent registry endpoint); unknown assets are refused outbound.
- Proposed registry text: "U-5 money handling in frontends: IMPLEMENTED (shared lib/amount.ts, parseJsonExact guard, static ban test);
  PARTIALLY: >2^53 outbound amounts refused pending backend string DTOs."
