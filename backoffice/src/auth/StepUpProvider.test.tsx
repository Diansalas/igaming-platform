import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse, delay } from 'msw'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiFetch } from '../api/client'
import { Modal } from '../components/Modal'
import { server } from '../test/mswServer'
import { renderWithProviders } from '../test/renderWithProviders'
import { signInAs } from '../test/session'
import {
  errorBody,
  GOOD_CODE,
  Harness,
  installStepUpFixture,
  SERVER_MESSAGE_SENTINEL,
  STEPPED_JWT,
  stepUpRequiredResponse,
} from '../test/stepUp'
import { makeTestJwt } from '../test/jwt'
import { STEP_UP_GENERIC, STEP_UP_INVALID_CODE, STEP_UP_MALFORMED, STEP_UP_NETWORK, STEP_UP_UNAVAILABLE } from './StepUpProvider'
import { STEP_UP_MESSAGES } from './stepUpCoordinator'
import { getAccessToken, getRefreshToken } from './tokenStore'

afterEach(() => vi.restoreAllMocks())

const dialog = () => screen.findByRole('dialog', { name: 'Confirm it is you' })
const code = () => screen.getByLabelText('6-digit code') as HTMLInputElement

async function open(user: ReturnType<typeof userEvent.setup>, button = 'Run') {
  await user.click(screen.getByRole('button', { name: button }))
  await dialog()
}

async function enter(user: ReturnType<typeof userEvent.setup>, value = GOOD_CODE) {
  await user.type(code(), value)
  await user.click(screen.getByRole('button', { name: 'Verify' }))
}

