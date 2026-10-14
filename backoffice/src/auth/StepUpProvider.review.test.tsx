// ADR 0119 S7b security-review follow-ups (F1-F7).
import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse, delay } from 'msw'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { staffStepUp } from '../api/auth'
import { refreshSessionOnce } from '../api/client'
import { ApiError } from '../api/types'
import { queryClient } from '../app/queryClient'
import { SecurityPage } from '../features/account/SecurityPage'
import { makeTestJwt } from '../test/jwt'
import { server } from '../test/mswServer'
import { renderWithProviders } from '../test/renderWithProviders'
import { signInAs } from '../test/session'
import { GOOD_CODE, Harness, installStepUpFixture, STEPPED_JWT, stepUpRequiredResponse } from '../test/stepUp'
import { STEP_UP_RENEWED } from './StepUpProvider'
import { clearSession, getAccessToken, InsufficientAssuranceError, replaceAccessToken, setSession } from './tokenStore'

const dialog = () => screen.findByRole('dialog', { name: 'Confirm it is you' })
const code = () => screen.getByLabelText('6-digit code') as HTMLInputElement

describe('F1: leaving My security while the step-up modal is open', () => {
  it('page unmounted, host still mounted: verifying completes the step-up but the regeneration is NOT re-sent', async () => {
    signInAs({})
    const f = installStepUpFixture()
    let regenerates = 0
    server.use(
      http.get('/v1/staff/me/mfa', () =>
        HttpResponse.json({ enrolled: true, factor_type: 'totp', activated_at: null, recovery_codes_remaining: 5, hard_locked: false, locked_until: null }),
      ),
      http.post('/v1/staff/mfa/recovery-codes/regenerate', () => {
        regenerates++
        return stepUpRequiredResponse()
      }),
    )
    let leave: () => void = () => undefined
    function Routes() {
      const [show, setShow] = useState(true)
      leave = () => setShow(false)
      return show ? <SecurityPage /> : <div>elsewhere</div>
    }
    renderWithProviders(<Routes />)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Regenerate recovery codes' }))
    await user.click(screen.getByRole('button', { name: 'Generate new codes' }))
    await dialog()
    act(() => leave())
    expect(screen.getByText('elsewhere')).toBeInTheDocument()
    await user.type(code(), GOOD_CODE)
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    await waitFor(() => expect(f.stepUps).toHaveLength(1))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(regenerates).toBe(1)
    expect(document.body.innerHTML).not.toContain('AAAAA')
  })
})

describe('F2: the TOTP value is wiped from the removed element', () => {
  it('Cancel wipes the typed value', async () => {
    signInAs({})
    installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Run' }))
    await dialog()
    const el = code()
    await user.type(el, '98')
    expect(el.value).toBe('98')
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    await screen.findByText(/^err 1 step_up_cancelled:/)
    expect(el.isConnected).toBe(false)
    expect(el.value).toBe('')
  })

  it('unmounting the host wipes the typed value', async () => {
    signInAs({})
    installStepUpFixture()
    const { unmount } = renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Run' }))
    await dialog()
    const el = code()
    await user.type(el, '7654')
    unmount()
    expect(el.value).toBe('')
  })

  it('a wrong code leaves nothing in the element', async () => {
    signInAs({})
    installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Run' }))
    await dialog()
    const el = code()
    await user.type(el, '000000')
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    await screen.findByRole('alert')
    expect(el.value).toBe('')
  })
})

describe('F3: the step-up call and token refresh', () => {
  it('waits for an in-flight refresh before sending the code', async () => {
    signInAs({})
    const order: string[] = []
    const fresh = makeTestJwt({ jti: 'refreshed' })
    server.use(
      http.post('/v1/auth/refresh', async () => {
        order.push('refresh-start')
        await delay(120)
        order.push('refresh-end')
        return HttpResponse.json({ access_token: fresh, refresh_token: 'r2', token_type: 'Bearer', expires_in: 900 })
      }),
      http.post('/v1/staff/auth/step-up', ({ request }) => {
        order.push(`step-up:${request.headers.get('Authorization') === `Bearer ${fresh}` ? 'fresh' : 'stale'}`)
        return HttpResponse.json({ access_token: STEPPED_JWT, token_type: 'Bearer', expires_in: 900, mfa_at: '', step_up_expires_at: { financial: '', critical: '' } })
      }),
    )
    const refresh = refreshSessionOnce()
    await staffStepUp(GOOD_CODE)
    await refresh
    expect(order).toEqual(['refresh-start', 'refresh-end', 'step-up:fresh'])
  })

  it('a 401 on the step-up call never auto-resends the code: refreshes once, keeps the prompt open, asks for a new code', async () => {
    signInAs({})
    let stepUps = 0
    let refreshes = 0
    server.use(
      http.post('/v1/admin/thing', ({ request }) =>
        request.headers.get('Authorization') === `Bearer ${STEPPED_JWT}` ? HttpResponse.json({ done: true }) : stepUpRequiredResponse(),
      ),
      http.post('/v1/auth/refresh', () => {
        refreshes++
        return HttpResponse.json({ access_token: makeTestJwt({ jti: 'r' }), refresh_token: 'r2', token_type: 'Bearer', expires_in: 900 })
      }),
      http.post('/v1/staff/auth/step-up', () => {
        stepUps++
        if (stepUps === 1) return HttpResponse.json({ code: 'unauthorized', message: 'x' }, { status: 401 })
        return HttpResponse.json({ access_token: STEPPED_JWT, token_type: 'Bearer', expires_in: 900, mfa_at: '', step_up_expires_at: { financial: '', critical: '' } })
      }),
    )
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Run' }))
    await dialog()
    await user.type(code(), GOOD_CODE)
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(STEP_UP_RENEWED)
    expect(stepUps).toBe(1)
    expect(refreshes).toBe(1)
    await user.type(code(), GOOD_CODE)
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByText('ok 1')).toBeInTheDocument()
    expect(stepUps).toBe(2)
  })
})

