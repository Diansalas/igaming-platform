import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { useEffect } from 'react'
import { beforeEach, describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { BetSlip } from './BetSlip'
import { useBetSlip, type SlipSelection } from './BetSlipContext'

const SELECTION: SlipSelection = {
  selectionId: 'sel-1',
  selectionName: 'Home FC',
  marketName: 'Match Winner',
  eventId: 'event-1',
  eventName: 'Home FC vs Away FC',
  oddsNumerator: 3,
  oddsDenominator: 2,
}

/** Seeds the bet-slip context with a selection before rendering the real BetSlip component, standing in for "the player already picked an outcome on the event page." */
function Harness() {
  const { setSelection } = useBetSlip()
  useEffect(() => {
    setSelection(SELECTION)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  return <BetSlip />
}

// BetSlip renders BOTH a persistent desktop sidebar and a mobile bottom
// sheet (visually mutually exclusive only via a CSS media query, which
// this jsdom test environment never evaluates) so the same content exists
// twice in the DOM - a real, deliberate responsive-layout pattern, not a
// bug. Tests scope every query to the desktop `<aside>` (the one
// `role="complementary"` landmark on the page) to disambiguate, exactly
// as a screen-reader user landing on either would see the same content.
function sidebar() {
  return within(screen.getByRole('complementary'))
}

describe('BetSlip', () => {
  beforeEach(() => {
    // Every scenario here is the authenticated "Place bet" path.
    setSession(makeTestJwt(), 'refresh-token-for-test')
  })

  it('renders the server-confirmed bet on a 201 accepted response, never the client-side estimate', async () => {
    renderWithProviders(<Harness />)
    const user = userEvent.setup()

    await user.type(sidebar().getByLabelText(/Stake/), '10')
    await user.click(sidebar().getByRole('button', { name: 'Place bet' }))

    expect(await sidebar().findByText('Bet placed.')).toBeInTheDocument()
    expect(sidebar().getByText('bet-1')).toBeInTheDocument()
    // 2500 minor units of USD ("25.00 USD"), exactly as the mocked
    // server response's own potential_return field says - a fixed test
    // fixture, not a value this test recomputes from the selection's
    // odds. The assertion is on the rendered server field only.
    expect(sidebar().getByText('25.00 USD')).toBeInTheDocument()
  })

  it('renders the odds_changed rejection distinctly and offers a re-check affordance', async () => {
    server.use(
      http.post('/v1/me/sportsbook/bets', () =>
        HttpResponse.json({
          accepted: false,
          rejection_category: 'odds_changed',
          rejection_code: 'stale_price',
          rejection_message: 'The odds for this selection have changed.',
        }),
      ),
      http.get('/v1/sportsbook/events/:id', () =>
        HttpResponse.json({
          id: 'event-1',
          name: 'Home FC vs Away FC',
          start_time: '2026-01-01T18:00:00Z',
          status: 'open',
          sport_code: 'football',
          sport_name: 'Football',
          competition_name: 'Premier League',
          markets: [
            {
              id: 'market-1',
              name: 'Match Winner',
              status: 'open',
              selections: [{ id: 'sel-1', name: 'Home FC', odds_numerator: 2, odds_denominator: 1, status: 'open' }],
            },
          ],
        }),
      ),
    )

    renderWithProviders(<Harness />)
    const user = userEvent.setup()

    await user.type(sidebar().getByLabelText(/Stake/), '10')
    await user.click(sidebar().getByRole('button', { name: 'Place bet' }))

    expect(await sidebar().findByText('The odds changed')).toBeInTheDocument()
    // The stale-price re-check refetches the event and shows the new price
    // (odds_numerator=2/odds_denominator=1 -> decimal odds 2.00 - the
    // ratio itself, never +1) rather than leaving the old, now-wrong 1.50
    // (3/2) as if nothing happened.
    await waitFor(() => expect(sidebar().getByText(/New price: 2\.00/)).toBeInTheDocument())
  })

  it('renders the insufficient_funds rejection distinctly, with a deposit affordance', async () => {
    server.use(
      http.post('/v1/me/sportsbook/bets', () =>
        HttpResponse.json({
          accepted: false,
          rejection_category: 'insufficient_funds',
          rejection_code: 'insufficient_funds',
          rejection_message: 'Your balance does not cover this stake.',
        }),
      ),
    )

    renderWithProviders(<Harness />)
    const user = userEvent.setup()

    await user.type(sidebar().getByLabelText(/Stake/), '10000')
    await user.click(sidebar().getByRole('button', { name: 'Place bet' }))

    expect(await sidebar().findByText('Not enough funds')).toBeInTheDocument()
    expect(sidebar().getByRole('link', { name: 'Deposit funds' })).toBeInTheDocument()
  })

  it('renders a genuine server error distinctly from a rejection, and never assumes success', async () => {
    server.use(
      http.post('/v1/me/sportsbook/bets', () =>
        HttpResponse.json({ code: 'internal_error', message: 'unexpected failure' }, { status: 500 }),
      ),
    )

    renderWithProviders(<Harness />)
    const user = userEvent.setup()

    await user.type(sidebar().getByLabelText(/Stake/), '10')
    await user.click(sidebar().getByRole('button', { name: 'Place bet' }))

    expect(await sidebar().findByText('unexpected failure')).toBeInTheDocument()
    expect(screen.queryByText('Bet placed.')).not.toBeInTheDocument()
  })

  it('renders a network error distinctly, and never assumes success', async () => {
    server.use(http.post('/v1/me/sportsbook/bets', () => HttpResponse.error()))

    renderWithProviders(<Harness />)
    const user = userEvent.setup()

    await user.type(sidebar().getByLabelText(/Stake/), '10')
    await user.click(sidebar().getByRole('button', { name: 'Place bet' }))

    expect(await sidebar().findByText(/Could not reach the server/)).toBeInTheDocument()
    expect(screen.queryByText('Bet placed.')).not.toBeInTheDocument()
  })
})
