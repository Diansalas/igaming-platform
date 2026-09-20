// DISPLAY-ONLY odds/return math. The server's integer numerator/
// denominator fraction (e.g. 5/2) is the only value ever sent back to the
// server (expected_odds_numerator/expected_odds_denominator) or trusted
// as authoritative; everything here exists purely so a human can read a
// price and get an immediate stake-entry estimate. Once a bet actually
// exists, the UI always renders the SERVER's own `potential_return` and
// `odds_numerator`/`odds_denominator` from the bet record - never this
// module's estimate - per the Stage 6 directive.

/**
 * e.g. numerator=250, denominator=100 -> "2.50" (decimal odds). The
 * numerator/denominator ratio IS the decimal odds value directly - it
 * already includes the stake (a 2.50 selection returns 2.5x the stake
 * total, not 2.5x in addition to it) - matching the server's own
 * computePotentialReturn exactly (internal/sportsbook/orchestrator.go:
 * stake * numerator/denominator, no "+1"). Do not add 1 here.
 */
export function formatDecimalOdds(numerator: number, denominator: number): string {
  if (denominator <= 0) return '-'
  const decimal = numerator / denominator
  return decimal.toFixed(2)
}

/**
 * A DISPLAY-ONLY potential-return estimate in the same minor units as the
 * stake, for immediate bet-slip feedback before submission. Rounds down
 * (a player should never be shown a return estimate more generous than
 * what a real settlement could pay) - the server computes and returns the
 * real, authoritative `potential_return` once a bet is actually placed.
 * Mirrors the server's stake * numerator/denominator exactly - see
 * formatDecimalOdds's own comment for why there is no "+1" here.
 */
export function estimatePotentialReturnMinorUnits(stakeAmountMinorUnits: number, numerator: number, denominator: number): number {
  if (denominator <= 0 || stakeAmountMinorUnits <= 0) return 0
  return Math.floor((stakeAmountMinorUnits * numerator) / denominator)
}
