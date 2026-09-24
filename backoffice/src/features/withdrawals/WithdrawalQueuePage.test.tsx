import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { WithdrawalQueuePage } from './WithdrawalQueuePage'

function renderQueue(route = '/withdrawals') {
  return renderWithProviders(
    <Routes>
      <Route path="/withdrawals" element={<WithdrawalQueuePage />} />
      <Route path="/withdrawals/:id" element={<div>detail page</div>} />
    </Routes>,
    { route },
  )
}

describe('WithdrawalQueuePage', () => {
  beforeEach(() => {
    setSession(makeTestJwt({ role: 'finance' }), 'refresh-token-for-test')
  })

  it('opens on the Pending review queue, which calls GET /v1/admin/withdrawals (the promoting queue) and explains the side effect', async () => {
    const pendingSpy = vi.fn()
    const historySpy = vi.fn()
    server.use(
      http.get('/v1/admin/withdrawals', ({ request }) => {
        pendingSpy(new URL(request.url).pathname)
        return HttpResponse.json([
          { id: 'wd-p', asset_code: 'EUR', amount: 5000, state: 'pending_review', requested_at: 't', player_account_id: 'player-p' },
        ])
      }),
      http.get('/v1/admin/withdrawals/history', () => {
        historySpy()
        return HttpResponse.json({ items: [], limit: 20, offset: 0, total: 0 })
      }),
    )
    renderQueue()

    expect(await screen.findByText('wd-p')).toBeInTheDocument()
    expect(pendingSpy).toHaveBeenCalledWith('/v1/admin/withdrawals')
    expect(historySpy).not.toHaveBeenCalled()
    expect(screen.getByText(/Opening this queue moves every newly/)).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Pending review' })).toHaveAttribute('aria-selected', 'true')
  })

  it('History tab uses the read-only paged endpoint, and Submitted tab uses /submitted', async () => {
    renderQueue('/withdrawals?tab=history')
    const user = userEvent.setup()
    expect(await screen.findByText('player-1')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Submitted' }))
    expect(await screen.findByText('wd-submitted-1')).toBeInTheDocument()
  })

  it('navigates to the detail page when a pending row is clicked', async () => {
    renderQueue()
    const user = userEvent.setup()
    await user.click(await screen.findByText('wd-pending-1'))
    await waitFor(() => expect(screen.getByText('detail page')).toBeInTheDocument())
  })

  it('renders the API error with its request id when the pending queue fails', async () => {
    server.use(
      http.get('/v1/admin/withdrawals', () =>
        HttpResponse.json({ code: 'forbidden', message: 'insufficient permissions for this operation', request_id: 'req-9' }, { status: 403 }),
      ),
    )
    renderQueue()
    expect(await screen.findByText('insufficient permissions for this operation')).toBeInTheDocument()
    expect(screen.getByText('Request ID: req-9')).toBeInTheDocument()
  })
})
