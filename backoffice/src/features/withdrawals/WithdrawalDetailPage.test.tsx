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
