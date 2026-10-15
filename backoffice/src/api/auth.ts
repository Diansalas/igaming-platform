import { apiFetch, whenRefreshSettled } from './client'

export interface TokenPairResponse {
  access_token: string
  refresh_token: string
  token_type: string
  expires_in: number
}

export interface StaffLoginParams {
  /** Omit entirely for a platform_admin login - every other role must supply it. */
  tenantSlug?: string
  email: string
  password: string
}

/**
 * Single-use staff auth challenge (ADR 0119 s6). SECRET: keep in memory
 * only, never log, never persist. `mfa` -> verify/recover; `enrol` -> enrol/start.
 */
export interface StaffAuthChallenge {
  type: 'mfa' | 'enrol'
  token: string
  expires_in: number
  methods: Array<'totp' | 'recovery_code'>
}

export interface EnrolStartResponse {
  factor_id: string
  otpauth_uri: string
  secret_base32: string
  expires_in: number
}

export interface EnrolConfirmResponse {
  recovery_codes: string[]
  token_pair: TokenPairResponse
}

export interface StaffMfaStatus {
  enrolled: boolean
  factor_type: 'totp' | null
  activated_at: string | null
  recovery_codes_remaining: number
  hard_locked: boolean
  locked_until: string | null
}

// All staff-auth calls are `anonymous`: no bearer, no refresh-on-401, no cache.
function postAnonymous<T>(path: string, body: unknown): Promise<T> {
  return apiFetch<T>(path, { method: 'POST', body: JSON.stringify(body), anonymous: true })
}

function credentialBody(p: { tenantSlug?: string; email: string; password: string }): Record<string, string> {
  const body: Record<string, string> = { email: p.email, password: p.password }
  if (p.tenantSlug) body.tenant_slug = p.tenantSlug
  return body
}

/** Step 1: password. Never returns a token pair (S3 breaking change) - only an `mfa` challenge. */
export async function staffLogin(params: StaffLoginParams): Promise<StaffAuthChallenge> {
  const res = await postAnonymous<{ challenge: StaffAuthChallenge }>('/v1/staff/auth/login', credentialBody(params))
  return res.challenge
}

export function staffMfaVerify(challengeToken: string, code: string): Promise<TokenPairResponse> {
  return postAnonymous('/v1/staff/auth/mfa/verify', { challenge_token: challengeToken, code })
}

/** Lost device: a recovery code yields an `enrol` challenge, never a session. */
export async function staffMfaRecover(challengeToken: string, recoveryCode: string): Promise<StaffAuthChallenge> {
  const res = await postAnonymous<{ challenge: StaffAuthChallenge }>('/v1/staff/auth/mfa/recover', {
    challenge_token: challengeToken,
    recovery_code: recoveryCode,
  })
  return res.challenge
}

export async function staffEnrolRedeem(params: {
  token: string
  tenantSlug?: string
  email: string
  password: string
}): Promise<StaffAuthChallenge> {
  const res = await postAnonymous<{ challenge: StaffAuthChallenge }>('/v1/staff/auth/enrol/redeem', {
    token: params.token,
    ...credentialBody(params),
  })
  return res.challenge
}

/**
 * POST /v1/staff/invitations/accept (anonymous, token-authenticated, x-sensitive body: `token` and `password` are secrets
 * and are never logged or kept). The tenant and identity come from the token row. Success yields an `enrol` challenge:
 * continue with staffEnrolStart / staffEnrolConfirm. Every token failure is one `401 invalid_enrolment_token`.
 */
export async function acceptStaffInvitation(token: string, password: string): Promise<StaffAuthChallenge> {
  const res = await postAnonymous<{ challenge: StaffAuthChallenge }>('/v1/staff/invitations/accept', { token, password })
  return res.challenge
}

export function staffEnrolStart(challengeToken: string): Promise<EnrolStartResponse> {
  return postAnonymous('/v1/staff/auth/enrol/start', { challenge_token: challengeToken })
}

export function staffEnrolConfirm(challengeToken: string, factorId: string, code: string): Promise<EnrolConfirmResponse> {
  return postAnonymous('/v1/staff/auth/enrol/confirm', {
    challenge_token: challengeToken,
    factor_id: factorId,
    code,
  })
}

/** GET /v1/staff/me/mfa (bearer). Not a secret, but never cached. */
export function getMyMfaStatus(): Promise<StaffMfaStatus> {
  return apiFetch<StaffMfaStatus>('/v1/staff/me/mfa', { cache: 'no-store' })
}

export interface StepUpResponse {
  access_token: string
  token_type: string
  expires_in: number
  mfa_at: string
  step_up_expires_at: { financial: string; critical: string }
}

/**
 * POST /v1/staff/auth/step-up (bearer): a fresh TOTP code for a new access token. `noStepUp` so a refusal here
 * can never recurse into the step-up flow. SECRET in the body: callers must not keep the code.
 */
export async function staffStepUp(code: string): Promise<StepUpResponse> {
  // A racing refresh would supersede the token this call is sent with ("not the session's newest"): let it settle first.
  await whenRefreshSettled()
  return apiFetch<StepUpResponse>('/v1/staff/auth/step-up', {
    method: 'POST',
    body: JSON.stringify({ code }),
    cache: 'no-store',
    noStepUp: true,
  })
}

export interface RecoveryCodesResponse {
  recovery_codes: string[]
}

/** POST /v1/staff/mfa/recovery-codes/regenerate (bearer, step-up critical, no body). SECRET response: never cached. */
export function regenerateRecoveryCodes(signal?: AbortSignal): Promise<RecoveryCodesResponse> {
  return apiFetch<RecoveryCodesResponse>('/v1/staff/mfa/recovery-codes/regenerate', { method: 'POST', cache: 'no-store', signal })
}

/**
 * Revokes the session server-side (internal/auth.RevokeSessionByToken).
 * Uses `fetch` directly, not `apiFetch` - logout has no access token to
 * attach by the time it matters (the caller clears local state right
 * after), and it must never trigger apiFetch's own 401-refresh-retry
 * logic. Best-effort: the caller clears local session state regardless
 * of whether this call succeeds, so a network failure never traps the
 * user in a "signed in" UI they can't get out of.
 */
export function staffLogout(refreshToken: string): Promise<void> {
  return fetch(`${import.meta.env.VITE_API_BASE_URL ?? ''}/v1/auth/logout`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ refresh_token: refreshToken }),
  }).then(() => undefined)
}
