import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { server } from '../test/mswServer'
import { makeTestJwt } from '../test/jwt'
import { ENROL_CHALLENGE_TOKEN, MFA_CHALLENGE_TOKEN, RECOVERY_CODES, VALID_RECOVERY_CODE, VALID_TOTP, tokenPair } from '../test/handlers'
import { renderWithProviders } from '../test/renderWithProviders'
import { LoginPage } from './LoginPage'
import { getAccessToken, getRefreshToken } from './tokenStore'

async function submitPassword(user: ReturnType<typeof userEvent.setup>, password = 'correct-password') {
  await user.type(screen.getByLabelText('Email'), 'admin@example.com')
  await user.type(screen.getByLabelText('Password'), password)
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
}

async function reachMfaStep(user: ReturnType<typeof userEvent.setup>) {
  await submitPassword(user)
  await screen.findByLabelText('Authentication code')
}

function failWith(path: string, code: string, status: number, headers?: Record<string, string>) {
  server.use(http.post(path, () => HttpResponse.json({ code, message: 'SERVER-TEXT-MUST-NOT-RENDER' }, { status, headers })))
}

afterEach(() => vi.useRealTimers())

describe('LoginPage - password step', () => {
  it('moves to the MFA step on a challenge and is NOT signed in', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    expect(getAccessToken()).toBeNull()
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument()
  })

  it('shows a fixed message for wrong credentials (never the server text)', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await submitPassword(user, 'wrong-password')
    expect(await screen.findByRole('alert')).toHaveTextContent('Email or password is incorrect.')
  })

  it.each([
    ['mfa_enrolment_required', 403, 'Your account has no authenticator yet. Ask your administrator for an enrolment link.'],
    ['mfa_locked', 403, 'Too many incorrect codes. Try again in 15 minutes, or ask your administrator to reset your authenticator.'],
    ['forbidden', 403, 'This account cannot log in. Contact your administrator.'],
    ['not_found', 404, 'Unknown operator.'],
    ['service_unavailable', 503, 'Sign-in is temporarily unavailable. Try again.'],
    ['rate_limited', 429, 'Too many attempts. Wait a minute.'],
    ['internal_error', 500, 'Sign-in failed. Please try again.'],
  ])('maps %s to a fixed accessible message', async (code, status, text) => {
    failWith('/v1/staff/auth/login', code, status)
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await submitPassword(user)
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(text)
    expect(alert).not.toHaveTextContent('SERVER-TEXT-MUST-NOT-RENDER')
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeEnabled()
  })

  it('shows the Retry-After wait when the server sends one', async () => {
    failWith('/v1/staff/auth/login', 'rate_limited', 429, { 'Retry-After': '42' })
    renderWithProviders(<LoginPage />, { route: '/login' })
    await submitPassword(userEvent.setup())
    expect(await screen.findByRole('alert')).toHaveTextContent('Wait 42 seconds')
  })

  it('reports a network failure without leaking detail', async () => {
    server.use(http.post('/v1/staff/auth/login', () => HttpResponse.error()))
    renderWithProviders(<LoginPage />, { route: '/login' })
    await submitPassword(userEvent.setup())
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not reach the server')
  })

  it('sends no bearer token and does not refresh or clear a session on a 401', async () => {
    let auth: string | null = 'unset'
    let refreshCalls = 0
    server.use(
      http.post('/v1/staff/auth/login', ({ request }) => {
        auth = request.headers.get('Authorization')
        return HttpResponse.json({ code: 'unauthorized', message: 'x' }, { status: 401 })
      }),
      http.post('/v1/auth/refresh', () => {
        refreshCalls++
        return HttpResponse.json({}, { status: 401 })
      }),
    )
    renderWithProviders(<LoginPage />, { route: '/login' })
    await submitPassword(userEvent.setup())
    await screen.findByRole('alert')
    expect(auth).toBeNull()
    expect(refreshCalls).toBe(0)
  })

  it('rejects an empty form locally without calling the API', async () => {
    let calls = 0
    server.use(http.post('/v1/staff/auth/login', () => (calls++, HttpResponse.json({}, { status: 500 }))))
    renderWithProviders(<LoginPage />, { route: '/login' })
    await userEvent.setup().click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter your email and password.')
    expect(calls).toBe(0)
  })
})

