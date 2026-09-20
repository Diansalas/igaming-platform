import { screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { AccountPage } from './AccountPage'

/**
 * Proves the wallet display renders EXACTLY the amounts the server
 * returns, with no client-side recalculation. The fixture below is
 * deliberately non-additive (cash/available/locked/bonus do not sum to
 * anything meaningful) - if any future change made this component derive
 * one balance from another instead of rendering each field verbatim,
 * this test would catch it.
 */
describe('AccountPage wallet display', () => {
  it('renders every balance field exactly as returned by the API', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    server.use(
      http.get('/v1/me/wallets', () =>
        HttpResponse.json([
          {
            wallet_id: 'wallet-9',
            asset_code: 'EUR',
            status: 'active',
            cash_balance: 100000,
            available_balance: 42,
            held_for_withdrawal: 999,
            locked_balance: 1234,
            bonus_balance: 56,
          },
        ]),
      ),
    )

    renderWithProviders(<AccountPage />)

    expect(await screen.findByText('1000.00 EUR')).toBeInTheDocument() // cash_balance
    expect(screen.getByText('0.42 EUR')).toBeInTheDocument() // available_balance
    expect(screen.getByText('12.34 EUR')).toBeInTheDocument() // locked_balance
    expect(screen.getByText('0.56 EUR')).toBeInTheDocument() // bonus_balance
  })
})
