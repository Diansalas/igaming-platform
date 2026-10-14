// The ONLY module in this app allowed to call fetch()/know about HTTP or
// JSON wire shapes for the Platform API. Every feature reaches the API
// exclusively through the typed functions in this directory, never
// directly.
import { clearSession, getAccessToken, getRefreshToken, setSession } from '../auth/tokenStore'
import { requestStepUp, stepUpNotEffectiveError } from '../auth/stepUpCoordinator'
import { ApiError, type ApiErrorBody } from './types'

// Relative by default so the Vite dev-server proxy (vite.config.ts)
// forwards to the real API with no CORS involved; a production build can
// override this at build time if the app is not served same-origin with
// the API.
const API_BASE = import.meta.env.VITE_API_BASE_URL ?? ''

async function toApiError(res: Response): Promise<ApiError> {
  let body: Partial<ApiErrorBody> = {}
  try {
    body = (await res.json()) as ApiErrorBody
  } catch {
    // Non-JSON error body (e.g. a proxy/gateway error) - fall through to
    // the generic mapping below.
  }
  const code = (body.code as ApiErrorBody['code']) ?? 'internal_error'
  const message = body.message ?? res.statusText ?? 'Request failed'
  const checkCodes = Array.isArray(body.check_codes) ? body.check_codes.filter((c): c is string => typeof c === 'string') : []
  const retryAfterRaw = Number(res.headers.get('Retry-After'))
  const retryAfter = Number.isFinite(retryAfterRaw) && retryAfterRaw > 0 ? Math.floor(retryAfterRaw) : undefined
  return new ApiError(code as ApiError['code'], message, res.status, body.request_id, checkCodes, retryAfter, stepUpMaxAge(body, res))
}

// `max_age` of a step_up_required refusal: body first, then the RFC 9470 WWW-Authenticate parameter. Used only to
// explain the prompt, never to compute freshness (the server clock decides).
function stepUpMaxAge(body: Partial<ApiErrorBody> & { max_age?: unknown }, res: Response): number | undefined {
  if (body.code !== 'step_up_required') return undefined
  const fromHeader = /max_age="?(\d+)"?/.exec(res.headers.get('WWW-Authenticate') ?? '')?.[1]
  const n = typeof body.max_age === 'number' ? body.max_age : Number(fromHeader)
  return Number.isInteger(n) && n > 0 && n <= 86400 ? n : undefined
}

// A one-shot stream cannot be sent twice; every request body in this app is a string.
function isResendable(body: BodyInit | null | undefined): boolean {
  return !(typeof ReadableStream !== 'undefined' && body instanceof ReadableStream)
}

// Refresh calls are made with a bare fetch, never apiFetch - apiFetch's
// own 401 handling calls THIS function, so routing it back through
// apiFetch would recurse.
export async function rawRefresh(refreshToken: string): Promise<{ access_token: string; refresh_token: string }> {
  const res = await fetch(`${API_BASE}/v1/auth/refresh`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ refresh_token: refreshToken }),
  })
  if (!res.ok) throw await toApiError(res)
  return res.json()
}

// Multiple requests failing with 401 at once (e.g. several widgets on a
// detail page) must trigger exactly ONE refresh call, not one per
// request - every concurrent caller awaits the same in-flight promise.
let inFlightRefresh: Promise<void> | null = null

// Exported so AuthProvider's bootstrap-on-load restore goes through the
// same single-flight dedupe as apiFetch's own 401 handling - calling
// rawRefresh directly there let React 18 StrictMode's deliberate
// double-effect-invocation (or any other concurrent caller) present the
// same refresh token to the server twice, which the backend correctly
// treats as reuse of an already-rotated token and revokes the whole
// session chain (internal/auth.RotateSession) - a real bug this
// re-export fixes, not merely a style preference.
export function refreshSessionOnce(): Promise<void> {
  if (!inFlightRefresh) {
    inFlightRefresh = (async () => {
      const refreshToken = getRefreshToken()
      if (!refreshToken) throw new ApiError('unauthorized', 'no active session', 401)
      const pair = await rawRefresh(refreshToken)
      setSession(pair.access_token, pair.refresh_token)
    })().finally(() => {
      inFlightRefresh = null
    })
  }
  return inFlightRefresh
}

