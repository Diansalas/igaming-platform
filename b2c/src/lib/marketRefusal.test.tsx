import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import { ApiError } from '../api/types'
import { RegisterPage } from '../auth/RegisterPage'
import { clearSession, setSession } from '../auth/tokenStore'
import { CasinoLobbyPage } from '../features/casino/CasinoLobbyPage'
import { DepositPage } from '../features/deposit/DepositPage'
import { makeTestJwt } from '../test/jwt'
import { server } from '../test/mswServer'
import { renderWithProviders } from '../test/renderWithProviders'
import { MARKET_NOT_AVAILABLE_MESSAGE, isMarketRefusal, marketRefusalOr } from './marketRefusal'

// A8: the operating-market gate answers registration, deposit initiation and game launch with ONE
// 403 MARKET_NOT_AVAILABLE. The UI shows its OWN fixed sentence on all three screens and never the
// server's text (which could one day vary), a country, a reason or a retry hint.

const REFUSAL_BODY = { code: 'MARKET_NOT_AVAILABLE', message: 'this service is not available in your region', request_id: 'r-1' }
const refusal = () => HttpResponse.json(REFUSAL_BODY, { status: 403 })

describe('marketRefusal helpers', () => {
  it('recognises only the MARKET_NOT_AVAILABLE ApiError', () => {
    expect(isMarketRefusal(new ApiError('MARKET_NOT_AVAILABLE', 'x', 403))).toBe(true)
    expect(isMarketRefusal(new ApiError('forbidden', 'x', 403))).toBe(false)
    expect(isMarketRefusal(new Error('MARKET_NOT_AVAILABLE'))).toBe(false)
    expect(isMarketRefusal(undefined)).toBe(false)
  })

  it('uses the UI-owned sentence, not the server message', () => {
    const err = new ApiError('MARKET_NOT_AVAILABLE', 'a different server sentence that mentions DE and a rung', 403)
    expect(marketRefusalOr(err, () => 'fallback')).toBe(MARKET_NOT_AVAILABLE_MESSAGE)
    expect(marketRefusalOr(new ApiError('conflict', 'c', 409), () => 'fallback')).toBe('fallback')
  })

  it('names no country, reason or workaround', () => {
    expect(MARKET_NOT_AVAILABLE_MESSAGE).toBe('This service is not available in your region.')
    expect(MARKET_NOT_AVAILABLE_MESSAGE).not.toMatch(/location|country|vpn|licen|config|unknown/i)
  })
})

describe('market refusal on the three gated screens', () => {
  beforeEach(() => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
  })

  it('registration shows the generic refusal and stays on the form (not the "site unavailable" screen)', async () => {
    clearSession()
    server.use(http.post('/v1/auth/register', refusal))
    renderWithProviders(<RegisterPage />, { route: '/register' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Email'), 'blocked@example.com')
    await user.type(screen.getByLabelText('Password'), 'a-strong-password')
    await user.click(screen.getByRole('button', { name: 'Create account' }))

    expect(await screen.findByRole('alert')).toHaveTextContent(MARKET_NOT_AVAILABLE_MESSAGE)
    expect(screen.getByRole('button', { name: 'Create account' })).toBeInTheDocument()
    expect(screen.queryByText('This site is not available at the moment')).not.toBeInTheDocument()
  })

  it('deposit shows the generic refusal and creates no deposit entry', async () => {
    server.use(
      http.get('/v1/me/deposits', () => HttpResponse.json([])),
      http.post('/v1/me/deposits', refusal),
    )
    const user = userEvent.setup()
    renderWithProviders(<DepositPage />)

    await user.type(screen.getByPlaceholderText('50.00'), '50')
    await user.click(screen.getByRole('button', { name: 'Start deposit' }))

    expect(await screen.findByText(MARKET_NOT_AVAILABLE_MESSAGE)).toBeInTheDocument()
    expect(screen.queryByText(/Deposit dep-/)).not.toBeInTheDocument()
  })

  it('game launch shows the generic refusal on the card and does not navigate', async () => {
    server.use(http.post('/v1/me/casino/games/:gameId/launch', refusal))
    renderWithProviders(
      <Routes>
        <Route path="/casino" element={<CasinoLobbyPage />} />
        <Route path="/casino/sessions/:sessionId" element={<p>Session screen stub</p>} />
      </Routes>,
      { route: '/casino' },
    )
    const user = userEvent.setup()

    await screen.findByText('Mock Slots Deluxe')
    await user.click(screen.getByRole('button', { name: 'Play' }))

    await waitFor(() => expect(screen.getByText(MARKET_NOT_AVAILABLE_MESSAGE)).toBeInTheDocument())
    expect(screen.queryByText('Session screen stub')).not.toBeInTheDocument()
  })
})
