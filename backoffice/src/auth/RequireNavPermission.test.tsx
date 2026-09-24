import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { setSession } from './tokenStore'
import { makeTestJwt } from '../test/jwt'
import { renderWithProviders } from '../test/renderWithProviders'
import { RequireNavPermission } from './RequireNavPermission'

describe('RequireNavPermission', () => {
  it('renders children when the signed-in role holds the permission', () => {
    setSession(makeTestJwt({ role: 'finance' }), 'rt')
    renderWithProviders(
      <RequireNavPermission permission="withdrawals">
        <div>Withdrawal queue</div>
      </RequireNavPermission>,
    )
    expect(screen.getByText('Withdrawal queue')).toBeInTheDocument()
  })

  it('renders a not-authorized state (and never the gated content) when the role lacks the permission', () => {
    setSession(makeTestJwt({ role: 'support' }), 'rt')
    renderWithProviders(
      <RequireNavPermission permission="withdrawals">
        <div>Withdrawal queue</div>
      </RequireNavPermission>,
    )
    expect(screen.queryByText('Withdrawal queue')).not.toBeInTheDocument()
    expect(screen.getByText(/does not have access/i)).toBeInTheDocument()
  })

  it.each([
    ['tenant_admin', undefined, 'providerConfig', true],
    ['finance', undefined, 'providerConfig', false],
    ['compliance', undefined, 'casinoConfig', false],
    ['platform_admin', '00000000-0000-0000-0000-000000000000', 'catalogue', true],
    ['tenant_admin', undefined, 'catalogue', false],
  ] as const)('%s -> %s gate: allowed=%s', (role, tenantId, permission, allowed) => {
    setSession(makeTestJwt({ role, ...(tenantId ? { tenant_id: tenantId } : {}) }), 'rt')
    renderWithProviders(
      <RequireNavPermission permission={permission}>
        <div>Gated page</div>
      </RequireNavPermission>,
    )
    if (allowed) expect(screen.getByText('Gated page')).toBeInTheDocument()
    else expect(screen.queryByText('Gated page')).not.toBeInTheDocument()
  })
})
