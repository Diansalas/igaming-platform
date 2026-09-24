import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { WithdrawalPage } from './WithdrawalPage'

const WALLET = {
  wallet_id: 'w-1',
  asset_code: 'USD',
  status: 'active',
  cash_balance: 10000,
  available_balance: 10000,
  held_for_withdrawal: 0,
  locked_balance: 0,
  bonus_balance: 0,
}

/** A tiny server: requesting moves money to held server-side; the page only re-reads it. */
function withdrawalServer() {
  const state = {
    wallet: { ...WALLET },
    withdrawals: [] as Array<Record<string, unknown>>,
    requests: [] as Array<Record<string, unknown>>,
    cancelled: [] as string[],
  }
  server.use(
    http.get('/v1/me/wallets', () => HttpResponse.json([state.wallet])),
    http.get('/v1/me/withdrawals', () => HttpResponse.json(state.withdrawals)),
    http.post('/v1/me/withdrawals', async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>
      state.requests.push(body)
      const amount = body.amount as number
      if (amount > state.wallet.available_balance) {
        return HttpResponse.json({ code: 'conflict', message: 'insufficient available balance' }, { status: 409 })
      }
      state.wallet = {
        ...state.wallet,
        available_balance: state.wallet.available_balance - amount,
        held_for_withdrawal: state.wallet.held_for_withdrawal + amount,
      }
      const wr = { id: 'wd-1', asset_code: 'USD', amount, state: 'requested', requested_at: '2026-09-25T10:00:00Z' }
      state.withdrawals = [wr]
      return HttpResponse.json(wr, { status: 201 })
    }),
    http.post('/v1/me/withdrawals/:id/cancel', ({ params }) => {
      state.cancelled.push(params.id as string)
      state.withdrawals = state.withdrawals.map((w) => (w.id === params.id ? { ...w, state: 'cancelled' } : w))
      return new HttpResponse(null, { status: 204 })
    }),
  )
  return state
}

describe('WithdrawalPage', () => {
  it('requests a withdrawal with an idempotency key and shows the server-held amount and state', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    const state = withdrawalServer()
    const user = userEvent.setup()

    renderWithProviders(<WithdrawalPage />)
    expect(await screen.findByText('100.00 USD')).toBeInTheDocument() // available
    await user.type(screen.getByPlaceholderText('25.00'), '25')
    await user.click(screen.getByRole('button', { name: 'Request withdrawal' }))

    expect(await screen.findByText(/Withdrawal requested/)).toBeInTheDocument()
    expect(state.requests).toHaveLength(1)
    expect(state.requests[0]).toMatchObject({ asset_code: 'USD', amount: 2500 })
    expect(typeof state.requests[0].idempotency_key).toBe('string')
    expect(await screen.findByText('75.00 USD')).toBeInTheDocument() // available, re-read from server
    expect(screen.getAllByText('25.00 USD').length).toBeGreaterThan(0) // held + list row
    expect(screen.getByText('Requested')).toBeInTheDocument()
  })

  it('shows insufficient balance as an actionable error and records nothing client-side', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    withdrawalServer()
    const user = userEvent.setup()
    renderWithProviders(<WithdrawalPage />)
    await screen.findByText('100.00 USD')
    await user.type(screen.getByPlaceholderText('25.00'), '500')
    await user.click(screen.getByRole('button', { name: 'Request withdrawal' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('does not cover this amount')
    expect(screen.getByText('No withdrawals yet.')).toBeInTheDocument()
  })

  it('offers cancel only while requested, and reflects the server state afterwards', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    const state = withdrawalServer()
    state.withdrawals = [
      { id: 'wd-1', asset_code: 'USD', amount: 1000, state: 'requested', requested_at: '2026-09-25T10:00:00Z' },
      { id: 'wd-2', asset_code: 'USD', amount: 2000, state: 'pending_review', requested_at: '2026-09-25T09:00:00Z' },
    ]
    const user = userEvent.setup()
    renderWithProviders(<WithdrawalPage />)

    expect(await screen.findByText('Awaiting review')).toBeInTheDocument()
    const cancelButtons = screen.getAllByRole('button', { name: 'Cancel' })
    expect(cancelButtons).toHaveLength(1)
    await user.click(cancelButtons[0])
    expect(await screen.findByText('Cancelled')).toBeInTheDocument()
    expect(state.cancelled).toEqual(['wd-1'])
  })

  it('tells a player with no wallet to deposit first', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    withdrawalServer()
    server.use(http.get('/v1/me/wallets', () => HttpResponse.json([])))
    renderWithProviders(<WithdrawalPage />)
    expect(await screen.findByText(/make a deposit before withdrawing/)).toBeInTheDocument()
  })
})
