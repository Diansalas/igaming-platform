import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { useEffect } from 'react'
import { beforeEach, describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { MARKET_NOT_AVAILABLE_MESSAGE } from '../../lib/marketRefusal'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { BetSlip } from './BetSlip'
import { useBetSlip, type SlipSelection } from './BetSlipContext'

// A8 / SR8-1: bet placement is market-gated. The 403 MARKET_NOT_AVAILABLE shows the UI-owned sentence
// (never the server text) and no "bet placed" state.
const SELECTION: SlipSelection = {
  selectionId: 'sel-1', selectionName: 'Home FC', marketName: 'Match Winner', eventId: 'event-1',
  eventName: 'Home FC vs Away FC', oddsNumerator: 3, oddsDenominator: 2, idempotencyKey: 'market-key-1',
}

function Harness() {
  const { setSelection } = useBetSlip()
  useEffect(() => {
    setSelection(SELECTION)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  return <BetSlip />
}

describe('BetSlip market refusal', () => {
  beforeEach(() => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
  })

  it('shows the generic regional sentence on a 403 MARKET_NOT_AVAILABLE and never a placed bet', async () => {
    server.use(
      http.post('/v1/me/sportsbook/bets', () =>
        HttpResponse.json({ code: 'MARKET_NOT_AVAILABLE', message: 'server text that must not be shown', request_id: 'r1' }, { status: 403 }),
      ),
    )
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    const side = within(screen.getByRole('complementary'))

    await user.type(side.getByLabelText(/Stake/), '10')
    await user.click(side.getByRole('button', { name: 'Place bet' }))

    expect(await side.findByText(MARKET_NOT_AVAILABLE_MESSAGE)).toBeInTheDocument()
    expect(side.queryByText('Bet placed.')).not.toBeInTheDocument()
    expect(screen.queryByText(/server text that must not be shown/)).not.toBeInTheDocument()
  })
})
