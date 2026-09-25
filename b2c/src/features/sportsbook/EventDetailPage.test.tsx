import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { BetSlip } from '../betslip/BetSlip'
import { EventDetailPage } from './EventDetailPage'

/**
 * Shapes mirror GET /v1/sportsbook/events/{id} as the real backend returns
 * it: a market is `open`, and its selections are `active` (or `suspended`)
 * - internal/sportsbook/types.go MarketStatus / SelectionStatus. A
 * selection is never `open`; the previous fixtures used that impossible
 * value, which is how a UI that only accepted it went unnoticed.
 */
function eventWith(market: { status: string }, selections: Array<{ id: string; name: string; status: string }>) {
  return {
    id: 'event-1',
    name: 'Los Angeles Lakers vs Boston Celtics',
    start_time: '2026-09-26T15:19:42Z',
    status: 'scheduled',
    sport_code: 'basketball',
    sport_name: 'Basketball',
    competition_name: 'NBA',
    markets: [
      {
        id: 'market-1',
        name: 'Match Winner',
        status: market.status,
        selections: selections.map((s) => ({ ...s, odds_numerator: 185, odds_denominator: 100, available: true })),
      },
    ],
  }
}

function renderEvent() {
  return renderWithProviders(
    <>
      <Routes>
        <Route path="/sports/events/:id" element={<EventDetailPage />} />
      </Routes>
      <BetSlip />
    </>,
    { route: '/sports/events/event-1' },
  )
}

describe('EventDetailPage selection pickability', () => {
  it('an active selection in an open market is enabled and clicking it adds it to the bet slip', async () => {
    server.use(
      http.get('/v1/sportsbook/events/:id', () =>
        HttpResponse.json(eventWith({ status: 'open' }, [{ id: 'sel-home', name: 'Home', status: 'active' }])),
      ),
    )
    const user = userEvent.setup()
    renderEvent()

    const button = await screen.findByRole('button', { name: /Home/ })
    expect(button).toBeEnabled()
    await user.click(button)

    const slip = within(screen.getByRole('complementary'))
    expect(await slip.findByText('Home')).toBeInTheDocument()
    expect(slip.getByText(/Los Angeles Lakers vs Boston Celtics/)).toBeInTheDocument()
  })

  it.each([
    ['a suspended selection', { status: 'open' }, 'suspended'],
    ['a selection with the impossible legacy status "open"', { status: 'open' }, 'open'],
    ['an unknown selection status', { status: 'open' }, 'withdrawn'],
    ['an active selection in a suspended market', { status: 'suspended' }, 'active'],
    ['an active selection in a closed market', { status: 'closed' }, 'active'],
  ])('%s stays unpickable', async (_label, market, selectionStatus) => {
    server.use(
      http.get('/v1/sportsbook/events/:id', () =>
        HttpResponse.json(eventWith(market, [{ id: 'sel-x', name: 'Away', status: selectionStatus }])),
      ),
    )
    const user = userEvent.setup()
    renderEvent()

    const button = await screen.findByRole('button', { name: /Away/ })
    expect(button).toBeDisabled()
    await user.click(button)
    // Nothing was added: the bet slip renders no sidebar until a selection is set.
    expect(screen.queryByRole('complementary')).not.toBeInTheDocument()
  })

  it('only the active selection is pickable when a market mixes active and suspended selections', async () => {
    server.use(
      http.get('/v1/sportsbook/events/:id', () =>
        HttpResponse.json(
          eventWith({ status: 'open' }, [
            { id: 'sel-home', name: 'Home', status: 'active' },
            { id: 'sel-away', name: 'Away', status: 'suspended' },
          ]),
        ),
      ),
    )
    renderEvent()
    expect(await screen.findByRole('button', { name: /Home/ })).toBeEnabled()
    expect(screen.getByRole('button', { name: /Away/ })).toBeDisabled()
  })
})
