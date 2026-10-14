import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { clearSession, getAccessToken } from '../../auth/tokenStore'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { signInAs } from '../../test/session'
import { errorBody, GOOD_CODE, installStepUpFixture, SERVER_MESSAGE_SENTINEL, STEPPED_JWT, stepUpRequiredResponse } from '../../test/stepUp'
import { STEP_UP_MESSAGES } from '../../auth/stepUpCoordinator'
import { SecurityPage } from './SecurityPage'

afterEach(() => vi.restoreAllMocks())

const NEW_CODES = Array.from({ length: 10 }, (_, i) => `NEWCD${i}-ZXY${i}`)

interface Calls {
  regenerate: Array<{ auth: string | null; cache: string; method: string; body: string }>
  statusReads: number
}

/** Regenerate endpoint: step_up_required until the request carries the stepped-up token (or `mode` overrides). */
function install(mode: 'step-up' | 'direct' | ((n: number) => Response) = 'step-up') {
  const f = installStepUpFixture()
  const calls: Calls = { regenerate: [], statusReads: 0 }
  server.use(
    http.get('/v1/staff/me/mfa', () => {
      calls.statusReads++
      return HttpResponse.json({
        enrolled: true,
        factor_type: 'totp',
        activated_at: '2026-10-01T09:30:00Z',
        recovery_codes_remaining: calls.statusReads > 1 ? 10 : 2,
        hard_locked: false,
        locked_until: null,
      })
    }),
    http.post('/v1/staff/mfa/recovery-codes/regenerate', async ({ request }) => {
      const auth = request.headers.get('Authorization')
      calls.regenerate.push({ auth, cache: request.cache, method: request.method, body: await request.text() })
      if (typeof mode === 'function') return mode(calls.regenerate.length)
      if (mode === 'direct' || auth === `Bearer ${STEPPED_JWT}`) return HttpResponse.json({ recovery_codes: NEW_CODES })
      return stepUpRequiredResponse(300)
    }),
  )
  return { f, calls }
}

async function start(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'Regenerate recovery codes' }))
  await user.click(screen.getByRole('button', { name: 'Generate new codes' }))
}

async function stepUpWith(user: ReturnType<typeof userEvent.setup>, code = GOOD_CODE) {
  await screen.findByRole('dialog', { name: 'Confirm it is you' })
  await user.type(screen.getByLabelText('6-digit code'), code)
  await user.click(screen.getByRole('button', { name: 'Verify' }))
}

