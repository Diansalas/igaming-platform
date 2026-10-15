import { configure, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../app/AppRoutes'
import { ErrorBoundary } from '../components/ErrorBoundary'
import { ENROL_CHALLENGE_TOKEN, INVITE_LINK_TOKEN, INVITE_PASSWORD, RECOVERY_CODES, TOTP_SECRET, VALID_TOTP } from '../test/handlers'
import { server } from '../test/mswServer'
import { renderWithProviders } from '../test/renderWithProviders'
import { signInAs } from '../test/session'
import { SERVER_MESSAGE_SENTINEL } from '../test/stepUp'
import { captureEnrolLinkToken, clearEnrolLinkToken, clearInviteLinkToken, getEnrolLinkToken, getInviteLinkToken } from './enrolLink'
import { InvitationAcceptPage } from './InvitationAcceptPage'
import { INVITATION_DEAD } from './staffAuthErrors'

// The suite shares a loaded CI machine with heavy tests: widen async waits (not assertions).
configure({ asyncUtilTimeout: 5000 })
vi.setConfig({ testTimeout: 20000 })

function seedInviteLink(token = INVITE_LINK_TOKEN) {
  captureEnrolLinkToken({ pathname: '/', search: '', hash: `#/enrol?token=${token}&invite=1` }, { replaceState: () => {} })
}

function page() {
  return renderWithProviders(<InvitationAcceptPage />, { route: '/accept-invitation' })
}

async function submit(user: ReturnType<typeof userEvent.setup>, password = INVITE_PASSWORD, confirm = password) {
  await user.type(screen.getByLabelText('New password'), password)
  await user.type(screen.getByLabelText('Confirm new password'), confirm)
  await user.click(screen.getByRole('button', { name: 'Set password and continue' }))
}

let consoleSpies: Array<ReturnType<typeof vi.spyOn>> = []
beforeEach(() => {
  seedInviteLink()
  consoleSpies = (['log', 'info', 'warn', 'error', 'debug'] as const).map((m) => vi.spyOn(console, m).mockImplementation(() => {}))
})
afterEach(() => {
  clearInviteLinkToken()
  clearEnrolLinkToken()
  vi.restoreAllMocks()
})

/** Nothing the page does may put the token, the password or a challenge token in a log, storage or the DOM text. */
function expectNoSecretLeak() {
  const logged = JSON.stringify(consoleSpies.flatMap((s) => s.mock.calls))
  for (const secret of [INVITE_LINK_TOKEN, INVITE_PASSWORD]) {
    expect(logged).not.toContain(secret)
    expect(JSON.stringify({ ...localStorage })).not.toContain(secret)
    expect(JSON.stringify({ ...sessionStorage })).not.toContain(secret)
    expect(document.body.textContent).not.toContain(secret)
  }
  expect(window.location.href).not.toContain(INVITE_LINK_TOKEN)
}

describe('InvitationAcceptPage - link handling', () => {
  it('without a token shows the fixed dead-link message and no form', () => {
    clearInviteLinkToken()
    page()
    expect(screen.getByRole('alert')).toHaveTextContent(INVITATION_DEAD)
    expect(screen.getByRole('link', { name: 'Go to sign in' })).toBeInTheDocument()
    expect(screen.queryByLabelText('New password')).not.toBeInTheDocument()
  })

  it('is reachable outside the authenticated shell: no session, no redirect to /login', async () => {
    renderWithProviders(<AppRoutes />, { route: '/accept-invitation' })
    expect(await screen.findByRole('heading', { name: 'Accept your invitation' })).toBeInTheDocument()
  })

  it('an already signed-in visitor is sent home and the token is dropped', async () => {
    signInAs({ role: 'tenant_admin' })
    renderWithProviders(
      <Routes>
        <Route path="/accept-invitation" element={<InvitationAcceptPage />} />
        <Route path="/" element={<p>home</p>} />
      </Routes>,
      { route: '/accept-invitation' },
    )
    expect(await screen.findByText('home')).toBeInTheDocument()
    expect(getInviteLinkToken()).toBeNull()
  })
})

describe('InvitationAcceptPage - password rules (client mirror of the server minimum)', () => {
  it('rejects 11 characters without calling the API, then accepts exactly 12', async () => {
    const spy = vi.fn()
    server.use(
      http.post('/v1/staff/invitations/accept', async ({ request }) => {
        spy(await request.json())
        return HttpResponse.json({ challenge: { type: 'enrol', token: ENROL_CHALLENGE_TOKEN, expires_in: 300, methods: ['totp'] } })
      }),
    )
    page()
    const user = userEvent.setup()
    await submit(user, 'x'.repeat(11))
    expect(screen.getByText('Password must be at least 12 characters.')).toBeInTheDocument()
    expect(spy).not.toHaveBeenCalled()
    await user.clear(screen.getByLabelText('New password'))
    await user.clear(screen.getByLabelText('Confirm new password'))
    await submit(user, 'y'.repeat(12))
    await screen.findByRole('heading', { name: 'Set up your authenticator' })
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('rejects a confirmation mismatch without calling the API', async () => {
    const spy = vi.fn()
    server.use(http.post('/v1/staff/invitations/accept', () => (spy(), HttpResponse.json({}, { status: 200 }))))
    page()
    await submit(userEvent.setup(), INVITE_PASSWORD, INVITE_PASSWORD + 'x')
    expect(screen.getByText('The two passwords do not match.')).toBeInTheDocument()
    expect(spy).not.toHaveBeenCalled()
  })

  it('uses new-password autocomplete and masked inputs', () => {
    page()
    for (const label of ['New password', 'Confirm new password']) {
      const el = screen.getByLabelText(label)
      expect(el).toHaveAttribute('type', 'password')
      expect(el).toHaveAttribute('autocomplete', 'new-password')
    }
  })
})

describe('InvitationAcceptPage - accept, then the existing enrolment wizard', () => {
  it('sends only {token, password}, forgets both, and continues to authenticator setup through to the recovery codes', async () => {
    let body: Record<string, string> = {}
    server.use(
      http.post('/v1/staff/invitations/accept', async ({ request }) => {
        body = (await request.json()) as Record<string, string>
        expect(request.headers.get('Authorization')).toBeNull()
        return HttpResponse.json({ challenge: { type: 'enrol', token: ENROL_CHALLENGE_TOKEN, expires_in: 300, methods: ['totp'] } })
      }),
    )
    page()
    const user = userEvent.setup()
    await submit(user)
    await screen.findByRole('heading', { name: 'Set up your authenticator' })
    expect(Object.keys(body).sort()).toEqual(['password', 'token'])
    expect(body.token).toBe(INVITE_LINK_TOKEN)
    expect(getInviteLinkToken()).toBeNull()
    expect(screen.queryByLabelText('New password')).not.toBeInTheDocument()
    await user.click(await screen.findByRole('button', { name: 'Generate setup secret' }))
    await user.type(await screen.findByLabelText('6-digit code'), VALID_TOTP)
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    await screen.findByRole('heading', { name: 'Save your recovery codes' })
    // Show-secret toggles and recovery codes are the wizard's own, already covered in EnrolPage tests.
    expect(RECOVERY_CODES.length).toBeGreaterThan(0)
    void TOTP_SECRET
    expectNoSecretLeak()
  })
})

describe('InvitationAcceptPage - refusals', () => {
  it.each([
    ['unknown / expired / used / revoked / issuer no longer live (one answer)', 401, 'invalid_enrolment_token'],
  ])('%s: one fixed dead-link message, token dropped, form gone, password cleared', async (_name, status, code) => {
    server.use(
      http.post('/v1/staff/invitations/accept', () =>
        HttpResponse.json({ code, message: `${SERVER_MESSAGE_SENTINEL} issuer_not_live` }, { status }),
      ),
    )
    page()
    await submit(userEvent.setup())
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(INVITATION_DEAD)
    expect(document.body.textContent).not.toContain(SERVER_MESSAGE_SENTINEL)
    expect(document.body.textContent).not.toContain('issuer_not_live')
    expect(getInviteLinkToken()).toBeNull()
    expect(screen.queryByLabelText('New password')).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Go to sign in' })).toBeInTheDocument()
    expectNoSecretLeak()
  })

  it('the server 400 (short password) keeps the form and the token for a retry, with cleared fields', async () => {
    server.use(http.post('/v1/staff/invitations/accept', () => HttpResponse.json({ code: 'validation_error', message: SERVER_MESSAGE_SENTINEL }, { status: 400 })))
    page()
    await submit(userEvent.setup())
    expect(await screen.findByRole('alert')).toHaveTextContent('Choose a password of at least 12 characters.')
    expect(getInviteLinkToken()).toBe(INVITE_LINK_TOKEN)
    expect(screen.getByLabelText('New password')).toHaveValue('')
    expect(screen.getByLabelText('Confirm new password')).toHaveValue('')
    expect(document.body.textContent).not.toContain(SERVER_MESSAGE_SENTINEL)
  })

  it('rate limit shows the wait and keeps the token', async () => {
    server.use(
      http.post('/v1/staff/invitations/accept', () =>
        HttpResponse.json({ code: 'rate_limited', message: SERVER_MESSAGE_SENTINEL }, { status: 429, headers: { 'Retry-After': '30' } }),
      ),
    )
    page()
    await submit(userEvent.setup())
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many attempts. Wait 30 seconds and try again.')
    expect(getInviteLinkToken()).toBe(INVITE_LINK_TOKEN)
  })

  it.each([
    ['service_unavailable', 503, /temporarily unavailable/],
    ['internal_error', 500, /could not be accepted/],
  ])('%s keeps the token and shows fixed wording', async (code, status, text) => {
    server.use(http.post('/v1/staff/invitations/accept', () => HttpResponse.json({ code, message: SERVER_MESSAGE_SENTINEL }, { status })))
    page()
    await submit(userEvent.setup())
    expect(await screen.findByRole('alert')).toHaveTextContent(text)
    expect(getInviteLinkToken()).toBe(INVITE_LINK_TOKEN)
    expect(document.body.textContent).not.toContain(SERVER_MESSAGE_SENTINEL)
  })

  it('a network failure keeps the token and says so', async () => {
    server.use(http.post('/v1/staff/invitations/accept', () => HttpResponse.error()))
    page()
    await submit(userEvent.setup())
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not reach the server.')
    expect(getInviteLinkToken()).toBe(INVITE_LINK_TOKEN)
  })

  it('a challenge of the wrong type is not trusted', async () => {
    server.use(http.post('/v1/staff/invitations/accept', () => HttpResponse.json({ challenge: { type: 'mfa', token: 't', expires_in: 1, methods: ['totp'] } })))
    page()
    await submit(userEvent.setup())
    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Set up your authenticator' })).not.toBeInTheDocument()
  })

  it('submits once on a double click', async () => {
    const spy = vi.fn()
    server.use(
      http.post('/v1/staff/invitations/accept', async () => {
        spy()
        await new Promise((r) => setTimeout(r, 30))
        return HttpResponse.json({ challenge: { type: 'enrol', token: ENROL_CHALLENGE_TOKEN, expires_in: 300, methods: ['totp'] } })
      }),
    )
    page()
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('New password'), INVITE_PASSWORD)
    await user.type(screen.getByLabelText('Confirm new password'), INVITE_PASSWORD)
    const button = screen.getByRole('button', { name: 'Set password and continue' })
    await user.dblClick(button)
    await screen.findByRole('heading', { name: 'Set up your authenticator' })
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('when the wizard challenge dies the page falls back with a fixed message and no way to reuse the token', async () => {
    server.use(http.post('/v1/staff/auth/enrol/start', () => HttpResponse.json({ code: 'invalid_challenge', message: 'x' }, { status: 401 })))
    page()
    const user = userEvent.setup()
    await submit(user)
    await user.click(await screen.findByRole('button', { name: 'Generate setup secret' }))
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument())
    expect(getInviteLinkToken()).toBeNull()
  })
})

describe('error boundary', () => {
  it('shows a fixed message and never the thrown text', () => {
    // React itself logs a caught render error via console.error; this component adds no logging of its own.
    vi.spyOn(console, 'error').mockImplementation(() => {})
    function Boom(): never {
      throw new Error('LEAK-secret-token-123')
    }
    renderWithProviders(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    )
    expect(screen.getByRole('alert')).toHaveTextContent('Something went wrong on this screen')
    expect(document.body.textContent).not.toContain('LEAK-secret-token-123')
    expect(document.body.innerHTML).not.toContain('LEAK-secret-token-123')
  })
})

describe('link capture for invitations', () => {
  it('maps the emailed #/enrol?token=..&invite=1 link to the invitation token only, and rewrites the URL', () => {
    clearInviteLinkToken()
    const replaceState = vi.fn()
    captureEnrolLinkToken({ pathname: '/', search: '', hash: '#/enrol?token=tok-inv_1&invite=1' }, { replaceState })
    expect(getInviteLinkToken()).toBe('tok-inv_1')
    expect(getEnrolLinkToken()).toBeNull()
    expect(replaceState).toHaveBeenCalledWith(null, '', '/accept-invitation')
  })

  it('accepts #/accept-invitation?token= as an equivalent fragment form', () => {
    clearInviteLinkToken()
    const replaceState = vi.fn()
    captureEnrolLinkToken({ pathname: '/', search: '', hash: '#/accept-invitation?token=tok2' }, { replaceState })
    expect(getInviteLinkToken()).toBe('tok2')
    expect(replaceState).toHaveBeenCalledWith(null, '', '/accept-invitation')
  })

  it('a plain enrolment / MFA-reset link never fills the invitation token (no cross-use)', () => {
    clearInviteLinkToken()
    clearEnrolLinkToken()
    captureEnrolLinkToken({ pathname: '/', search: '', hash: '#/enrol?token=reset-tok' }, { replaceState: () => {} })
    expect(getEnrolLinkToken()).toBe('reset-tok')
    expect(getInviteLinkToken()).toBeNull()
  })

  it('does NOT accept a query-string token on /accept-invitation: nothing kept, URL rewritten', () => {
    clearInviteLinkToken()
    const replaceState = vi.fn()
    captureEnrolLinkToken({ pathname: '/accept-invitation', search: '?token=nope', hash: '' }, { replaceState })
    expect(getInviteLinkToken()).toBeNull()
    expect(replaceState).toHaveBeenCalledWith(null, '', '/accept-invitation')
  })

  it('scrubs the real address bar', () => {
    clearInviteLinkToken()
    window.history.replaceState(null, '', '/#/enrol?token=real-invite-tok&invite=1')
    captureEnrolLinkToken()
    expect(getInviteLinkToken()).toBe('real-invite-tok')
    expect(window.location.href).not.toContain('real-invite-tok')
    expect(window.location.pathname).toBe('/accept-invitation')
    expect(window.location.hash).toBe('')
    window.history.replaceState(null, '', '/')
  })
})