/** Resolves once any in-flight token refresh has settled (success or failure). */
export function whenRefreshSettled(): Promise<void> {
  return inFlightRefresh ? inFlightRefresh.then(() => undefined, () => undefined) : Promise.resolve()
}

export interface ApiFetchOptions extends RequestInit {
  /** Internal: set to false on the retried attempt to prevent a refresh loop. */
  _isRetry?: boolean
  /**
   * Unauthenticated, secret-bearing call (staff login / MFA / enrolment,
   * ADR 0119): no bearer token is attached, a 401 is a business answer
   * (wrong code) and never triggers refresh or clears the session, and the
   * request/response is never cached.
   */
  anonymous?: boolean
  /** Internal: set on the single re-send after a step-up so a second `step_up_required` never prompts again. */
  _stepUpRetried?: boolean
  /** The step-up call itself: a refusal there must never start another step-up. */
  noStepUp?: boolean
}

/**
 * apiFetch performs one authenticated request against the Platform API.
 * On a 401, it attempts exactly one silent refresh (per the Stage 5
 * directive) and retries the original request once; if the refresh also
 * fails, it clears the session (which the auth route guard reacts to by
 * redirecting to /login) and rethrows.
 */
export async function apiFetch<T>(path: string, options: ApiFetchOptions = {}): Promise<T> {
  const { _isRetry, _stepUpRetried, noStepUp, anonymous, headers, ...rest } = options
  const token = anonymous ? null : getAccessToken()
  const mergedHeaders = new Headers(headers)
  if (!mergedHeaders.has('Content-Type') && rest.body) {
    mergedHeaders.set('Content-Type', 'application/json')
  }
  if (token) mergedHeaders.set('Authorization', `Bearer ${token}`)

  let res: Response
  try {
    res = await fetch(`${API_BASE}${path}`, { ...rest, ...(anonymous ? { cache: 'no-store' as const } : {}), headers: mergedHeaders })
  } catch {
    throw new ApiError('network_error', 'Could not reach the server. Check your connection and try again.', 0)
  }

  if (res.status === 401 && !anonymous) {
    // Two 401 answers are final business answers, not "access token expired": a wrong / replayed step-up code
    // (`invalid_mfa_code`: must not refresh, must not end the session) and a dead session (`session_revoked`:
    // refreshing a revoked chain is pointless; sign out). Branch on the code only.
    const peek = await toApiError(res.clone())
    if (peek.code === 'invalid_mfa_code') throw peek
    if (peek.code === 'session_revoked') {
      clearSession()
      throw peek
    }
  }

  if (res.status === 401 && !_isRetry && !anonymous) {
    try {
      await refreshSessionOnce()
    } catch {
      clearSession()
      throw await toApiError(res)
    }
    // Never replay a TOTP code automatically: the operator enters a fresh one.
    if (noStepUp) throw await toApiError(res)
    return apiFetch<T>(path, { ...options, _isRetry: true })
  }

  if (!res.ok) {
    const err = await toApiError(res)
    if (res.status === 403 && err.code === 'step_up_required' && !anonymous && !noStepUp) {
      // ADR 0119 S4b contract section 5: the refusal happened BEFORE any write, so one re-send is safe. The
      // original options object is reused, hence the same frozen body and the same Idempotency-Key header.
      // Exactly one re-send per original request; a second refusal fails visibly without prompting.
      if (_stepUpRetried || !isResendable(rest.body)) throw stepUpNotEffectiveError(err.requestId)
      await requestStepUp(token, err.stepUpMaxAge)
      if (rest.signal?.aborted) throw new DOMException('The operation was aborted.', 'AbortError')
      return apiFetch<T>(path, { ...options, _stepUpRetried: true })
    }
    if (res.status === 401 && !anonymous) clearSession()
    throw err
  }

  if (res.status === 204) return undefined as T
  const text = await res.text()
  return (text ? JSON.parse(text) : undefined) as T
}

/** Builds a query string from a plain object, skipping undefined/empty values. */
export function buildQuery(params: object): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params as Record<string, unknown>)) {
    if (value === undefined || value === null || value === '') continue
    search.set(key, String(value))
  }
  const qs = search.toString()
  return qs ? `?${qs}` : ''
}