describe('step-up: happy path and retry-once', () => {
  it('opens the modal on 403 step_up_required, swaps the token, re-sends ONCE with the same body and Idempotency-Key', async () => {
    signInAs({})
    const f = installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    expect(f.guarded).toHaveLength(1)
    expect(screen.getByRole('dialog')).toHaveAttribute('aria-modal', 'true')
    expect(code()).toHaveFocus()
    await enter(user)
    expect(await screen.findByText('ok 1')).toBeInTheDocument()

    expect(f.stepUps).toEqual([GOOD_CODE])
    expect(f.guarded).toHaveLength(2)
    expect(f.guarded[1].auth).toBe(`Bearer ${STEPPED_JWT}`)
    expect(f.guarded[1].body).toBe(f.guarded[0].body)
    expect(f.guarded[1].idempotencyKey).toBe('key-1')
    expect(f.guarded[0].idempotencyKey).toBe('key-1')
    // The step-up call carried the CURRENT (pre-step-up) bearer.
    expect(f.stepUpAuth[0]).not.toBe(`Bearer ${STEPPED_JWT}`)
    expect(getAccessToken()).toBe(STEPPED_JWT)
    // The refresh token does not change on step-up and no refresh was attempted.
    expect(getRefreshToken()).toBe('refresh-token-for-test')
    expect(f.refreshCalls).toBe(0)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('explains the prompt with max_age (body) but does not use it for anything else', async () => {
    signInAs({})
    installStepUpFixture({ maxAge: 120 })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    expect(screen.getByRole('dialog')).toHaveAccessibleDescription(/last 2 minutes/)
  })

  it('falls back to the WWW-Authenticate max_age when the body has none', async () => {
    signInAs({})
    installStepUpFixture()
    server.use(
      http.post('/v1/admin/thing', () =>
        HttpResponse.json({ code: 'step_up_required', message: 'x' }, { status: 403, headers: { 'WWW-Authenticate': 'Bearer max_age="600"' } }),
      ),
    )
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    expect(screen.getByRole('dialog')).toHaveAccessibleDescription(/last 10 minutes/)
  })

  it('never loops: a second step_up_required after the re-send fails visibly with no second prompt', async () => {
    signInAs({})
    const f = installStepUpFixture({ alwaysRefuse: true })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    expect(await screen.findByText(`err 1 step_up_required: ${STEP_UP_MESSAGES.not_effective}`)).toBeInTheDocument()
    expect(f.guarded).toHaveLength(2)
    expect(f.stepUps).toHaveLength(1)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('a re-send that answers something else (403 forbidden) is surfaced as-is, with no further prompt', async () => {
    signInAs({})
    const f = installStepUpFixture()
    server.use(
      http.post('/v1/admin/thing', ({ request }) => {
        f.guarded.push({ auth: request.headers.get('Authorization'), body: '', idempotencyKey: null })
        return request.headers.get('Authorization') === `Bearer ${STEPPED_JWT}` ? errorBody('forbidden', 403) : stepUpRequiredResponse()
      }),
    )
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    expect(await screen.findByText(/^err 1 forbidden:/)).toBeInTheDocument()
    expect(f.guarded).toHaveLength(2)
  })

  it('a state-based 409 on the re-send is passed through (no extra retry)', async () => {
    signInAs({})
    const f = installStepUpFixture()
    server.use(
      http.post('/v1/admin/thing', ({ request }) => {
        f.guarded.push({ auth: request.headers.get('Authorization'), body: '', idempotencyKey: null })
        return request.headers.get('Authorization') === `Bearer ${STEPPED_JWT}` ? errorBody('conflict', 409) : stepUpRequiredResponse()
      }),
    )
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    expect(await screen.findByText(/^err 1 conflict:/)).toBeInTheDocument()
    expect(f.guarded).toHaveLength(2)
  })
})

describe('step-up: concurrency (single in-flight)', () => {
  it('three concurrent refusals share ONE modal and ONE step-up call; each request is re-sent exactly once', async () => {
    signInAs({})
    const f = installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Run all' }))
    await dialog()
    await waitFor(() => expect(f.guarded).toHaveLength(3))
    expect(screen.getAllByRole('dialog')).toHaveLength(1)
    await enter(user)
    await waitFor(() => expect(screen.getAllByText(/^ok /)).toHaveLength(3))
    expect(f.stepUps).toHaveLength(1)
    expect(f.guarded).toHaveLength(6)
    const resent = f.guarded.slice(3)
    expect(resent.every((r) => r.auth === `Bearer ${STEPPED_JWT}`)).toBe(true)
    expect(resent.map((r) => r.idempotencyKey).sort()).toEqual(['key-1', 'key-2', 'key-3'])
  })

  it('cancelling fails every queued request and re-sends none', async () => {
    signInAs({})
    const f = installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Run all' }))
    await dialog()
    await waitFor(() => expect(f.guarded).toHaveLength(3))
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.getAllByText(/^err \d step_up_cancelled:/)).toHaveLength(3))
    expect(f.guarded).toHaveLength(3)
    expect(f.stepUps).toHaveLength(0)
  })

  it('a request sent BEFORE the step-up and refused AFTER it is released without a second prompt', async () => {
    signInAs({})
    const f = installStepUpFixture()
    // Request 2 is slow: it is sent with the old token, answered only after the step-up completed.
    server.use(
      http.post('/v1/admin/thing', async ({ request }) => {
        const body = (await request.clone().json()) as { i: number }
        const auth = request.headers.get('Authorization')
        f.guarded.push({ auth, body: JSON.stringify(body), idempotencyKey: request.headers.get('Idempotency-Key') })
        if (body.i === 2 && auth !== `Bearer ${STEPPED_JWT}`) await delay(400)
        return auth === `Bearer ${STEPPED_JWT}` ? HttpResponse.json({ done: true }) : stepUpRequiredResponse()
      }),
    )
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Run all' }))
    await dialog()
    await enter(user)
    await waitFor(() => expect(screen.getAllByText(/^ok /)).toHaveLength(3))
    expect(f.stepUps).toHaveLength(1)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
})

describe('step-up: cancel', () => {
  it('Cancel button: the original action fails with a clear message, nothing is re-sent, the session survives', async () => {
    signInAs({})
    const f = installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(await screen.findByText(`err 1 step_up_cancelled: ${STEP_UP_MESSAGES.cancelled}`)).toBeInTheDocument()
    expect(f.guarded).toHaveLength(1)
    expect(f.stepUps).toHaveLength(0)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent(STEP_UP_MESSAGES.cancelled)
    expect(getAccessToken()).not.toBeNull()
  })

  it('Escape cancels and returns focus to the control that started the action', async () => {
    signInAs({})
    const f = installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    const trigger = screen.getByRole('button', { name: 'Run' })
    trigger.focus()
    await user.click(trigger)
    await dialog()
    await user.keyboard('{Escape}')
    expect(await screen.findByText(/^err 1 step_up_cancelled:/)).toBeInTheDocument()
    expect(f.guarded).toHaveLength(1)
    expect(trigger).toHaveFocus()
  })

  it('Escape closes only the step-up modal, not a dialog underneath it', async () => {
    signInAs({})
    installStepUpFixture()
    const onClose = vi.fn()
    renderWithProviders(
      <>
        <Modal title="Governed dialog" onClose={onClose}>
          <Harness />
        </Modal>
      </>,
    )
    const user = userEvent.setup()
    await open(user)
    await user.keyboard('{Escape}')
    await screen.findByText(/^err 1 step_up_cancelled:/)
    expect(onClose).not.toHaveBeenCalled()
  })

  it('the code typed before cancelling is wiped from the DOM and not reachable afterwards', async () => {
    signInAs({})
    installStepUpFixture()
    const { container } = renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await user.type(code(), '98')
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    await screen.findByText(/^err 1 step_up_cancelled:/)
    expect(container.ownerDocument.querySelectorAll('input').length).toBe(0)
  })

  it('with no modal host mounted a refusal fails closed (no hang, no re-send)', async () => {
    signInAs({})
    const f = installStepUpFixture()
    await expect(apiFetch('/v1/admin/thing', { method: 'POST', body: '{}' })).rejects.toMatchObject({ code: 'step_up_failed' })
    expect(f.guarded).toHaveLength(1)
  })
})

describe('step-up: error paths (fixed strings keyed on the code, server text never rendered)', () => {
  it('invalid / replayed code: stays open, clears the field, fixed message, session intact, no refresh; then succeeds', async () => {
    signInAs({})
    const f = installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user, '000000')
    expect(await screen.findByRole('alert')).toHaveTextContent(STEP_UP_INVALID_CODE)
    expect(document.body).not.toHaveTextContent(SERVER_MESSAGE_SENTINEL)
    expect(code()).toHaveValue('')
    expect(code()).toHaveFocus()
    expect(code()).toHaveAttribute('aria-invalid', 'true')
    expect(code()).toHaveAccessibleDescription(STEP_UP_INVALID_CODE)
    expect(getAccessToken()).not.toBeNull()
    expect(f.refreshCalls).toBe(0)
    expect(f.guarded).toHaveLength(1)
    await enter(user)
    expect(await screen.findByText('ok 1')).toBeInTheDocument()
    expect(f.guarded).toHaveLength(2)
  })

  it('a malformed code is rejected locally without spending a server attempt', async () => {
    signInAs({})
    const f = installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user, '12ab')
    expect(await screen.findByRole('alert')).toHaveTextContent(STEP_UP_MALFORMED)
    expect(f.stepUps).toHaveLength(0)
  })

  it('accepts a code typed with spaces or a dash', async () => {
    signInAs({})
    const f = installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user, '123 456')
    await screen.findByText('ok 1')
    expect(f.stepUps).toEqual(['123456'])
  })

  it('mfa_locked closes the prompt; the original action fails visibly with the lockout message and is not re-sent', async () => {
    signInAs({})
    const f = installStepUpFixture({ stepUp: () => errorBody('mfa_locked', 403) })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    expect(await screen.findByText(`err 1 step_up_failed: ${STEP_UP_MESSAGES.locked}`)).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent(/Too many incorrect codes/)
    expect(f.guarded).toHaveLength(1)
    expect(getAccessToken()).not.toBeNull()
    expect(document.body).not.toHaveTextContent(SERVER_MESSAGE_SENTINEL)
  })

  it('rate_limited keeps the prompt open and shows the wait from Retry-After', async () => {
    signInAs({})
    installStepUpFixture({ stepUp: () => errorBody('rate_limited', 429, { 'Retry-After': '17' }) })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    expect(await screen.findByRole('alert')).toHaveTextContent('Wait 17 seconds')
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent(SERVER_MESSAGE_SENTINEL)
  })

  it('503 gives one uniform message and keeps the prompt open for a retry', async () => {
    signInAs({})
    let n = 0
    const f = installStepUpFixture({
      stepUp: () => (++n === 1 ? errorBody('service_unavailable', 503) : HttpResponse.json({ access_token: STEPPED_JWT, token_type: 'Bearer', expires_in: 900, mfa_at: '', step_up_expires_at: { financial: '', critical: '' } })),
    })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    expect(await screen.findByRole('alert')).toHaveTextContent(STEP_UP_UNAVAILABLE)
    await enter(user)
    expect(await screen.findByText('ok 1')).toBeInTheDocument()
    expect(f.guarded).toHaveLength(2)
  })

  it('network failure and unknown codes get fixed messages', async () => {
    signInAs({})
    let n = 0
    installStepUpFixture({ stepUp: () => (++n === 1 ? HttpResponse.error() : errorBody('internal_error', 500)) })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    expect(await screen.findByRole('alert')).toHaveTextContent(STEP_UP_NETWORK)
    await enter(user)
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(STEP_UP_GENERIC))
    expect(document.body).not.toHaveTextContent(SERVER_MESSAGE_SENTINEL)
  })

  it('401 session_revoked (also: five failed step-ups revoking the session) ends the session, closes the prompt, fails the action', async () => {
    signInAs({})
    const f = installStepUpFixture({ stepUp: () => errorBody('session_revoked', 401) })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    expect(await screen.findByText(/^err 1 session_revoked:/)).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(getAccessToken()).toBeNull()
    expect(getRefreshToken()).toBeNull()
    expect(f.refreshCalls).toBe(0)
    expect(f.guarded).toHaveLength(1)
    expect(screen.getByRole('status')).toHaveTextContent(STEP_UP_MESSAGES.session_ended)
  })

  it('401 unauthorized from step-up: one refresh attempt, then sign out when it fails', async () => {
    signInAs({})
    const f = installStepUpFixture({ stepUp: () => errorBody('unauthorized', 401) })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    await screen.findByText(/^err 1 session_revoked:/)
    expect(f.refreshCalls).toBe(1)
    expect(getAccessToken()).toBeNull()
  })

  it('403 forbidden from step-up (not a staff token) signs out', async () => {
    signInAs({})
    installStepUpFixture({ stepUp: () => errorBody('forbidden', 403) })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    await screen.findByText(/^err 1 session_revoked:/)
    expect(getAccessToken()).toBeNull()
  })

  it('the step-up endpoint answering step_up_required never opens another prompt (no recursion)', async () => {
    signInAs({})
    const f = installStepUpFixture({ stepUp: () => stepUpRequiredResponse() })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Direct' }))
    expect(await screen.findByText('direct step_up_required')).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(f.stepUps).toHaveLength(1)
  })

  it('fails closed when the step-up token is for another subject or is not aal2', async () => {
    signInAs({})
    installStepUpFixture({
      stepUp: () =>
        HttpResponse.json({ access_token: makeTestJwt({ sub: 'someone-else' }), token_type: 'Bearer', expires_in: 900, mfa_at: '', step_up_expires_at: { financial: '', critical: '' } }),
    })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    await screen.findByText(/^err 1 session_revoked:/)
    expect(getAccessToken()).toBeNull()
  })

  it('a double submit sends the code once', async () => {
    signInAs({})
    const f = installStepUpFixture({
      stepUp: async () => {
        await delay(150)
        return HttpResponse.json({ access_token: STEPPED_JWT, token_type: 'Bearer', expires_in: 900, mfa_at: '', step_up_expires_at: { financial: '', critical: '' } })
      },
    })
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await user.type(code(), GOOD_CODE)
    const verify = screen.getByRole('button', { name: 'Verify' })
    await user.dblClick(verify)
    await user.keyboard('{Enter}')
    await screen.findByText('ok 1')
    expect(f.stepUps).toHaveLength(1)
  })

  it('the session ending while the prompt is open (another call signs out) fails the action and closes the prompt', async () => {
    signInAs({})
    const f = installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    const { clearSession } = await import('./tokenStore')
    act(() => clearSession())
    expect(await screen.findByText(/^err 1 session_revoked:/)).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(f.guarded).toHaveLength(1)
  })
})

