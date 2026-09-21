import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { server } from '../../test/mswServer'
import { makeTestJwt } from '../../test/jwt'
import { renderWithProviders } from '../../test/renderWithProviders'
import { ChangeRequestQueuePage } from './ChangeRequestQueuePage'

const CHANGE_REQUEST = {
  id: 'cr-1',
  operation: 'grant_extension',
  target_type: 'grant',
  target_id: 'grant-1',
  state: 'pending' as const,
  requested_by_principal_id: 'someone-else',
  requested_at: '2026-01-01T00:00:00Z',
  amount_at_request: '5000',
  asset_code: 'USD',
}

function mockList(overrides: Partial<typeof CHANGE_REQUEST> = {}) {
  server.use(
    http.get('/v1/admin/bonus/change-requests', () =>
      HttpResponse.json({ items: [{ ...CHANGE_REQUEST, ...overrides }], limit: 20, offset: 0, total: 1 }),
    ),
  )
}

describe('ChangeRequestQueuePage', () => {
  beforeEach(() => {
    // makeTestJwt's default `sub` is 'staff-1' - see src/test/jwt.ts.
    setSession(makeTestJwt({ role: 'tenant_admin' }), 'refresh-token-for-test')
  })

  it('displays the decimal-string amount as raw minor units with an explicit qualifier, never a bare number', async () => {
    mockList()
    renderWithProviders(<ChangeRequestQueuePage />)

    expect(await screen.findByText('5000 USD (raw minor units, exponent unknown)')).toBeInTheDocument()
  })

  it('calls the decide endpoint exactly once even when the confirm button is triggered twice in immediate succession', async () => {
    mockList()
    const decideSpy = vi.fn()
    server.use(
      http.post('/v1/admin/bonus/change-requests/:id/decide', async ({ params }) => {
        decideSpy(params.id)
        // A small delay so a naive re-render-driven disable (rather than the
        // synchronous useOnceGuard ref) would have a real window to fail in.
        await new Promise((resolve) => setTimeout(resolve, 20))
        return HttpResponse.json({ ...CHANGE_REQUEST, state: 'applied' })
      }),
    )

    renderWithProviders(<ChangeRequestQueuePage />)
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Approve' }))
    const dialog = await screen.findByRole('dialog', { name: 'Approve change request' })
    const confirmButton = within(dialog).getByRole('button', { name: 'Approve' })
    // Two synchronous fireEvent.click calls in the same tick, deliberately
    // NOT using userEvent's own await-between-events helper - this is
    // exactly the race useOnceGuard's doc comment describes: both must be
    // dispatched before React commits the isPending-driven re-render.
    fireEvent.click(confirmButton)
    fireEvent.click(confirmButton)

    await waitFor(() => expect(decideSpy).toHaveBeenCalledTimes(1))
    // Wait for the delayed response to fully resolve (dialog closes on
    // success) before asserting no second call snuck in - keeps the
    // assertion inside act()'s tracked async boundary rather than a bare
    // untracked timer.
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(decideSpy).toHaveBeenCalledTimes(1)
  })

  it('warns when the signed-in staff member is the same principal who requested the change', async () => {
    mockList({ requested_by_principal_id: 'staff-1' })
    renderWithProviders(<ChangeRequestQueuePage />)
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Approve' }))

    expect(await screen.findByText(/You submitted this change request/)).toBeInTheDocument()
  })

  it('does not show the self-approval warning for a request submitted by a different principal', async () => {
    mockList({ requested_by_principal_id: 'someone-else' })
    renderWithProviders(<ChangeRequestQueuePage />)
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Approve' }))
    await screen.findByRole('dialog', { name: 'Approve change request' })

    expect(screen.queryByText(/You submitted this change request/)).not.toBeInTheDocument()
  })
})
