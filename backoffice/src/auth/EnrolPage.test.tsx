import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { server } from '../test/mswServer'
import {
  ENROL_LINK_TOKEN,
  RECOVERY_CODES,
  TOTP_SECRET,
  VALID_TOTP,
  ENROL_CHALLENGE_TOKEN,
} from '../test/handlers'
import { makeTestJwt } from '../test/jwt'
import { renderWithProviders } from '../test/renderWithProviders'
import { clearEnrolLinkToken } from './enrolLink'
import { captureEnrolLinkToken } from './enrolLink'
import { EnrolPage } from './EnrolPage'
import { formatRecoveryCodesFile, isSafeOtpauthUri } from './EnrolWizard'
import { getAccessToken } from './tokenStore'

function seedLinkToken(token = ENROL_LINK_TOKEN) {
  captureEnrolLinkToken({ pathname: '/', search: '', hash: `#/enrol?token=${token}` }, { replaceState: () => {} })
}

async function redeem(user: ReturnType<typeof userEvent.setup>, password = 'correct-password') {
  await user.type(screen.getByLabelText('Email'), 'admin@example.com')
  await user.type(screen.getByLabelText('Password'), password)
  await user.click(screen.getByRole('button', { name: 'Continue' }))
}

async function toScanStep(user: ReturnType<typeof userEvent.setup>) {
  await redeem(user)
  await user.click(await screen.findByRole('button', { name: 'Generate setup secret' }))
  await screen.findByLabelText('6-digit code')
}

async function toCodesStep(user: ReturnType<typeof userEvent.setup>) {
  await toScanStep(user)
  await user.type(screen.getByLabelText('6-digit code'), VALID_TOTP)
  await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
  await screen.findByRole('heading', { name: 'Save your recovery codes' })
}

beforeEach(() => seedLinkToken())
afterEach(() => {
  clearEnrolLinkToken()
  vi.restoreAllMocks()
})

describe('EnrolPage - redeem', () => {
  it('without a link token shows the uniform invalid-link message', () => {
    clearEnrolLinkToken()
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    expect(screen.getByRole('alert')).toHaveTextContent('This enrolment link is invalid or expired')
    expect(screen.getByRole('link', { name: 'Go to sign in' })).toBeInTheDocument()
  })

  it('invalid token / mismatched details give one uniform message and clear the password', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await redeem(user, 'wrong-password')
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'This enrolment link is invalid or expired, or the details do not match.',
    )
    expect(screen.getByLabelText('Password')).toHaveValue('')
  })

  it.each([
    ['rate_limited', 429, 'Too many attempts. Wait a minute.'],
    ['service_unavailable', 503, 'Sign-in is temporarily unavailable. Try again.'],
    ['not_found', 404, 'Unknown operator.'],
    ['forbidden', 403, 'This account cannot log in. Contact your administrator.'],
  ])('maps %s', async (code, status, text) => {
    server.use(http.post('/v1/staff/auth/enrol/redeem', () => HttpResponse.json({ code, message: 'RAW' }, { status })))
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    await redeem(userEvent.setup())
    const a = await screen.findByRole('alert')
    expect(a).toHaveTextContent(text)
    expect(a).not.toHaveTextContent('RAW')
  })

  it('sends the one-time token to redeem and then forgets it', async () => {
    let body: Record<string, string> = {}
    server.use(
      http.post('/v1/staff/auth/enrol/redeem', async ({ request }) => {
        body = (await request.json()) as Record<string, string>
        return HttpResponse.json({
          challenge: { type: 'enrol', token: ENROL_CHALLENGE_TOKEN, expires_in: 300, methods: ['totp'] },
        })
      }),
    )
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    await redeem(userEvent.setup())
    await screen.findByRole('heading', { name: 'Set up your authenticator' })
    expect(body.token).toBe(ENROL_LINK_TOKEN)
    expect(body).not.toHaveProperty('tenant_slug')
  })
})