describe('F4: the query client does not retry step-up / session outcomes', () => {
  it.each(['step_up_required', 'step_up_cancelled', 'step_up_failed', 'session_revoked', 'mfa_locked'] as const)('%s', (c) => {
    const retry = queryClient.getDefaultOptions().queries!.retry as (n: number, e: unknown) => boolean
    expect(retry(0, new ApiError(c, 'x', 403))).toBe(false)
    expect(retry(0, new ApiError('internal_error', 'x', 500))).toBe(true)
  })
})

describe('F5: a cleared session forgets the stepped-up token', () => {
  it('after sign-out and a new session carrying the old stepped-up token, a refusal prompts again', async () => {
    signInAs({})
    const f = installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Run' }))
    await dialog()
    await user.type(code(), GOOD_CODE)
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    await screen.findByText('ok 1')
    // Sign out and back in on the very same token value that the step-up minted.
    act(() => clearSession())
    act(() => setSession(STEPPED_JWT, 'r'))
    // A request "sent before" with a different token and refused now must prompt, not be waved through.
    server.use(http.post('/v1/admin/thing', () => stepUpRequiredResponse()))
    await user.click(screen.getByRole('button', { name: 'Run' }))
    await dialog()
    expect(f.stepUps).toHaveLength(1)
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
  })
})

describe('F6: replaceAccessToken pins tenant and session id', () => {
  it('rejects a different tenant_id', () => {
    setSession(makeTestJwt({ tenant_id: '11111111-1111-1111-1111-111111111111' }), 'r')
    expect(() => replaceAccessToken(makeTestJwt({ tenant_id: '22222222-2222-2222-2222-222222222222' }))).toThrow(InsufficientAssuranceError)
    expect(getAccessToken()).toBeNull()
  })
  it('rejects a different sid, and a sid appearing or disappearing', () => {
    setSession(makeTestJwt({ sid: 's1' }), 'r')
    expect(() => replaceAccessToken(makeTestJwt({ sid: 's2' }))).toThrow(InsufficientAssuranceError)
    setSession(makeTestJwt({ sid: 's1' }), 'r')
    expect(() => replaceAccessToken(makeTestJwt({}))).toThrow(InsufficientAssuranceError)
    setSession(makeTestJwt({}), 'r')
    expect(() => replaceAccessToken(makeTestJwt({ sid: 's1' }))).toThrow(InsufficientAssuranceError)
  })
  it('accepts the same sid', () => {
    setSession(makeTestJwt({ sid: 's1', jti: 'a' }), 'r')
    const next = makeTestJwt({ sid: 's1', jti: 'b' })
    replaceAccessToken(next)
    expect(getAccessToken()).toBe(next)
  })
})

describe('F7: the rest of the app is inert while the modal is open', () => {
  it('marks the app inert and aria-hidden, and restores it (and focus) on close', async () => {
    signInAs({})
    installStepUpFixture()
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    const trigger = screen.getByRole('button', { name: 'Run' })
    trigger.focus()
    await user.click(trigger)
    await dialog()
    const host = trigger.closest('[inert]')
    expect(host).not.toBeNull()
    expect(host).toHaveAttribute('aria-hidden', 'true')
    expect(screen.getByRole('dialog').closest('[inert]')).toBeNull()
    await user.keyboard('{Escape}')
    await screen.findByText(/^err 1 step_up_cancelled:/)
    expect(document.querySelector('[inert]')).toBeNull()
    expect(document.querySelector('[aria-hidden="true"]')).toBeNull()
    expect(trigger).toHaveFocus()
  })
})
