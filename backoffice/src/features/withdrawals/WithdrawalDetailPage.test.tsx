import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { server } from '../../test/mswServer'
import { makeTestJwt } from '../../test/jwt'
import { renderWithProviders } from '../../test/renderWithProviders'
import { WithdrawalDetailPage } from './WithdrawalDetailPage'

describe('WithdrawalDetailPage approve flow', () => {
  beforeEach(() => {
    // Simulate an already-authenticated finance staff session so the page's
    // own API calls carry a bearer token, matching how the app is actually
    // used (a signed-in operator navigating to a withdrawal).
    setSession(makeTestJwt({ role: 'finance' }), 'refresh-token-for-test')
  })

  it('shows a confirmation dialog, calls the approve endpoint on confirm, and shows success feedback', async () => {
    const approveSpy = vi.fn()
    server.use(
      http.post('/v1/admin/withdrawals/:id/approve', ({ params }) => {
        approveSpy(params.id)
        return HttpResponse.json({ approved: true })
      }),
    )

    renderWithProviders(
      <Routes>
        <Route path="/withdrawals/:id" element={<WithdrawalDetailPage />} />
      </Routes>,
      { route: '/withdrawals/wd-1' },
    )
    const user = userEvent.setup()

    await screen.findByText('Withdrawal wd-1')

    // The approve action must never fire directly from the list/detail
    // button - a confirmation dialog is required first.
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    const dialog = await screen.findByRole('dialog', { name: 'Approve withdrawal' })
    expect(dialog).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Approve withdrawal' }))

    await waitFor(() => expect(approveSpy).toHaveBeenCalledWith('wd-1'))
    expect(await screen.findByText('Withdrawal approved.')).toBeInTheDocument()
    // The dialog closes after a successful decision.
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('shows the server error and keeps the dialog open when the API rejects the approval', async () => {
    server.use(
      http.post('/v1/admin/withdrawals/:id/approve', () =>
        HttpResponse.json({ code: 'forbidden', message: 'cannot approve your own withdrawal' }, { status: 403 }),
      ),
    )

    renderWithProviders(
      <Routes>
        <Route path="/withdrawals/:id" element={<WithdrawalDetailPage />} />
      </Routes>,
      { route: '/withdrawals/wd-1' },
    )
    const user = userEvent.setup()

    await screen.findByText('Withdrawal wd-1')
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    await screen.findByRole('dialog', { name: 'Approve withdrawal' })
    await user.click(screen.getByRole('button', { name: 'Approve withdrawal' }))

    expect(await screen.findByText('cannot approve your own withdrawal')).toBeInTheDocument()
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('calls the approve endpoint exactly once when the confirm button is triggered twice in immediate succession', async () => {
    const approveSpy = vi.fn()
    server.use(
      http.post('/v1/admin/withdrawals/:id/approve', async ({ params }) => {
        approveSpy(params.id)
        // A small delay so a naive re-render-driven disable (rather than
        // the synchronous useOnceGuard ref) would have a real window to
        // fail in - see useOnceGuard's own doc comment.
        await new Promise((resolve) => setTimeout(resolve, 20))
        return HttpResponse.json({ approved: true })
      }),
    )

    renderWithProviders(
      <Routes>
        <Route path="/withdrawals/:id" element={<WithdrawalDetailPage />} />
      </Routes>,
      { route: '/withdrawals/wd-1' },
    )
    const user = userEvent.setup()

    await screen.findByText('Withdrawal wd-1')
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    await screen.findByRole('dialog', { name: 'Approve withdrawal' })
    const confirmButton = screen.getByRole('button', { name: 'Approve withdrawal' })

    // Two synchronous fireEvent.click calls in the same tick, deliberately
    // not using userEvent's own await-between-events helper, to reproduce
    // the exact double-dispatch race useOnceGuard exists to close.
    fireEvent.click(confirmButton)
    fireEvent.click(confirmButton)

    await waitFor(() => expect(approveSpy).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(approveSpy).toHaveBeenCalledTimes(1)
  })
})

function mockWithdrawalState(state: string) {
  server.use(
    http.get('/v1/admin/withdrawals/:id', ({ params }) =>
      HttpResponse.json({
        id: params.id,
        asset_code: 'EUR',
        amount: 10000,
        state,
        requested_at: '2026-01-01T00:00:00Z',
        player_account_id: 'player-1',
        decimal_exponent: 2,
      }),
    ),
  )
}

function renderDetail(id = 'wd-1') {
  return renderWithProviders(
    <Routes>
      <Route path="/withdrawals/:id" element={<WithdrawalDetailPage />} />
    </Routes>,
    { route: `/withdrawals/${id}` },
  )
}

describe('WithdrawalDetailPage state-gated actions', () => {
  beforeEach(() => {
    const token = makeTestJwt({ role: 'finance', sub: 'finance-staff-1' })
    setSession(token, 'refresh-token-for-test')
    // AuthProvider's bootstrap refresh would otherwise swap in the default
    // handler's tenant_admin token.
    server.use(
      http.post('/v1/auth/refresh', () =>
        HttpResponse.json({ access_token: token, refresh_token: 'rt-2', token_type: 'Bearer', expires_in: 900 }),
      ),
    )
  })

  it('does NOT offer approve/reject for a `requested` withdrawal (backend 409s) and points to the pending queue', async () => {
    mockWithdrawalState('requested')
    renderDetail()
    await screen.findByText('Withdrawal wd-1')
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Reject' })).not.toBeInTheDocument()
    expect(screen.getByText(/has not entered review yet/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Pending review queue' })).toHaveAttribute('href', '/withdrawals?tab=pending')
  })

  it('offers approve/reject for pending_review and shows who is acting', async () => {
    renderDetail()
    await screen.findByText('Withdrawal wd-1')
    expect(screen.getByRole('button', { name: 'Approve' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Reject' })).toBeInTheDocument()
    expect(screen.getByTestId('acting-as')).toHaveTextContent('Acting as staff finance-staff-1 (finance)')
  })

  it('shows the second-approver message when approve returns {approved:false}', async () => {
    server.use(http.post('/v1/admin/withdrawals/:id/approve', () => HttpResponse.json({ approved: false })))
    renderDetail()
    const user = userEvent.setup()
    await screen.findByText('Withdrawal wd-1')
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    await user.click(await screen.findByRole('button', { name: 'Approve withdrawal' }))
    expect(await screen.findByText('Approval recorded — a second, different approver is still required.')).toBeInTheDocument()
  })

  it('shows the API message and request id when approve is refused', async () => {
    server.use(
      http.post('/v1/admin/withdrawals/:id/approve', () =>
        HttpResponse.json(
          { code: 'conflict', message: 'you have already recorded a decision for this withdrawal', request_id: 'req-42' },
          { status: 409 },
        ),
      ),
    )
    renderDetail()
    const user = userEvent.setup()
    await screen.findByText('Withdrawal wd-1')
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    await user.click(await screen.findByRole('button', { name: 'Approve withdrawal' }))
    expect(
      await screen.findByText('you have already recorded a decision for this withdrawal (Request ID: req-42)'),
    ).toBeInTheDocument()
  })

  it('reject sends {reason_code}', async () => {
    let body: unknown
    server.use(
      http.post('/v1/admin/withdrawals/:id/reject', async ({ request }) => {
        body = await request.json()
        return new HttpResponse(null, { status: 204 })
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await screen.findByText('Withdrawal wd-1')
    await user.click(screen.getByRole('button', { name: 'Reject' }))
    await user.type(await screen.findByLabelText('Reason code'), 'suspected_fraud')
    await user.click(screen.getByRole('button', { name: 'Reject withdrawal' }))
    expect(await screen.findByText('Withdrawal rejected.')).toBeInTheDocument()
    expect(body).toEqual({ reason_code: 'suspected_fraud' })
  })

  it('for an approved withdrawal offers only Submit, which POSTs {payment_method} (default "card") and shows the provider result', async () => {
    mockWithdrawalState('approved')
    const calls: { path: string; body: unknown }[] = []
    server.use(
      http.post('/v1/admin/withdrawals/:id/submit', async ({ request }) => {
        calls.push({ path: new URL(request.url).pathname, body: await request.json() })
        return HttpResponse.json({
          id: 'wd-1',
          asset_code: 'EUR',
          amount: 10000,
          state: 'completed',
          requested_at: '2026-01-01T00:00:00Z',
          provider_id: 'mock-payments',
          provider_reference: 'ref-123',
        })
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await screen.findByText('Withdrawal wd-1')
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Submit to provider' }))
    expect(await screen.findByLabelText('Payment method')).toHaveValue('card')
    await user.click(screen.getByRole('button', { name: 'Submit withdrawal' }))
    expect(
      await screen.findByText('Withdrawal submitted to provider mock-payments (reference ref-123). Current state: completed.'),
    ).toBeInTheDocument()
    expect(calls).toEqual([{ path: '/v1/admin/withdrawals/wd-1/submit', body: { payment_method: 'card' } }])
  })

  it('surfaces a 503 no-provider error on submit and keeps the dialog open', async () => {
    mockWithdrawalState('approved')
    server.use(
      http.post('/v1/admin/withdrawals/:id/submit', () =>
        HttpResponse.json(
          { code: 'service_unavailable', message: 'no payment provider available for this withdrawal', request_id: 'req-7' },
          { status: 503 },
        ),
      ),
    )
    renderDetail()
    const user = userEvent.setup()
    await screen.findByText('Withdrawal wd-1')
    await user.click(screen.getByRole('button', { name: 'Submit to provider' }))
    await user.click(await screen.findByRole('button', { name: 'Submit withdrawal' }))
    expect(await screen.findByText('no payment provider available for this withdrawal (Request ID: req-7)')).toBeInTheDocument()
    expect(screen.getByRole('dialog', { name: 'Submit withdrawal' })).toBeInTheDocument()
  })

  it('submit fires exactly once on a double click', async () => {
    mockWithdrawalState('approved')
    const spy = vi.fn()
    server.use(
      http.post('/v1/admin/withdrawals/:id/submit', async () => {
        spy()
        await new Promise((r) => setTimeout(r, 20))
        return HttpResponse.json({ id: 'wd-1', asset_code: 'EUR', amount: 10000, state: 'submitted', requested_at: 't' })
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await screen.findByText('Withdrawal wd-1')
    await user.click(screen.getByRole('button', { name: 'Submit to provider' }))
    const confirm = await screen.findByRole('button', { name: 'Submit withdrawal' })
    fireEvent.click(confirm)
    fireEvent.click(confirm)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('for a submitted withdrawal offers Resolve, which POSTs to /resolve with no body', async () => {
    mockWithdrawalState('submitted')
    const calls: { path: string; body: string }[] = []
    server.use(
      http.post('/v1/admin/withdrawals/:id/resolve', async ({ request }) => {
        calls.push({ path: new URL(request.url).pathname, body: await request.text() })
        return HttpResponse.json({
          id: 'wd-1',
          asset_code: 'EUR',
          amount: 10000,
          state: 'completed',
          requested_at: 't',
          provider_id: 'mock-payments',
          provider_reference: 'ref-9',
        })
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await screen.findByText('Withdrawal wd-1')
    await user.click(screen.getByRole('button', { name: 'Resolve' }))
    await user.click(await screen.findByRole('button', { name: 'Resolve withdrawal' }))
    expect(
      await screen.findByText('Withdrawal resolved to provider mock-payments (reference ref-9). Current state: completed.'),
    ).toBeInTheDocument()
    expect(calls).toEqual([{ path: '/v1/admin/withdrawals/wd-1/resolve', body: '' }])
  })
})
