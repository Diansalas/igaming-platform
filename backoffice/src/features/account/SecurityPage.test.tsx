import { screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { signInAs } from '../../test/session'
import { SecurityPage } from './SecurityPage'

function status(over: Record<string, unknown>) {
  server.use(
    http.get('/v1/staff/me/mfa', () =>
      HttpResponse.json({
        enrolled: true,
        factor_type: 'totp',
        activated_at: '2026-10-01T09:30:00Z',
        recovery_codes_remaining: 8,
        hard_locked: false,
        locked_until: null,
        ...over,
      }),
    ),
  )
}

describe('SecurityPage', () => {
  it('shows factor status and recovery codes remaining, with no warning at 8', async () => {
    signInAs({})
    renderWithProviders(<SecurityPage />)
    expect(await screen.findByText('Enrolled')).toBeInTheDocument()
    expect(screen.getByText('Authenticator app (TOTP)')).toBeInTheDocument()
    expect(screen.getByText('2026-10-01 09:30 UTC')).toBeInTheDocument()
    expect(screen.getByText('8')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it.each([
    [3, 'running low'],
    [1, 'running low'],
    [0, 'no recovery codes left'],
  ])('warns at %i remaining', async (n, text) => {
    signInAs({})
    status({ recovery_codes_remaining: n })
    renderWithProviders(<SecurityPage />)
    expect(await screen.findByRole('alert')).toHaveTextContent(text)
  })

  it('does not warn at 4', async () => {
    signInAs({})
    status({ recovery_codes_remaining: 4 })
    renderWithProviders(<SecurityPage />)
    await screen.findByText('Enrolled')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('shows a hard lock', async () => {
    signInAs({})
    status({ hard_locked: true })
    renderWithProviders(<SecurityPage />)
    expect(await screen.findByText(/Ask your administrator to reset/)).toBeInTheDocument()
  })

  it('shows a soft lock', async () => {
    signInAs({})
    status({ locked_until: '2026-10-13T10:15:00Z' })
    renderWithProviders(<SecurityPage />)
    expect(await screen.findByText(/Temporarily locked until 2026-10-13 10:15 UTC/)).toBeInTheDocument()
  })

  it('shows not enrolled', async () => {
    signInAs({})
    status({ enrolled: false, factor_type: null, activated_at: null, recovery_codes_remaining: 0 })
    renderWithProviders(<SecurityPage />)
    expect(await screen.findByText('Not enrolled')).toBeInTheDocument()
  })

  it('offers regeneration only to an enrolled account (a hint; the server decides)', async () => {
    signInAs({})
    status({ enrolled: false, factor_type: null, activated_at: null, recovery_codes_remaining: 0 })
    renderWithProviders(<SecurityPage />)
    await screen.findByText('Not enrolled')
    expect(screen.queryByRole('button', { name: /regenerate/i })).not.toBeInTheDocument()
    expect(screen.queryByText(/NOT IMPLEMENTED/)).not.toBeInTheDocument()
  })

  it.each([
    [503, 'service_unavailable', 'Sign-in is temporarily unavailable. Try again.'],
    [500, 'internal_error', 'Could not load your security status. Try again.'],
  ])('degrades to a fixed message on %i', async (st, code, text) => {
    signInAs({})
    server.use(http.get('/v1/staff/me/mfa', () => HttpResponse.json({ code, message: 'RAW' }, { status: st })))
    renderWithProviders(<SecurityPage />)
    expect(await screen.findByRole('alert')).toHaveTextContent(text)
  })
})
