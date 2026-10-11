import { ApiError } from '../api/types'

/** The one neutral sentence shown when the brand cannot be used (ADR 0112 section 7.3). */
export const SITE_UNAVAILABLE_MESSAGE = 'This site is not available at the moment'

/**
 * True when a login/registration error means "this brand is not open": the 404 that an unknown or
 * not-yet-launched brand answers identically (so brand existence is never disclosed), or the 409
 * BRAND_NOT_ACCEPTING_REGISTRATIONS of a suspended/closed brand. A 409 `conflict` is a duplicate
 * email and is NOT this case.
 */
export function isSiteUnavailableError(err: unknown): boolean {
  if (!(err instanceof ApiError)) return false
  return err.status === 404 || err.code === 'not_found' || err.code === 'BRAND_NOT_ACCEPTING_REGISTRATIONS'
}

/**
 * Neutral state: deliberately gives no reason, no legal wording and no hint whether the brand
 * exists, is awaiting launch, is suspended or closed.
 */
export function SiteUnavailable() {
  return (
    <div className="flex min-h-screen items-center justify-center bg-surface-alt px-4">
      <div role="status" className="w-full max-w-sm rounded-lg border border-border bg-surface p-6 text-center shadow-sm">
        <h1 className="text-lg font-semibold text-slate-900">{SITE_UNAVAILABLE_MESSAGE}</h1>
        <p className="mt-2 text-sm text-slate-500">Please try again later.</p>
      </div>
    </div>
  )
}