describe('step-up: accessibility', () => {
  it('has dialog semantics, a labelled field, and an accessible name and description', async () => {
    signInAs({})
    installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    const d = screen.getByRole('dialog', { name: 'Confirm it is you' })
    expect(d).toHaveAttribute('aria-modal', 'true')
    expect(d).toHaveAccessibleDescription(/authenticator app/)
    expect(within(d).getByLabelText('6-digit code')).toHaveAttribute('autocomplete', 'one-time-code')
    expect(within(d).getByLabelText('6-digit code')).toHaveAttribute('inputmode', 'numeric')
  })

  it('traps Tab and Shift+Tab inside the dialog', async () => {
    signInAs({})
    installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    const d = screen.getByRole('dialog')
    expect(code()).toHaveFocus()
    await user.tab()
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus()
    await user.tab()
    expect(screen.getByRole('button', { name: 'Verify' })).toHaveFocus()
    await user.tab()
    expect(code()).toHaveFocus() // wrapped
    await user.tab({ shift: true })
    expect(screen.getByRole('button', { name: 'Verify' })).toHaveFocus() // wrapped backwards
    for (let i = 0; i < 6; i++) {
      await user.tab()
      expect(d.contains(document.activeElement)).toBe(true)
    }
  })

  it('pulls focus back in if it somehow sits outside the dialog', async () => {
    signInAs({})
    installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    ;(document.activeElement as HTMLElement).blur()
    await user.tab()
    expect(screen.getByRole('dialog').contains(document.activeElement)).toBe(true)
  })

  it('submits with Enter', async () => {
    signInAs({})
    installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await user.type(code(), `${GOOD_CODE}{Enter}`)
    expect(await screen.findByText('ok 1')).toBeInTheDocument()
  })
})

