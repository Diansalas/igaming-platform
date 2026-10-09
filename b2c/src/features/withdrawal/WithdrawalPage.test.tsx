import { screen, waitFor } from '@testing-library/react'
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

const FUTURE = '2099-01-01T00:00:00Z'
const INSTRUMENT_ID = '11111111-1111-4111-8111-111111111111'
const VERIFIED_INSTRUMENT = {
  id: INSTRUMENT_ID,
  kind: 'bank_account',
  rail: 'bank_transfer',
  asset_codes: ['USD'],
  display_mask: '****4242',
  state: 'verified',
  state_changed_at: '2026-09-01T10:00:00Z',
  created_at: '2026-09-01T09:00:00Z',
  verification_source: 'synthetic',
  verified_at: '2026-09-01T10:00:00Z',
  verification_expires_at: FUTURE,
}

/** A tiny server: requesting moves money to held server-side; the page only re-reads it. */
function withdrawalServer() {
  const state = {
    wallet: { ...WALLET },
    withdrawals: [] as Array<Record<string, unknown>>,
    requests: [] as Array<Record<string, unknown>>,
    cancelled: [] as string[],
    instruments: [VERIFIED_INSTRUMENT] as Array<Record<string, unknown>>,
    instrumentListCalls: 0,
    /** When set, POST /v1/me/withdrawals answers with this instead of creating anything. */
    reject: null as null | { status: number; body: Record<string, unknown> },
  }
  server.use(
    http.get('/v1/me/payout-instruments', () => {
      state.instrumentListCalls++
      return HttpResponse.json(state.instruments)
    }),
    http.get('/v1/me/wallets', () => HttpResponse.json([state.wallet])),
    http.get('/v1/me/withdrawals', () => HttpResponse.json(state.withdrawals)),
    http.post('/v1/me/withdrawals', async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>
      state.requests.push(body)
      if (state.reject) return HttpResponse.json(state.reject.body, { status: state.reject.status })
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

  describe('payout instrument (B13)', () => {
    const NOT_USABLE = {
      code: 'PAYOUT_INSTRUMENT_NOT_USABLE',
      message: 'the payout instrument cannot be used for this withdrawal',
    }

    async function fillAndSubmit(user: ReturnType<typeof userEvent.setup>, amount = '25') {
      await screen.findByText('100.00 USD')
      await user.type(screen.getByPlaceholderText('25.00'), amount)
      await user.click(screen.getByRole('button', { name: 'Request withdrawal' }))
    }

    it('lists only masked, eligible instruments for the chosen asset', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.instruments = [
        VERIFIED_INSTRUMENT,
        { ...VERIFIED_INSTRUMENT, id: '22222222-2222-4222-8222-222222222222', display_mask: '****0001', state: 'pending_verification' },
        { ...VERIFIED_INSTRUMENT, id: '33333333-3333-4333-8333-333333333333', display_mask: '****0002', state: 'revoked' },
        { ...VERIFIED_INSTRUMENT, id: '44444444-4444-4444-8444-444444444444', display_mask: '****0003', state: 'suspended' },
        { ...VERIFIED_INSTRUMENT, id: '55555555-5555-4555-8555-555555555555', display_mask: '****0004', asset_codes: ['EUR'] },
        { ...VERIFIED_INSTRUMENT, id: '66666666-6666-4666-8666-666666666666', display_mask: '****0005', verification_expires_at: '2020-01-01T00:00:00Z' },
      ]
      renderWithProviders(<WithdrawalPage />)
      const select = await screen.findByRole('combobox', { name: 'Payout destination' })
      expect(select).toHaveTextContent('****4242')
      for (const hidden of ['****0001', '****0002', '****0003', '****0004', '****0005']) {
        expect(select).not.toHaveTextContent(hidden)
      }
    })

    it('sends the selected payout_instrument_id with the withdrawal', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      const second = { ...VERIFIED_INSTRUMENT, id: '22222222-2222-4222-8222-222222222222', display_mask: '****9999' }
      state.instruments = [VERIFIED_INSTRUMENT, second]
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await user.selectOptions(await screen.findByRole('combobox', { name: 'Payout destination' }), second.id)
      await user.type(await screen.findByPlaceholderText('25.00'), '25')
      await user.click(screen.getByRole('button', { name: 'Request withdrawal' }))
      expect(await screen.findByText(/Withdrawal requested/)).toBeInTheDocument()
      expect(state.requests[0]).toMatchObject({ asset_code: 'USD', amount: 2500, payout_instrument_id: second.id })
    })

    it('requires an explicit choice when several destinations are eligible and does not submit without one', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.instruments = [VERIFIED_INSTRUMENT, { ...VERIFIED_INSTRUMENT, id: '22222222-2222-4222-8222-222222222222', display_mask: '****9999' }]
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await fillAndSubmit(user)
      expect(await screen.findByRole('alert')).toHaveTextContent('Choose a verified payout destination')
      expect(state.requests).toHaveLength(0)
    })

    it('with no eligible instrument shows the required state, disables the button and never submits', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.instruments = [{ ...VERIFIED_INSTRUMENT, state: 'pending_verification' }]
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      expect(await screen.findByText('Payout destination required')).toBeInTheDocument()
      expect(screen.queryByRole('combobox', { name: 'Payout destination' })).not.toBeInTheDocument()
      const button = screen.getByRole('button', { name: 'Request withdrawal' })
      expect(button).toBeDisabled()
      await user.type(screen.getByPlaceholderText('25.00'), '25')
      await user.click(button)
      expect(state.requests).toHaveLength(0)
    })

    it('treats an empty instrument list the same way', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.instruments = []
      renderWithProviders(<WithdrawalPage />)
      expect(await screen.findByText('Payout destination required')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Request withdrawal' })).toBeDisabled()
      expect(state.requests).toHaveLength(0)
    })

    it('does not allow submission while the instrument list failed to load (503) and offers retry', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      server.use(
        http.get('/v1/me/payout-instruments', () =>
          HttpResponse.json({ code: 'service_unavailable', message: 'payout instruments are not available' }, { status: 503 }),
        ),
      )
      renderWithProviders(<WithdrawalPage />)
      expect(await screen.findByText('payout instruments are not available')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Request withdrawal' })).toBeDisabled()
      expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument()
      expect(state.requests).toHaveLength(0)
    })

    it('shows the generic backend rejection for a stale instrument and refreshes the list', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.reject = { status: 409, body: NOT_USABLE }
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await screen.findByRole('combobox', { name: 'Payout destination' })
      const before = state.instrumentListCalls
      // The backend revokes it after the page loaded; the page is now stale.
      state.instruments = [{ ...VERIFIED_INSTRUMENT, state: 'revoked' }]
      await fillAndSubmit(user)
      const alert = await screen.findByRole('alert')
      expect(alert).toHaveTextContent('cannot be used for this withdrawal')
      expect(screen.queryByText(/Withdrawal requested/)).not.toBeInTheDocument()
      expect(screen.getByText('No withdrawals yet.')).toBeInTheDocument()
      // The refreshed list no longer offers it.
      await screen.findByText('Payout destination required')
      expect(state.instrumentListCalls).toBeGreaterThan(before)
    })

    it('uses the same generic wording for another player\'s, inactive or unverified instrument (backend does not distinguish)', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.reject = { status: 409, body: NOT_USABLE }
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await fillAndSubmit(user)
      const alert = await screen.findByRole('alert')
      expect(alert).toHaveTextContent('This payout destination cannot be used for this withdrawal')
      expect(alert.textContent ?? '').not.toMatch(/foreign|owner|belong|someone else|other player/i)
    })

    it('shows the backend error and no success when a forged id slips past the UI', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      // The list (UI) offers an id the backend does not accept for this player.
      state.instruments = [{ ...VERIFIED_INSTRUMENT, id: '99999999-9999-4999-8999-999999999999' }]
      state.reject = { status: 409, body: NOT_USABLE }
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await fillAndSubmit(user)
      expect(await screen.findByRole('alert')).toHaveTextContent('cannot be used')
      expect(state.requests[0]).toMatchObject({ payout_instrument_id: '99999999-9999-4999-8999-999999999999' })
      expect(screen.queryByText(/Withdrawal requested/)).not.toBeInTheDocument()
      expect(screen.queryByText('Requested')).not.toBeInTheDocument()
    })

    it('maps PAYOUT_INSTRUMENT_REQUIRED to a choose-a-destination message', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.reject = { status: 409, body: { code: 'PAYOUT_INSTRUMENT_REQUIRED', message: 'a payout instrument is required' } }
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await fillAndSubmit(user)
      expect(await screen.findByRole('alert')).toHaveTextContent('Choose a verified payout destination')
    })

    it('maps a malformed-id 400 to a generic message, without success, and refreshes the list', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.reject = { status: 400, body: { code: 'validation_error', message: 'payout_instrument_id: must be a UUID' } }
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await screen.findByRole('combobox', { name: 'Payout destination' })
      const before = state.instrumentListCalls
      await fillAndSubmit(user)
      expect(await screen.findByRole('alert')).toHaveTextContent('selected payout destination is not valid')
      expect(screen.queryByText(/Withdrawal requested/)).not.toBeInTheDocument()
      await screen.findByRole('combobox', { name: 'Payout destination' })
      expect(state.instrumentListCalls).toBeGreaterThan(before)
    })

    it('shows a temporary-unavailable message on 503 (service not configured) and records nothing', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.reject = { status: 503, body: { code: 'service_unavailable', message: 'withdrawals are temporarily unavailable' } }
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await fillAndSubmit(user)
      expect(await screen.findByRole('alert')).toHaveTextContent('temporarily unavailable')
      expect(screen.queryByText(/Withdrawal requested/)).not.toBeInTheDocument()
    })

    it('keeps showing the non-active tenant/brand 409 as before', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.reject = { status: 409, body: { code: 'TENANT_OR_BRAND_NOT_ACTIVE', message: 'withdrawals are not available right now' } }
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await fillAndSubmit(user)
      expect(await screen.findByRole('alert')).toHaveTextContent('withdrawals are not available right now')
      expect(screen.queryByText(/Withdrawal requested/)).not.toBeInTheDocument()
    })

    it('reuses the Idempotency key and instrument on retry after an error, and a new key after success', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.reject = { status: 503, body: { code: 'service_unavailable', message: 'x' } }
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await fillAndSubmit(user)
      await screen.findByRole('alert')
      state.reject = null
      await user.click(screen.getByRole('button', { name: 'Request withdrawal' }))
      expect(await screen.findByText(/Withdrawal requested/)).toBeInTheDocument()
      expect(state.requests).toHaveLength(2)
      expect(state.requests[1].idempotency_key).toBe(state.requests[0].idempotency_key)
      expect(state.requests[1].payout_instrument_id).toBe(INSTRUMENT_ID)
      // A new attempt after the server answered gets a fresh key.
      await user.type(screen.getByPlaceholderText('25.00'), '5')
      await user.click(screen.getByRole('button', { name: 'Request withdrawal' }))
      await waitFor(() => expect(state.requests).toHaveLength(3))
      expect(state.requests[2].idempotency_key).not.toBe(state.requests[0].idempotency_key)
    })

    it('sends one request for a double click while the first is in flight', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      let release: () => void = () => {}
      const gate = new Promise<void>((r) => (release = r))
      server.use(
        http.post('/v1/me/withdrawals', async ({ request }) => {
          state.requests.push((await request.json()) as Record<string, unknown>)
          await gate
          return HttpResponse.json(
            { id: 'wd-1', asset_code: 'USD', amount: 2500, state: 'requested', requested_at: '2026-09-25T10:00:00Z' },
            { status: 201 },
          )
        }),
      )
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      await fillAndSubmit(user)
      await user.click(screen.getByRole('button', { name: 'Request withdrawal' }))
      release()
      await screen.findByText(/Withdrawal requested/)
      expect(state.requests).toHaveLength(1)
    })

    it('labels the destination control and announces errors with role=alert', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.reject = { status: 409, body: NOT_USABLE }
      const user = userEvent.setup()
      renderWithProviders(<WithdrawalPage />)
      expect(await screen.findByLabelText('Payout destination')).toBeInTheDocument()
      await fillAndSubmit(user)
      expect(await screen.findByRole('alert')).toBeInTheDocument()
    })

    it('never renders anything beyond the mask', async () => {
      setSession(makeTestJwt(), 'refresh-token-for-test')
      const state = withdrawalServer()
      state.instruments = [{ ...VERIFIED_INSTRUMENT, iban: 'DE89370400440532013000', fingerprint: 'abc123secret' }]
      const { container } = renderWithProviders(<WithdrawalPage />)
      await screen.findByRole('combobox', { name: 'Payout destination' })
      expect(container.textContent).not.toContain('DE89370400440532013000')
      expect(container.textContent).not.toContain('abc123secret')
    })
  })
})
