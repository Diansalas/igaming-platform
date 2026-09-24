import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { DepositPage } from './DepositPage'

const PENDING = { id: 'dep-1', asset_code: 'USD', amount: 5000, payment_method: 'card', status: 'pending' }

/** Server-side state: the deposit list reflects whatever the (mock) server decided, nothing client-side. */
function depositServer() {
  const state = { deposits: [] as Array<Record<string, unknown>>, requests: [] as unknown[], settled: [] as string[] }
  server.use(
    http.get('/v1/me/deposits', () => HttpResponse.json(state.deposits)),
    http.get('/v1/me/deposits/:id', ({ params }) => HttpResponse.json(state.deposits.find((d) => d.id === params.id))),
    http.post('/v1/me/deposits', async ({ request }) => {
      state.requests.push(await request.json())
      state.deposits = [{ ...PENDING }]
      return HttpResponse.json({ ...PENDING }, { status: 201 })
    }),
    http.post('/v1/me/deposits/:id/simulate-callback', ({ params }) => {
      state.settled.push(params.id as string)
      state.deposits = state.deposits.map((d) => (d.id === params.id ? { ...d, status: 'succeeded' } : d))
      return HttpResponse.json({ deposit_intent_id: params.id, status: 'succeeded', tombstoned: false })
    }),
  )
  return state
}

describe('DepositPage', () => {
  it('initiates a real deposit, then completes it through the staging simulated provider confirmation', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    const state = depositServer()
    const user = userEvent.setup()

    renderWithProviders(<DepositPage />)
    await user.type(screen.getByPlaceholderText('50.00'), '50')
    await user.click(screen.getByRole('button', { name: 'Start deposit' }))

    expect(await screen.findByText('Deposit dep-1')).toBeInTheDocument()
    expect(state.requests).toHaveLength(1)
    expect(state.requests[0]).toMatchObject({ asset_code: 'USD', amount: 5000, payment_method: 'card' })

    await user.click(screen.getByRole('button', { name: 'Simulate provider confirmation (staging)' }))
    expect(await screen.findByText(/Deposit confirmed and credited/)).toBeInTheDocument()
    expect(state.settled).toEqual(['dep-1'])
  })

  it('reports when simulated confirmation is not available on this deployment (route absent -> plain 404)', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    const state = depositServer()
    state.deposits = [{ ...PENDING }]
    server.use(
      http.post('/v1/me/deposits/:id/simulate-callback', () => new HttpResponse('404 page not found', { status: 404 })),
    )
    const user = userEvent.setup()

    renderWithProviders(<DepositPage />)
    const list = await screen.findByText('Past deposits')
    await user.click(await screen.findByRole('button', { name: 'Simulate confirmation' }))
    expect(await within(list.closest('section') ?? document.body).findByRole('alert')).toHaveTextContent(
      'not available on this deployment',
    )
  })

  it('warns that deposits are declined while the account is pending verification', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    depositServer()
    server.use(
      http.get('/v1/me', () =>
        HttpResponse.json({ id: 'p', email: 'p@example.com', status: 'pending_verification', kyc_tier: 0, tenant_id: 't', brand_id: 'b' }),
      ),
    )
    renderWithProviders(<DepositPage />)
    expect(await screen.findByText(/Deposits are declined until your account is active/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Verify your email' })).toHaveAttribute('href', '/account')
  })

  it('explains a declined deposit without inventing a specific reason', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    const state = depositServer()
    server.use(
      http.post('/v1/me/deposits', async () => {
        state.deposits = [{ ...PENDING, status: 'declined' }]
        return HttpResponse.json({ ...PENDING, status: 'declined' }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<DepositPage />)
    await user.type(screen.getByPlaceholderText('50.00'), '50')
    await user.click(screen.getByRole('button', { name: 'Start deposit' }))
    expect(await screen.findByText(/This deposit was declined/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Simulate/ })).not.toBeInTheDocument()
  })
})
