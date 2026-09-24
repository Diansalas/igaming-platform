import { fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { CasinoCataloguePage } from './CasinoCataloguePage'

const GAME_ID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc'

describe('CasinoCataloguePage (platform_admin)', () => {
  it('PUTs /v1/admin/casino/games with the upsertCasinoGameRequest body and shows the returned game id prominently', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const calls: { method: string; path: string; body: unknown }[] = []
    server.use(
      http.put('/v1/admin/casino/games', async ({ request }) => {
        calls.push({ method: request.method, path: new URL(request.url).pathname, body: await request.json() })
        return HttpResponse.json({
          id: GAME_ID,
          provider_id: 'mock-casino',
          name: 'Mock Fortune Slot',
          game_type: 'slot',
          feature_flags: null,
          supported_assets: ['USD'],
          mobile_supported: true,
          demo_supported: true,
        })
      }),
    )
    renderWithProviders(<CasinoCataloguePage />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Upsert game' }))

    expect(await screen.findByText(GAME_ID)).toBeInTheDocument()
    expect(screen.getByText(/Give this Game ID to the tenant admin to enable availability/)).toBeInTheDocument()
    expect(calls).toEqual([
      {
        method: 'PUT',
        path: '/v1/admin/casino/games',
        body: {
          provider_id: 'mock-casino',
          provider_game_id: 'mock-slot-001',
          name: 'Mock Fortune Slot',
          game_type: 'slot',
          supported_assets: ['USD'],
          mobile_supported: true,
          demo_supported: true,
          status: 'active',
        },
      },
    ])
  })

  it('shows a governance 409 verbatim with request id', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    server.use(
      http.put('/v1/admin/casino/games', () =>
        HttpResponse.json(
          { code: 'conflict', message: 'this change requires an approved catalogue change request', request_id: 'req-c' },
          { status: 409 },
        ),
      ),
    )
    renderWithProviders(<CasinoCataloguePage />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Upsert game' }))
    expect(await screen.findByText('this change requires an approved catalogue change request')).toBeInTheDocument()
    expect(screen.getByText('Request ID: req-c')).toBeInTheDocument()
  })

  it('double submit sends one PUT', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const spy = vi.fn()
    server.use(
      http.put('/v1/admin/casino/games', async () => {
        spy()
        await new Promise((r) => setTimeout(r, 20))
        return HttpResponse.json({ id: GAME_ID, provider_id: 'mock-casino', name: 'n', game_type: 'slot', feature_flags: [], supported_assets: [], mobile_supported: true, demo_supported: true })
      }),
    )
    renderWithProviders(<CasinoCataloguePage />)
    const button = screen.getByRole('button', { name: 'Upsert game' })
    fireEvent.click(button)
    fireEvent.click(button)
    expect(await screen.findByText(GAME_ID)).toBeInTheDocument()
    expect(spy).toHaveBeenCalledTimes(1)
  })
})
