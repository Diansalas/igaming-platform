import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { ComplianceStatusCard } from './ComplianceStatusCard'

describe('ComplianceStatusCard', () => {
  it('shows no KYC case and no RG restriction by default, and starts a verification case', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    let cases: Array<Record<string, unknown>> = []
    let posts = 0
    server.use(
      http.get('/v1/me/kyc/verifications', () => HttpResponse.json(cases)),
      http.post('/v1/me/kyc/verifications', () => {
        posts++
        cases = [{ id: 'v-1', status: 'pending', provider_id: 'mock', created_at: '2026-09-25T10:00:00Z' }]
        return HttpResponse.json(cases[0], { status: 201 })
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<ComplianceStatusCard kycTier={0} />)

    expect(await screen.findByText('No verification started.')).toBeInTheDocument()
    expect(screen.getByText('No active restrictions on your account.')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Start identity verification' }))
    expect(await screen.findByText('pending')).toBeInTheDocument()
    expect(posts).toBe(1)
  })

  it('lists only active RG restrictions', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    server.use(
      http.get('/v1/me/rg/status', () =>
        HttpResponse.json([
          { id: 'r-1', restriction_type: 'self_exclusion', scope: 'tenant', starts_at: 'x', indefinite: true, source: 'player', active: true },
          { id: 'r-2', restriction_type: 'cool_off', scope: 'brand', starts_at: 'x', indefinite: false, source: 'staff', active: false },
        ]),
      ),
    )
    renderWithProviders(<ComplianceStatusCard kycTier={1} />)
    expect(await screen.findByText('self_exclusion')).toBeInTheDocument()
    expect(screen.queryByText('cool_off')).not.toBeInTheDocument()
    expect(screen.getByText('KYC tier: 1')).toBeInTheDocument()
  })
})