describe('EnrolWizard - enrolment', () => {
  it('start shows the secret only behind a reveal toggle and never renders a QR service', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toScanStep(user)
    expect(screen.queryByText(TOTP_SECRET)).not.toBeInTheDocument()
    const toggle = screen.getByRole('button', { name: 'Show setup key' })
    expect(toggle).toHaveAttribute('aria-pressed', 'false')
    await user.click(toggle)
    expect(screen.getByText(TOTP_SECRET)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Hide setup key' }))
    expect(screen.queryByText(TOTP_SECRET)).not.toBeInTheDocument()
    expect(document.querySelector('img, canvas')).toBeNull()
  })

  it('copies the setup URI on request', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined)
    await toScanStep(user)
    await user.click(screen.getByRole('button', { name: 'Copy setup URI' }))
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(expect.stringMatching(/^otpauth:\/\/totp\//)))
  })

  it('only links a well-formed otpauth URI', () => {
    expect(isSafeOtpauthUri('otpauth://totp/A:b?secret=X')).toBe(true)
    expect(isSafeOtpauthUri('javascript:alert(1)')).toBe(false)
    expect(isSafeOtpauthUri('https://evil.example/otpauth://totp/x')).toBe(false)
    expect(isSafeOtpauthUri('otpauth://hotp/x')).toBe(false)
  })

  it('does not render a link for a hostile URI from the server', async () => {
    server.use(
      http.post('/v1/staff/auth/enrol/start', () =>
        HttpResponse.json({ factor_id: 'f', otpauth_uri: 'javascript:alert(1)', secret_base32: 'ABC', expires_in: 600 }),
      ),
    )
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    await toScanStep(userEvent.setup())
    expect(screen.queryByRole('link', { name: 'Open in authenticator app' })).not.toBeInTheDocument()
  })

  it('wrong confirm code keeps the step, clears the field, and is a fixed message', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toScanStep(user)
    await user.type(screen.getByLabelText('6-digit code'), '000000')
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('That code is not valid.')
    expect(screen.getByLabelText('6-digit code')).toHaveValue('')
  })

  it('mfa_enrolment_expired goes back to the start step with the same challenge', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toScanStep(user)
    server.use(
      http.post('/v1/staff/auth/enrol/confirm', () =>
        HttpResponse.json({ code: 'mfa_enrolment_expired', message: 'x' }, { status: 409 }),
      ),
    )
    await user.type(screen.getByLabelText('6-digit code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Setup took too long. Start again.')
    expect(screen.getByRole('button', { name: 'Generate setup secret' })).toBeInTheDocument()
    expect(screen.queryByText(TOTP_SECRET)).not.toBeInTheDocument()
  })

  it('invalid_challenge on confirm drops the secret and ends the flow', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toScanStep(user)
    server.use(
      http.post('/v1/staff/auth/enrol/confirm', () =>
        HttpResponse.json({ code: 'invalid_challenge', message: 'x' }, { status: 401 }),
      ),
    )
    await user.type(screen.getByLabelText('6-digit code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Your sign-in session expired. Please sign in again.')
    expect(screen.queryByLabelText('6-digit code')).not.toBeInTheDocument()
  })

  it.each([
    ['mfa_locked', 403, 'Too many incorrect codes.'],
    ['service_unavailable', 503, 'Sign-in is temporarily unavailable. Try again.'],
    ['rate_limited', 429, 'Too many attempts.'],
  ])('confirm error %s is mapped', async (code, status, text) => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toScanStep(user)
    server.use(http.post('/v1/staff/auth/enrol/confirm', () => HttpResponse.json({ code, message: 'RAW' }, { status })))
    await user.type(screen.getByLabelText('6-digit code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(text)
  })

  it('malformed confirm code is rejected locally', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toScanStep(user)
    await user.type(screen.getByLabelText('6-digit code'), '12')
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter the 6-digit code')
  })
})