describe('LoginPage - MFA step', () => {
  it('signs in only after a valid TOTP and an aal2 token pair', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    await user.type(screen.getByLabelText('Authentication code'), '123 456')
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    await waitFor(() => expect(getAccessToken()).not.toBeNull())
    expect(getRefreshToken()).toBe('refresh-token-1')
  })

  it('supports keyboard-only submit with Enter and focuses the code field', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    expect(screen.getByLabelText('Authentication code')).toHaveFocus()
    expect(screen.getByLabelText('Authentication code')).toHaveAttribute('autocomplete', 'one-time-code')
    await user.keyboard(`${VALID_TOTP}{Enter}`)
    await waitFor(() => expect(getAccessToken()).not.toBeNull())
  })

  it('rejects a malformed code locally (no attempt spent)', async () => {
    let calls = 0
    server.use(http.post('/v1/staff/auth/mfa/verify', () => (calls++, HttpResponse.json({}, { status: 500 }))))
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    await user.type(screen.getByLabelText('Authentication code'), '12ab')
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter the 6-digit code')
    expect(calls).toBe(0)
  })

  it('stays on the step with a fixed message and clears the field on a wrong code', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    await user.type(screen.getByLabelText('Authentication code'), '000000')
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('That code is not valid.')
    expect(screen.getByLabelText('Authentication code')).toHaveValue('')
    expect(getAccessToken()).toBeNull()
  })

  it.each([
    ['invalid_challenge', 401, 'Your sign-in session expired. Please sign in again.'],
    ['mfa_locked', 403, 'Too many incorrect codes. Try again in 15 minutes'],
    ['forbidden', 403, 'This account cannot log in. Contact your administrator.'],
  ])('%s restarts at the password step with a fixed message', async (code, status, text) => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    failWith('/v1/staff/auth/mfa/verify', code, status)
    await user.type(screen.getByLabelText('Authentication code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(text)
    expect(screen.getByLabelText('Password')).toHaveValue('')
    expect(getAccessToken()).toBeNull()
  })

  it.each([
    ['service_unavailable', 503, 'Sign-in is temporarily unavailable. Try again.'],
    ['rate_limited', 429, 'Too many attempts. Wait a minute.'],
  ])('%s keeps the step so the user can retry', async (code, status, text) => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    failWith('/v1/staff/auth/mfa/verify', code, status)
    await user.type(screen.getByLabelText('Authentication code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(text)
    expect(screen.getByLabelText('Authentication code')).toBeInTheDocument()
  })

  it('503 is the same message for a right and a wrong code (no oracle)', async () => {
    failWith('/v1/staff/auth/mfa/verify', 'service_unavailable', 503)
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    await user.type(screen.getByLabelText('Authentication code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    const right = (await screen.findByRole('alert')).textContent
    await user.type(screen.getByLabelText('Authentication code'), '999999')
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    await waitFor(() => expect(screen.getByRole('alert').textContent).toBe(right))
  })

  it('fails closed when the server returns a token that is not aal2', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    server.use(
      http.post('/v1/staff/auth/mfa/verify', () =>
        HttpResponse.json({
          access_token: makeTestJwt({ aal: 1 }),
          refresh_token: 'r',
          token_type: 'Bearer',
          expires_in: 900,
        }),
      ),
    )
    await user.type(screen.getByLabelText('Authentication code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByLabelText('Password')).toBeInTheDocument()
    expect(getAccessToken()).toBeNull()
    expect(getRefreshToken()).toBeNull()
  })

  it('Cancel returns to the password step and drops the challenge', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.getByLabelText('Password')).toHaveValue('')
  })

  it('restarts with an expiry message when the challenge lifetime passes', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    await reachMfaStep(user)
    act(() => {
      vi.advanceTimersByTime(300_001)
    })
    expect(await screen.findByRole('alert')).toHaveTextContent('Your sign-in session expired.')
    expect(screen.getByLabelText('Password')).toBeInTheDocument()
  })

  it('ignores a double submit while a verification is in flight', async () => {
    let calls = 0
    let release: () => void = () => {}
    const gate = new Promise<void>((r) => (release = r))
    server.use(
      http.post('/v1/staff/auth/mfa/verify', async () => {
        calls++
        await gate
        return HttpResponse.json({ code: 'invalid_mfa_code', message: 'x' }, { status: 401 })
      }),
    )
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    await user.type(screen.getByLabelText('Authentication code'), VALID_TOTP)
    await user.keyboard('{Enter}{Enter}')
    release()
    await screen.findByRole('alert')
    expect(calls).toBe(1)
  })
})

