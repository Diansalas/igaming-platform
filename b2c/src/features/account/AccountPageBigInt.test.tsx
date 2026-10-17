import { screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { AccountPage } from './AccountPage'

// U-5: the wallet DTO declares `integer int64` (OpenAPI WalletSummary), so a balance above 2^53 reaches the browser as a
// raw JSON number literal. The wire body below is sent as TEXT (never via JSON.stringify of a JS number) to prove the
// whole path - apiFetch -> parseJsonExact -> formatMoney - shows the exact amount instead of a rounded float.
describe('AccountPage wallet display above 2^53', () => {
  it('renders int64 balances exactly (no float rounding)', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    server.use(
      http.get('/v1/me/wallets', () =>
        new HttpResponse(
          '[{"wallet_id":"w1","asset_code":"EUR","status":"active","cash_balance":9007199254740993,"available_balance":9223372036854775807,"held_for_withdrawal":0,"locked_balance":1,"bonus_balance":2}]',
          { headers: { 'Content-Type': 'application/json' } },
        ),
      ),
    )
    renderWithProviders(<AccountPage />)
    expect(await screen.findByText('90071992547409.93 EUR')).toBeInTheDocument()
    expect(screen.getByText('92233720368547758.07 EUR')).toBeInTheDocument()
  })
})