describe('step-up: no secret persistence or logging', () => {
  it('the code and tokens reach neither Web Storage, nor the console, nor the DOM after the flow', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    const logs = (['log', 'info', 'warn', 'error', 'debug'] as const).map((m) => vi.spyOn(console, m).mockImplementation(() => {}))
    signInAs({})
    installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    // A wrong code first, then the right one: both are sentinels for "never stored".
    await open(user)
    await enter(user, '654321')
    await screen.findByRole('alert')
    expect(code()).toHaveValue('')
    await enter(user, GOOD_CODE)
    await screen.findByText('ok 1')

    const stored = JSON.stringify([...Object.entries(localStorage), ...Object.entries(sessionStorage), ...setItem.mock.calls])
    for (const s of ['654321', GOOD_CODE, STEPPED_JWT, 'refresh-token-for-test']) expect(stored).not.toContain(s)
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
    const logged = JSON.stringify(logs.flatMap((l) => l.mock.calls))
    for (const s of ['654321', GOOD_CODE, STEPPED_JWT]) expect(logged).not.toContain(s)
    expect(document.body.innerHTML).not.toContain('654321')
    expect(document.querySelectorAll('input')).toHaveLength(0)
  })

  it('the step-up request is sent no-store', async () => {
    signInAs({})
    installStepUpFixture()
    let cache = ''
    server.use(
      http.post('/v1/staff/auth/step-up', ({ request }) => {
        cache = request.cache
        return HttpResponse.json({ access_token: STEPPED_JWT, token_type: 'Bearer', expires_in: 900, mfa_at: '', step_up_expires_at: { financial: '', critical: '' } })
      }),
    )
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await open(user)
    await enter(user)
    await screen.findByText('ok 1')
    expect(cache).toBe('no-store')
  })
})

describe('step-up: unmount while open', () => {
  it('unmounting the host fails the waiting action instead of hanging', async () => {
    signInAs({})
    installStepUpFixture()
    const { unmount } = renderWithProviders(<Harness />)
    const p = apiFetch('/v1/admin/thing', { method: 'POST', body: '{}' }).catch((e) => e)
    await dialog()
    unmount()
    expect(await p).toMatchObject({ code: 'step_up_cancelled' })
  })
})