describe('LoginPage - challenge lifetime (security F1)', () => {
  it('does not wipe the recovery codes when the original mfa challenge lifetime passes during enrolment', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    await reachMfaStep(user)
    await user.click(screen.getByRole('button', { name: 'Use a recovery code' }))
    await user.type(await screen.findByLabelText('Recovery code'), VALID_RECOVERY_CODE)
    await user.click(screen.getByRole('button', { name: 'Use recovery code' }))
    await user.click(await screen.findByRole('button', { name: 'Generate setup secret' }))
    await user.type(await screen.findByLabelText('6-digit code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    await screen.findByRole('heading', { name: 'Save your recovery codes' })
    act(() => {
      vi.advanceTimersByTime(300_000)
    })
    expect(screen.getByRole('heading', { name: 'Save your recovery codes' })).toBeInTheDocument()
    expect(screen.getByText(RECOVERY_CODES[0])).toBeInTheDocument()
  })

  it('switching between code and recovery steps does not restart the single deadline', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    await reachMfaStep(user)
    act(() => {
      vi.advanceTimersByTime(200_000)
    })
    await user.click(screen.getByRole('button', { name: 'Use a recovery code' }))
    act(() => {
      vi.advanceTimersByTime(101_000)
    })
    expect(await screen.findByRole('alert')).toHaveTextContent('Your sign-in session expired.')
  })

  it('drops a late verify response after Cancel', async () => {
    let release: () => void = () => {}
    const gate = new Promise<void>((r) => (release = r))
    server.use(
      http.post('/v1/staff/auth/mfa/verify', async () => {
        await gate
        return HttpResponse.json(tokenPair('late-refresh'))
      }),
    )
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await reachMfaStep(user)
    await user.type(screen.getByLabelText('Authentication code'), VALID_TOTP)
    await user.keyboard('{Enter}')
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    release()
    await new Promise((r) => setTimeout(r, 100))
    expect(getAccessToken()).toBeNull()
    expect(screen.getByLabelText('Password')).toBeInTheDocument()
  })
})

describe('LoginPage - recovery code', () => {
  async function toRecovery(user: ReturnType<typeof userEvent.setup>) {
    await reachMfaStep(user)
    await user.click(screen.getByRole('button', { name: 'Use a recovery code' }))
    await screen.findByLabelText('Recovery code')
  }

  it('a valid recovery code leads to enrolment, NOT a session', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await toRecovery(user)
    await user.type(screen.getByLabelText('Recovery code'), VALID_RECOVERY_CODE.toLowerCase())
    await user.click(screen.getByRole('button', { name: 'Use recovery code' }))
    expect(await screen.findByRole('heading', { name: 'Set up your authenticator' })).toBeInTheDocument()
    expect(getAccessToken()).toBeNull()
  })

  it('sends the enrol challenge (not the mfa one) to enrol/start', async () => {
    let started: string | null = null
    server.use(
      http.post('/v1/staff/auth/enrol/start', async ({ request }) => {
        started = ((await request.json()) as { challenge_token: string }).challenge_token
        return HttpResponse.json({ code: 'invalid_challenge', message: 'x' }, { status: 401 })
      }),
    )
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await toRecovery(user)
    await user.type(screen.getByLabelText('Recovery code'), VALID_RECOVERY_CODE)
    await user.click(screen.getByRole('button', { name: 'Use recovery code' }))
    await user.click(await screen.findByRole('button', { name: 'Generate setup secret' }))
    await screen.findByLabelText('Password')
    expect(started).toBe(ENROL_CHALLENGE_TOKEN)
    expect(started).not.toBe(MFA_CHALLENGE_TOKEN)
  })

  it('rejects a malformed recovery code locally', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await toRecovery(user)
    await user.type(screen.getByLabelText('Recovery code'), 'short')
    await user.click(screen.getByRole('button', { name: 'Use recovery code' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('XXXXX-XXXXX')
  })

  it('a wrong recovery code shows the fixed invalid-code message', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await toRecovery(user)
    await user.type(screen.getByLabelText('Recovery code'), 'ZZZZZ-ZZZZZ')
    await user.click(screen.getByRole('button', { name: 'Use recovery code' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('That code is not valid.')
  })

  it('can go back to the authenticator code step', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()
    await toRecovery(user)
    await user.click(screen.getByRole('button', { name: 'Back to authenticator code' }))
    expect(screen.getByLabelText('Authentication code')).toBeInTheDocument()
  })
})
