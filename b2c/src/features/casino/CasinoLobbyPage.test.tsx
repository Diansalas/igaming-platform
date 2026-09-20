import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { CasinoLobbyPage } from './CasinoLobbyPage'

function SessionStub() {
  return <div>Session screen stub</div>
}

function renderLobby() {
  return renderWithProviders(
    <Routes>
      <Route path="/casino" element={<CasinoLobbyPage />} />
      <Route path="/casino/sessions/:sessionId" element={<SessionStub />} />
    </Routes>,
    { route: '/casino' },
  )
}

describe('CasinoLobbyPage', () => {
  beforeEach(() => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
  })

  it('renders the games returned by the catalogue endpoint', async () => {
    renderLobby()

    expect(await screen.findByText('Mock Slots Deluxe')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Play' })).toBeInTheDocument()
  })

  it('shows an empty state when the catalogue has no games', async () => {
    server.use(http.get('/v1/me/casino/games', () => HttpResponse.json([])))
    renderLobby()

    expect(await screen.findByText('No games are available right now.')).toBeInTheDocument()
  })

  it('launching a game navigates to the session screen', async () => {
    renderLobby()
    const user = userEvent.setup()

    await screen.findByText('Mock Slots Deluxe')
    await user.click(screen.getByRole('button', { name: 'Play' }))

    expect(await screen.findByText('Session screen stub')).toBeInTheDocument()
  })

  it('renders a launch denial inline on the game card, without navigating away', async () => {
    server.use(
      http.post('/v1/me/casino/games/:gameId/launch', () =>
        HttpResponse.json({ code: 'forbidden', message: 'gambling is currently restricted for this account: self-excluded' }, { status: 403 }),
      ),
    )
    renderLobby()
    const user = userEvent.setup()

    await screen.findByText('Mock Slots Deluxe')
    await user.click(screen.getByRole('button', { name: 'Play' }))

    expect(await screen.findByText(/self-excluded/)).toBeInTheDocument()
    expect(screen.queryByText('Session screen stub')).not.toBeInTheDocument()
  })
})
