import { screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import type { KycCase } from '../../api/kyc'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { renderWithProviders } from '../../test/renderWithProviders'
import { KycCaseDetail } from './KycCaseDetail'

// Stage 10.3 gate-W1 (security S-6, identity-compliance condition 2; ADR
// 0028 amendment item 5): the staff KYC case view must render the bounded
// provider reason as ESCAPED TEXT, never as markup.
const hostileReason = '<img src=x onerror=alert(1)><script>window.__kycXss=1</script>'

function kycCase(reason: string): KycCase {
  return {
    id: 'verification-1',
    player_account_id: 'player-1',
    brand_id: 'brand-1',
    status: 'rejected',
    provider_id: 'mock',
    reason,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    has_verified_residence: false,
  }
}

describe('KycCaseDetail', () => {
  beforeEach(() => {
    setSession(makeTestJwt({ role: 'compliance' }), 'refresh-token-for-test')
  })

  it('renders the staff KYC reason as escaped text, never as HTML', () => {
    const { container } = renderWithProviders(<KycCaseDetail kycCase={kycCase(hostileReason)} onClose={() => {}} />)

    const dialog = screen.getByRole('dialog', { name: 'KYC case detail' })
    // The exact string is present as a text node...
    expect(screen.getByText(hostileReason)).toBeInTheDocument()
    // ...and no element was created from it.
    expect(dialog.querySelector('img')).toBeNull()
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('script')).toBeNull()
    expect((window as unknown as { __kycXss?: number }).__kycXss).toBeUndefined()
    // The serialized DOM carries the escaped form.
    expect(dialog.innerHTML).toContain('&lt;img src=x onerror=alert(1)&gt;')
  })
})