describe('SecurityPage: recovery-code regeneration', () => {
  it('asks for confirmation first and sends nothing until it is given', async () => {
    signInAs({})
    const { calls } = install()
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Regenerate recovery codes' }))
    expect(screen.getByRole('group', { name: 'Confirm regeneration' })).toHaveTextContent(/old ones stop working immediately/)
    expect(calls.regenerate).toHaveLength(0)
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('group', { name: 'Confirm regeneration' })).not.toBeInTheDocument()
    expect(calls.regenerate).toHaveLength(0)
  })

  it('step-up first: 403 -> modal -> TOTP -> the request is re-sent once and the codes are shown', async () => {
    signInAs({})
    const { f, calls } = install()
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    await stepUpWith(user)
    expect(await screen.findByRole('heading', { name: 'Your new recovery codes' })).toHaveFocus()
    for (const c of NEW_CODES) expect(screen.getByText(c)).toBeInTheDocument()
    expect(calls.regenerate).toHaveLength(2)
    expect(calls.regenerate[0].auth).not.toBe(`Bearer ${STEPPED_JWT}`)
    expect(calls.regenerate[1].auth).toBe(`Bearer ${STEPPED_JWT}`)
    expect(calls.regenerate.every((c) => c.method === 'POST' && c.body === '' && c.cache === 'no-store')).toBe(true)
    expect(f.stepUps).toEqual([GOOD_CODE])
    // No second action while the codes are shown.
    expect(screen.queryByRole('button', { name: 'Regenerate recovery codes' })).not.toBeInTheDocument()
  })

  it('works without a prompt when the server needs no step-up', async () => {
    signInAs({})
    const { f } = install('direct')
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    expect(await screen.findByRole('heading', { name: 'Your new recovery codes' })).toBeInTheDocument()
    expect(f.stepUps).toHaveLength(0)
  })

  it('one-time display: Done is gated on the acknowledgement; afterwards the codes are gone and the count refreshes', async () => {
    signInAs({})
    const { calls } = install('direct')
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    await screen.findByRole('heading', { name: 'Your new recovery codes' })
    const done = screen.getByRole('button', { name: 'Done' })
    expect(done).toBeDisabled()
    await user.click(screen.getByRole('checkbox', { name: 'I have stored these codes' }))
    expect(done).toBeEnabled()
    await user.click(done)
    for (const c of NEW_CODES) expect(screen.queryByText(c)).not.toBeInTheDocument()
    expect(document.body.innerHTML).not.toContain('NEWCD')
    await waitFor(() => expect(screen.getByText('10')).toBeInTheDocument())
    expect(calls.statusReads).toBeGreaterThan(1)
    expect(screen.getByRole('button', { name: 'Regenerate recovery codes' })).toBeInTheDocument()
  })

  it('prompts on beforeunload while the codes are unacknowledged, and while the request is in flight, not after', async () => {
    signInAs({})
    install('direct')
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    const fire = () => {
      const e = new Event('beforeunload', { cancelable: true })
      window.dispatchEvent(e)
      return e.defaultPrevented
    }
    await screen.findByText('Enrolled')
    expect(fire()).toBe(false)
    await start(user)
    await screen.findByRole('heading', { name: 'Your new recovery codes' })
    expect(fire()).toBe(true)
    await user.click(screen.getByRole('checkbox', { name: 'I have stored these codes' }))
    expect(fire()).toBe(false)
  })

  it('prompts on beforeunload while the step-up modal is up (the request is in flight)', async () => {
    signInAs({})
    install()
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    await screen.findByRole('dialog', { name: 'Confirm it is you' })
    const e = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(e)
    expect(e.defaultPrevented).toBe(true)
  })

  it('S7a lesson: no timer wipes the codes (an hour of fake time later they are still shown and unacknowledged)', async () => {
    signInAs({})
    install('direct')
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    await screen.findByRole('heading', { name: 'Your new recovery codes' })
    vi.useFakeTimers()
    try {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(60 * 60 * 1000)
      })
    } finally {
      vi.useRealTimers()
    }
    for (const c of NEW_CODES) expect(screen.getByText(c)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Done' })).toBeDisabled()
  })

  it('the codes are held in component state only: leaving the page drops them', async () => {
    signInAs({})
    install('direct')
    const { unmount } = renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    await screen.findByRole('heading', { name: 'Your new recovery codes' })
    unmount()
    expect(document.body.innerHTML).not.toContain('NEWCD')
  })

  it('ending the session removes the codes from the DOM (the page is torn down by the route guard)', async () => {
    signInAs({})
    install('direct')
    const { unmount } = renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    await screen.findByRole('heading', { name: 'Your new recovery codes' })
    act(() => clearSession())
    unmount() // RequireAuth redirects away from the page when the session is gone
    expect(document.body.innerHTML).not.toContain('NEWCD')
  })

  it('cancelling the step-up: a clear message, the existing codes are unchanged, no codes appear, nothing re-sent', async () => {
    signInAs({})
    const { calls } = install()
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    await screen.findByRole('dialog', { name: 'Confirm it is you' })
    await user.keyboard('{Escape}')
    expect(await screen.findByText('Cancelled. Your existing recovery codes are unchanged.')).toBeInTheDocument()
    expect(calls.regenerate).toHaveLength(1)
    expect(screen.queryByRole('heading', { name: 'Your new recovery codes' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Regenerate recovery codes' })).toBeInTheDocument()
  })

  it('lockout during step-up: fixed message, existing codes unchanged', async () => {
    signInAs({})
    const { f, calls } = install()
    server.use(http.post('/v1/staff/auth/step-up', () => errorBody('mfa_locked', 403)))
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    await stepUpWith(user)
    expect(await screen.findByText(/Your identity could not be confirmed/)).toBeInTheDocument()
    expect(calls.regenerate).toHaveLength(1)
    expect(f.stepUps).toHaveLength(0)
    expect(document.body).not.toHaveTextContent(SERVER_MESSAGE_SENTINEL)
  })

  it('expired session (401 session_revoked) ends the session with the sign-in message', async () => {
    signInAs({})
    install(() => errorBody('session_revoked', 401))
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    expect(await screen.findByText(STEP_UP_MESSAGES.session_ended)).toBeInTheDocument()
    expect(getAccessToken()).toBeNull()
  })

  it('a second step_up_required after the step-up fails visibly and loops no further', async () => {
    signInAs({})
    const { f, calls } = install(() => stepUpRequiredResponse())
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    await stepUpWith(user)
    expect(await screen.findByText(STEP_UP_MESSAGES.not_effective)).toBeInTheDocument()
    expect(calls.regenerate).toHaveLength(2)
    expect(f.stepUps).toHaveLength(1)
  })

  it.each([
    [503, 'service_unavailable', 'Recovery codes are temporarily unavailable. Try again.'],
    [403, 'forbidden', 'You are not allowed to regenerate recovery codes.'],
    [500, 'internal_error', 'Recovery codes could not be regenerated. Try again.'],
  ])('%i %s renders a fixed message, never the server text', async (st, code, text) => {
    signInAs({})
    install(() => errorBody(code, st))
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    expect(await screen.findByText(text)).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent(SERVER_MESSAGE_SENTINEL)
    expect(screen.queryByRole('heading', { name: 'Your new recovery codes' })).not.toBeInTheDocument()
  })

  it('a double click on Generate sends one request', async () => {
    signInAs({})
    const { calls } = install('direct')
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Regenerate recovery codes' }))
    await user.dblClick(screen.getByRole('button', { name: 'Generate new codes' }))
    await screen.findByRole('heading', { name: 'Your new recovery codes' })
    expect(calls.regenerate).toHaveLength(1)
  })

  it('no recovery code, step-up code or token reaches Web Storage or the console', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    const logs = (['log', 'info', 'warn', 'error', 'debug'] as const).map((m) => vi.spyOn(console, m).mockImplementation(() => {}))
    signInAs({})
    install()
    renderWithProviders(<SecurityPage />)
    const user = userEvent.setup()
    await start(user)
    await stepUpWith(user)
    await screen.findByRole('heading', { name: 'Your new recovery codes' })
    await user.click(screen.getByRole('checkbox', { name: 'I have stored these codes' }))
    await user.click(screen.getByRole('button', { name: 'Done' }))
    const secrets = [...NEW_CODES, GOOD_CODE, STEPPED_JWT]
    const stored = JSON.stringify([...Object.entries(localStorage), ...Object.entries(sessionStorage), ...setItem.mock.calls])
    const logged = JSON.stringify(logs.flatMap((l) => l.mock.calls))
    for (const s of secrets) {
      expect(stored).not.toContain(s)
      expect(logged).not.toContain(s)
    }
    expect(localStorage.length + sessionStorage.length).toBe(0)
  })
})
