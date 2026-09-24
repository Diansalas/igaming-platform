import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { AccountPage } from './AccountPage'

const ME = {
  id: 'player-1',
  email: 'player@example.com',
  kyc_tier: 0,
  tenant_id: '11111111-1111-1111-1111-111111111111',
  brand_id: '22222222-2222-2222-2222-222222222222',
}

/** GET /v1/me reports pending_verification until confirm succeeds - the status only ever comes from the server. */
function meUntilConfirmed() {
  let verified = false
  const confirmed: string[] = []
  server.use(
    http.get('/v1/me', () => HttpResponse.json({ ...ME, status: verified ? 'active' : 'pending_verification' })),
    http.post('/v1/auth/email-verification/confirm', async ({ request }) => {
      const body = (await request.json()) as { token: string }
      confirmed.push(body.token)
      if (body.token !== 'good-code') {
        return HttpResponse.json({ code: 'validation_error', message: 'invalid or expired verification token' }, { status: 400 })
      }
      verified = true
      return new HttpResponse(null, { status: 204 })
    }),
  )
  return confirmed
}

describe('Email verification on the account page', () => {
  it('is not shown for an active account', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    renderWithProviders(<AccountPage />)
    expect(await screen.findByText('active')).toBeInTheDocument()
    expect(screen.queryByText('Verify your email')).not.toBeInTheDocument()
  })

  it('staging: shows the server-returned code, confirms it, and the account becomes active', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    const confirmed = meUntilConfirmed()
    server.use(http.post('/v1/me/email-verification/request', () => HttpResponse.json({ token: 'good-code' })))
    const user = userEvent.setup()

    renderWithProviders(<AccountPage />)
    await user.click(await screen.findByRole('button', { name: 'Send verification code' }))

    expect(await screen.findByText(/Staging only: the server returned your verification code/)).toBeInTheDocument()
    expect(screen.getByLabelText('Verification code')).toHaveValue('good-code')

    await user.click(screen.getByRole('button', { name: 'Verify email' }))

    expect(await screen.findByText('active')).toBeInTheDocument()
    expect(confirmed).toEqual(['good-code'])
    await waitFor(() => expect(screen.queryByText('Verify your email')).not.toBeInTheDocument())
  })

  it('production (204, no code in the response): never shows a code, the player types it from email', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    const confirmed = meUntilConfirmed()
    server.use(http.post('/v1/me/email-verification/request', () => new HttpResponse(null, { status: 204 })))
    const user = userEvent.setup()

    renderWithProviders(<AccountPage />)
    await user.click(await screen.findByRole('button', { name: 'Send verification code' }))

    expect(await screen.findByText(/on its way to your email/)).toBeInTheDocument()
    expect(screen.queryByText(/Staging only/)).not.toBeInTheDocument()
    expect(screen.getByLabelText('Verification code')).toHaveValue('')
    expect(screen.getByRole('button', { name: 'Resend verification code' })).toBeInTheDocument()

    await user.type(screen.getByLabelText('Verification code'), 'good-code')
    await user.click(screen.getByRole('button', { name: 'Verify email' }))
    expect(await screen.findByText('active')).toBeInTheDocument()
    expect(confirmed).toEqual(['good-code'])
  })

  it('an invalid or expired code shows an actionable error and the account stays pending', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    meUntilConfirmed()
    const user = userEvent.setup()

    renderWithProviders(<AccountPage />)
    await user.type(await screen.findByLabelText('Verification code'), 'stale-code')
    await user.click(screen.getByRole('button', { name: 'Verify email' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('invalid, expired or already used')
    expect(screen.getByText('pending_verification')).toBeInTheDocument()
  })

  it('a failed code request shows the error', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    meUntilConfirmed()
    server.use(
      http.post('/v1/me/email-verification/request', () =>
        HttpResponse.json({ code: 'service_unavailable', message: 'email verification is temporarily unavailable' }, { status: 503 }),
      ),
    )
    const user = userEvent.setup()

    renderWithProviders(<AccountPage />)
    await user.click(await screen.findByRole('button', { name: 'Send verification code' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('email verification is temporarily unavailable')
  })
})