describe('EnrolWizard - recovery codes', () => {
  it('shows the ten codes once, is NOT signed in yet, and gates Continue on the acknowledgement', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toCodesStep(user)
    for (const c of RECOVERY_CODES) expect(screen.getByText(c)).toBeInTheDocument()
    expect(getAccessToken()).toBeNull()
    const cont = screen.getByRole('button', { name: 'Continue' })
    expect(cont).toBeDisabled()
    await user.click(screen.getByRole('checkbox', { name: 'I have stored these codes' }))
    expect(cont).toBeEnabled()
    await user.click(cont)
    await waitFor(() => expect(getAccessToken()).not.toBeNull())
  })

  it('keyboard: tab order reaches copy, download, the checkbox, then Continue', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toCodesStep(user)
    await user.click(screen.getByRole('heading', { name: 'Save your recovery codes' }))
    await user.tab()
    expect(screen.getByRole('button', { name: 'Copy codes' })).toHaveFocus()
    await user.tab()
    expect(screen.getByRole('button', { name: 'Download .txt' })).toHaveFocus()
    await user.tab()
    await user.keyboard(' ')
    expect(screen.getByRole('checkbox', { name: 'I have stored these codes' })).toBeChecked()
    await user.tab()
    expect(screen.getByRole('button', { name: 'Continue' })).toHaveFocus()
  })

  it('copy writes the codes to the clipboard; a clipboard failure is reported', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined)
    await toCodesStep(user)
    await user.click(screen.getByRole('button', { name: 'Copy codes' }))
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(RECOVERY_CODES.join('\n')))
    writeText.mockRejectedValueOnce(new Error('denied'))
    await user.click(screen.getByRole('button', { name: 'Copy codes' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Copy failed')
  })

  it('download builds a text file and revokes the object URL', async () => {
    const create = vi.fn().mockReturnValue('blob:x')
    const revoke = vi.fn()
    Object.assign(URL, { createObjectURL: create, revokeObjectURL: revoke })
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toCodesStep(user)
    await user.click(screen.getByRole('button', { name: 'Download .txt' }))
    expect(create).toHaveBeenCalledTimes(1)
    expect(create.mock.calls[0][0]).toBeInstanceOf(Blob)
    await waitFor(() => expect(revoke).toHaveBeenCalledWith('blob:x'), { timeout: 3000 })
    expect(formatRecoveryCodesFile(RECOVERY_CODES)).toContain(RECOVERY_CODES[0])
  })

  it('fails closed if the token pair from enrol/confirm is not aal2', async () => {
    server.use(
      http.post('/v1/staff/auth/enrol/confirm', () =>
        HttpResponse.json({
          recovery_codes: RECOVERY_CODES,
          token_pair: { access_token: makeTestJwt({ aal: 1 }), refresh_token: 'r', token_type: 'Bearer', expires_in: 900 },
        }),
      ),
    )
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toCodesStep(user)
    await user.click(screen.getByRole('checkbox', { name: 'I have stored these codes' }))
    await user.click(screen.getByRole('button', { name: 'Continue' }))
    expect(getAccessToken()).toBeNull()
    expect(await screen.findByRole('alert')).toBeInTheDocument()
    for (const c of RECOVERY_CODES) expect(screen.queryByText(c)).not.toBeInTheDocument()
  })

  it('discards secrets from the DOM when unmounted', async () => {
    const { unmount, container } = renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toCodesStep(user)
    unmount()
    expect(container.textContent ?? '').not.toContain(RECOVERY_CODES[0])
    expect(document.body.textContent ?? '').not.toContain(RECOVERY_CODES[0])
  })
})

describe('EnrolWizard - leaving the page', () => {
  it('prompts on beforeunload while codes are unacknowledged, not after', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    const fire = () => {
      const e = new Event('beforeunload', { cancelable: true })
      window.dispatchEvent(e)
      return e.defaultPrevented
    }
    await toScanStep(user)
    expect(fire()).toBe(false)
    await user.type(screen.getByLabelText('6-digit code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    await screen.findByRole('heading', { name: 'Save your recovery codes' })
    expect(fire()).toBe(true)
    await user.click(screen.getByRole('checkbox', { name: 'I have stored these codes' }))
    expect(fire()).toBe(false)
  })

  it('links the error text to the code input', async () => {
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toScanStep(user)
    await user.type(screen.getByLabelText('6-digit code'), '000000')
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    await screen.findByRole('alert')
    expect(screen.getByLabelText('6-digit code').getAttribute('aria-describedby')).toContain('enrol-error')
  })
})

describe('no secret persistence or logging', () => {
  it('full enrolment writes none of the secrets to Web Storage and logs none to the console', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    const logs = (['log', 'info', 'warn', 'error', 'debug'] as const).map((m) => vi.spyOn(console, m).mockImplementation(() => {}))
    renderWithProviders(<EnrolPage />, { route: '/enrol' })
    const user = userEvent.setup()
    await toScanStep(user)
    await user.click(screen.getByRole('button', { name: 'Show setup key' }))
    await user.type(screen.getByLabelText('6-digit code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    await screen.findByRole('heading', { name: 'Save your recovery codes' })
    await user.click(screen.getByRole('checkbox', { name: 'I have stored these codes' }))
    await user.click(screen.getByRole('button', { name: 'Continue' }))
    await waitFor(() => expect(getAccessToken()).not.toBeNull())

    const secrets = [TOTP_SECRET, ENROL_CHALLENGE_TOKEN, ENROL_LINK_TOKEN, ...RECOVERY_CODES, getAccessToken()!, 'correct-password']
    const stored = JSON.stringify([...Object.entries(localStorage), ...Object.entries(sessionStorage), ...setItem.mock.calls])
    for (const s of secrets) expect(stored).not.toContain(s)
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
    const logged = JSON.stringify(logs.flatMap((l) => l.mock.calls))
    for (const s of secrets) expect(logged).not.toContain(s)
  })
})
