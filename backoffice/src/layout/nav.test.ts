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
    expect(items).toContain('Sportsbook')
    expect(items).toContain('Casino')
    expect(items).not.toContain('Withdrawals')
    expect(items).not.toContain('Tenants')
  })

  it('gives finance the withdrawals, the A10b governed-payment consoles, sportsbook, and casino sections, nothing else', () => {
    const items = buildNavItems({ ...baseClaims, role: 'finance' }).map((i) => i.label)
    // ADR 0114 S3/S5/S8: parked payments + force resolutions (payment_force_resolve:*), reconciliation. Finance holds no
    // capability_grant:* (K1 grantee only), so no Capability Grants entry.
    expect(items).toEqual(['Withdrawals', 'Parked Payments', 'Force Resolutions', 'Reconciliation', 'Sportsbook', 'Casino'])
  })

  it('A10b: each new console appears for exactly the roles the server permits (UI hint only)', () => {
    const labels = (role: string, platform = false) =>
      buildNavItems({ ...baseClaims, role, tenant_id: platform ? '00000000-0000-0000-0000-000000000000' : baseClaims.tenant_id }).map((i) => i.label)
    const NEW = ['Parked Payments', 'Force Resolutions', 'Hold Release', 'Reconciliation', 'Capability Grants']
    const only = (items: string[]) => items.filter((l) => NEW.includes(l))
    expect(only(labels('platform_admin', true))).toEqual(['Parked Payments', 'Force Resolutions', 'Hold Release', 'Reconciliation', 'Capability Grants'])
    expect(only(labels('tenant_admin'))).toEqual(['Reconciliation', 'Capability Grants'])
    expect(only(labels('compliance'))).toEqual(['Parked Payments', 'Force Resolutions', 'Reconciliation', 'Capability Grants'])
    expect(only(labels('finance'))).toEqual(['Parked Payments', 'Force Resolutions', 'Reconciliation'])
    for (const r of ['support', 'risk_manager', 'promotions_manager', 'bonus_operations']) expect(only(labels(r))).toEqual([])
  })

  it('gives a platform_admin (nil-tenant) principal Tenants + Casino Catalogue + Platform Audit Log, never tenant-operational sections', () => {
    const items = buildNavItems({
      ...baseClaims,
      role: 'platform_admin',
      tenant_id: '00000000-0000-0000-0000-000000000000',
    }).map((i) => i.label)
    // Launch Queue mirrors launch_authorisation:read for platform scope (ADR 0112).
    // Casino Catalogue mirrors PermCasinoCatalogueManage (platform_admin only).
    // Alerts mirrors alert:manage (platform_admin only; ADR 0114 S1).
    expect(items).toEqual(['Tenants', 'Launch Queue', 'Alerts', 'Payment Kill Switch', 'Parked Payments', 'Force Resolutions', 'Hold Release', 'Reconciliation', 'Capability Grants', 'Casino Catalogue', 'Platform Audit Log'])
  })

  it('gives tenant_admin the Providers & Games configuration page but never the platform catalogue', () => {
    const items = buildNavItems(baseClaims).map((i) => i.label)
    expect(items).toContain('Providers & Games')
    expect(items).toContain('My Tenant')
    expect(items).not.toContain('Casino Catalogue')
  })

  it('gives compliance its review sections but no configuration, catalogue, or withdrawals', () => {
    const items = buildNavItems({ ...baseClaims, role: 'compliance' }).map((i) => i.label)
    // 'My Tenant': own-tenant read since ADR 0112 slice 6 (b) (brand discovery for the launch panel).
    expect(items).toEqual(['My Tenant', 'Players', 'KYC', 'Responsible Gaming', 'Bonus', 'Parked Payments', 'Force Resolutions', 'Reconciliation', 'Capability Grants', 'Sportsbook', 'Casino', 'Audit Log'])
  })

  it('never gives finance configuration or catalogue pages', () => {
    const items = buildNavItems({ ...baseClaims, role: 'finance' }).map((i) => i.label)
    expect(items).not.toContain('Providers & Games')
    expect(items).not.toContain('Casino Catalogue')
  })
})
