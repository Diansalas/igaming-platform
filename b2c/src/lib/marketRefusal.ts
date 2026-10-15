import { ApiError } from '../api/types'

/**
 * The ONE sentence a player sees when the operating-market gate refuses registration, a deposit or a
 * game launch (A8; API code MARKET_NOT_AVAILABLE, HTTP 403). It is owned by the UI and deliberately
 * ignores the server's `message`: the server answers identically for "your country is outside our
 * market", "this brand has no market configured" and "we could not determine your location", and the
 * UI must not add any detail that would tell a player which one it was (no country, no reason, no
 * hint to retry with another network).
 */
export const MARKET_NOT_AVAILABLE_MESSAGE = 'This service is not available in your region.'

/** True when err is the operating-market gate's refusal. */
export function isMarketRefusal(err: unknown): boolean {
  return err instanceof ApiError && err.code === 'MARKET_NOT_AVAILABLE'
}

/**
 * Returns the generic refusal sentence for a market refusal, otherwise `fallback(err)`. Every
 * registration / deposit / launch error mapper calls this first so the refusal reads the same on all
 * three screens.
 */
export function marketRefusalOr(err: unknown, fallback: (err: unknown) => string): string {
  return isMarketRefusal(err) ? MARKET_NOT_AVAILABLE_MESSAGE : fallback(err)
}
