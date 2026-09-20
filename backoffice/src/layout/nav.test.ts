import { describe, expect, it } from 'vitest'
import type { StaffClaims } from '../auth/jwt'
import { buildNavItems } from './nav'

/**
 * This test proves only that the sidebar RENDERS a different set of links
 * for different roles - a client-side convenience. It does NOT prove
 * authorization: hiding a link here never stops a network call, and the
 * server independently enforces every permission on every request
 * regardless of what this function returns (see auth/permissions.ts's own
 * doc comment).
 */
describe('buildNavItems', () => {
  const baseClaims: StaffClaims = {
    sub: 'staff-1',
    tenant_id: '11111111-1111-1111-1111-111111111111',
    role: 'tenant_admin',
    principal_type: 'staff',
    exp: 0,
    iat: 0,
  }

  it('returns no items for an unauthenticated (null) principal', () => {
    expect(buildNavItems(null)).toEqual([])
  })

  it('gives tenant_admin the tenant-scoped operational sections', () => {
    const items = buildNavItems(baseClaims).map((i) => i.label)
    expect(items).toContain('Players')
    expect(items).toContain('KYC')
    expect(items).toContain('Responsible Gaming')
    expect(items).toContain('Bonus')
    expect(items).toContain('Audit Log')
    expect(items).not.toContain('Withdrawals')
    expect(items).not.toContain('Tenants')
  })

  it('gives finance ONLY the withdrawals section', () => {
    const items = buildNavItems({ ...baseClaims, role: 'finance' }).map((i) => i.label)
    expect(items).toEqual(['Withdrawals'])
  })

  it('gives a platform_admin (nil-tenant) principal Tenants + Platform Audit Log, never tenant-operational sections', () => {
    const items = buildNavItems({
      ...baseClaims,
      role: 'platform_admin',
      tenant_id: '00000000-0000-0000-0000-000000000000',
    }).map((i) => i.label)
    expect(items).toEqual(['Tenants', 'Platform Audit Log'])
  })
})
