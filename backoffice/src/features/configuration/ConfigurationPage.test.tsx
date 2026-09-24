import { fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { signInAs } from '../../test/session'
import { ConfigurationPage } from './ConfigurationPage'

const GAME_ID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'

describe('ConfigurationPage (tenant_admin)', () => {
  it('payment capability: PUTs the exact writeCapabilityRequest body to /v1/admin/providers/mock-payments/capability', async () => {
    signInAs({ role: 'tenant_admin' })
    const calls: { method: string; path: string; body: unknown }[] = []
    server.use(
      http.put('/v1/admin/providers/:pid/capability', async ({ request }) => {
        calls.push({ method: request.method, path: new URL(request.url).pathname, body: await request.json() })
        return HttpResponse.json({ id: 'cap-pay-1' })
      }),
    )
    renderWithProviders(<ConfigurationPage />)
    const user = userEvent.setup()

    const fiat = screen.getByLabelText('Supported fiat currencies')
    await user.clear(fiat)
    await user.type(fiat, 'EUR, USD')
    await user.click(screen.getByLabelText('Supports refund/reversal'))
    await user.click(screen.getByRole('button', { name: 'Add amount limit' }))
    await user.type(screen.getByLabelText('Limit 1 asset code'), 'EUR')
    await user.type(screen.getByLabelText('Limit 1 min amount'), '100')
    await user.type(screen.getByLabelText('Limit 1 max amount'), '500000')
    await user.click(screen.getByRole('button', { name: 'Save payment capability' }))

    expect(await screen.findByText('cap-pay-1')).toBeInTheDocument()
    expect(calls).toEqual([
      {
        method: 'PUT',
        path: '/v1/admin/providers/mock-payments/capability',
        body: {
          supported_fiat_currencies: ['EUR', 'USD'],
          supported_crypto_assets: [],
          supported_payment_methods: ['card'],
          supported_countries: [],
          supports_deposit: true,
          supports_withdrawal: true,
          supports_refund_reversal: true,
          amount_limits: [{ asset_code: 'EUR', min_amount: 100, max_amount: 500000 }],
          priority: 1,
          status: 'active',
        },
      },
    ])
    expect(screen.getAllByText(/Current configuration cannot be read back \(no read API\)/).length).toBe(2)
  })

  it('payment capability: shows a widening 400 verbatim with request id', async () => {
    signInAs({ role: 'tenant_admin' })
    server.use(
      http.put('/v1/admin/providers/:pid/capability', () =>
        HttpResponse.json(
          {
            code: 'validation_error',
            message: "payments: capability configuration widens adapter: supported_payment_methods exceeds adapter mock-payments's declared set",
            request_id: 'req-p',
          },
          { status: 400 },
        ),
      ),
    )
    renderWithProviders(<ConfigurationPage />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Save payment capability' }))
    expect(
      await screen.findByText(
        "payments: capability configuration widens adapter: supported_payment_methods exceeds adapter mock-payments's declared set",
      ),
    ).toBeInTheDocument()
    expect(screen.getByText('Request ID: req-p')).toBeInTheDocument()
  })

  it('payment capability: double submit sends one PUT', async () => {
    signInAs({ role: 'tenant_admin' })
    const spy = vi.fn()
    server.use(
      http.put('/v1/admin/providers/:pid/capability', async () => {
        spy()
        await new Promise((r) => setTimeout(r, 20))
        return HttpResponse.json({ id: 'cap-once' })
      }),
    )
    renderWithProviders(<ConfigurationPage />)
    const button = screen.getByRole('button', { name: 'Save payment capability' })
    fireEvent.click(button)
    fireEvent.click(button)
    expect(await screen.findByText('cap-once')).toBeInTheDocument()
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('casino capability: PUTs the exact writeCasinoCapabilityRequest body to /v1/admin/casino/providers/mock-casino/capability', async () => {
    signInAs({ role: 'tenant_admin' })
    const calls: { path: string; body: unknown }[] = []
    server.use(
      http.put('/v1/admin/casino/providers/:pid/capability', async ({ request }) => {
        calls.push({ path: new URL(request.url).pathname, body: await request.json() })
        return HttpResponse.json({ id: 'cap-casino-1' })
      }),
    )
    renderWithProviders(<ConfigurationPage />)
    const user = userEvent.setup()
    await user.click(screen.getByLabelText('Supports rollback'))
    await user.click(screen.getByRole('button', { name: 'Save casino capability' }))
    expect(await screen.findByText('cap-casino-1')).toBeInTheDocument()
    expect(calls).toEqual([
      {
        path: '/v1/admin/casino/providers/mock-casino/capability',
        body: {
          supports_catalogue: true,
          supports_launch: true,
          supports_balance: true,
          supports_bet: true,
          supports_win: true,
          supports_rollback: false,
          supported_assets: ['USD'],
          supported_game_types: ['slot'],
          priority: 1,
          status: 'active',
        },
      },
    ])
  })

  it('casino capability: shows a 404 unknown provider error', async () => {
    signInAs({ role: 'tenant_admin' })
    server.use(
      http.put('/v1/admin/casino/providers/:pid/capability', () =>
        HttpResponse.json({ code: 'not_found', message: 'no adapter registered for this provider id' }, { status: 404 }),
      ),
    )
    renderWithProviders(<ConfigurationPage />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Save casino capability' }))
    expect(await screen.findByText('no adapter registered for this provider id')).toBeInTheDocument()
  })

  it('game availability: PUTs {enabled:true} to /v1/admin/casino/games/{gameID}/availability', async () => {
    signInAs({ role: 'tenant_admin' })
    const calls: { path: string; body: unknown }[] = []
    server.use(
      http.put('/v1/admin/casino/games/:gid/availability', async ({ request, params }) => {
        calls.push({ path: new URL(request.url).pathname, body: await request.json() })
        return HttpResponse.json({ game_id: params.gid, enabled: true })
      }),
    )
    renderWithProviders(<ConfigurationPage />)
    const user = userEvent.setup()
    expect(screen.getByRole('button', { name: 'Save availability' })).toBeDisabled()
    await user.type(screen.getByLabelText('Game ID'), GAME_ID)
    await user.click(screen.getByRole('button', { name: 'Save availability' }))
    expect(await screen.findByText(/is now enabled for this tenant/)).toBeInTheDocument()
    expect(calls).toEqual([{ path: `/v1/admin/casino/games/${GAME_ID}/availability`, body: { enabled: true } }])
  })

  it('game availability: shows a 404 game not found error', async () => {
    signInAs({ role: 'tenant_admin' })
    server.use(
      http.put('/v1/admin/casino/games/:gid/availability', () =>
        HttpResponse.json({ code: 'not_found', message: 'game not found', request_id: 'req-g' }, { status: 404 }),
      ),
    )
    renderWithProviders(<ConfigurationPage />)
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Game ID'), GAME_ID)
    await user.click(screen.getByRole('button', { name: 'Save availability' }))
    expect(await screen.findByText('game not found')).toBeInTheDocument()
    expect(screen.getByText('Request ID: req-g')).toBeInTheDocument()
  })
})
