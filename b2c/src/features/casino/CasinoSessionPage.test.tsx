import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { CasinoSessionPage } from './CasinoSessionPage'

function renderSession() {
  return renderWithProviders(
    <Routes>
      <Route path="/casino/sessions/:sessionId" element={<CasinoSessionPage />} />
    </Routes>,
    { route: '/casino/sessions/session-1' },
  )
}

function resultPanel(title: string) {
  return within(screen.getByText(title).closest('[role="status"]') as HTMLElement)
}

describe('CasinoSessionPage', () => {
  beforeEach(() => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
  })

  it('renders the current wallet balance', async () => {
    renderSession()
    expect(await screen.findByText('95.00 USD')).toBeInTheDocument()
  })

  it('places a wager and renders a succeeded outcome distinctly', async () => {
    renderSession()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText(/Stake/), '10')
    await user.click(screen.getByRole('button', { name: 'Place wager' }))

    expect(await screen.findByText('Wager result')).toBeInTheDocument()
    expect(resultPanel('Wager result').getByText('succeeded')).toBeInTheDocument()
    expect(resultPanel('Wager result').getByText('ptx-wager-1')).toBeInTheDocument()
  })

  it('renders a declined wager outcome distinctly, without treating it as an error', async () => {
    server.use(
      http.post('/v1/me/casino/sessions/:sessionId/wager', () =>
        HttpResponse.json({
          outcome: 'declined',
          provider_tx_id: 'ptx-declined-1',
          tombstoned: false,
          decline_reason: 'insufficient funds',
        }),
      ),
    )
    renderSession()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText(/Stake/), '10')
    await user.click(screen.getByRole('button', { name: 'Place wager' }))

    expect(await screen.findByText('Wager result')).toBeInTheDocument()
    expect(resultPanel('Wager result').getByText('declined')).toBeInTheDocument()
    expect(resultPanel('Wager result').getByText('insufficient funds')).toBeInTheDocument()
  })

  it('renders a genuine server error distinctly from a decline', async () => {
    server.use(
      http.post('/v1/me/casino/sessions/:sessionId/wager', () =>
        HttpResponse.json({ code: 'service_unavailable', message: 'casino is temporarily disabled' }, { status: 503 }),
      ),
    )
    renderSession()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText(/Stake/), '10')
    await user.click(screen.getByRole('button', { name: 'Place wager' }))

    expect(await screen.findByText('casino is temporarily disabled')).toBeInTheDocument()
    expect(screen.queryByText('Wager result')).not.toBeInTheDocument()
  })

  it('settles a win and renders its own succeeded outcome', async () => {
    renderSession()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText(/Win amount/), '20')
    await user.click(screen.getByRole('button', { name: 'Settle win' }))

    expect(await screen.findByText('Win result')).toBeInTheDocument()
    expect(resultPanel('Win result').getByText('succeeded')).toBeInTheDocument()
    expect(resultPanel('Win result').getByText('ptx-win-1')).toBeInTheDocument()
  })

  it('auto-fills the rollback field from the last successful wager and rolls it back', async () => {
    renderSession()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText(/Stake/), '10')
    await user.click(screen.getByRole('button', { name: 'Place wager' }))
    await screen.findByText('Wager result')

    expect(screen.getByLabelText(/Original provider transaction ID/)).toHaveValue('ptx-wager-1')

    await user.click(screen.getByRole('button', { name: 'Roll back' }))

    expect(await screen.findByText('Rollback result')).toBeInTheDocument()
    expect(resultPanel('Rollback result').getByText('succeeded')).toBeInTheDocument()
  })
})
