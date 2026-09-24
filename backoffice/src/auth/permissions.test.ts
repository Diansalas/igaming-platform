import { describe, expect, it } from 'vitest'
import { getNavPermissions } from './permissions'

/**
 * Pins the write-capability flags to internal/auth/permission.go's
 * rolePermissions map. UI convenience only - the backend re-checks every
 * permission on every request.
 */
describe('getNavPermissions write flags', () => {
  const flags = (role: string) => {
    const p = getNavPermissions(role)
    return {
      tenantWrite: p.tenantWrite,
      brandWrite: p.brandWrite,
      staffAdmin: p.staffAdmin,
      providerConfig: p.providerConfig,
      casinoConfig: p.casinoConfig,
      catalogue: p.catalogue,
      withdrawals: p.withdrawals,
    }
  }

  it('platform_admin: tenant/brand/staff/catalogue writes, no tenant-scoped provider config, no withdrawals', () => {
    expect(flags('platform_admin')).toEqual({
      tenantWrite: true,
      brandWrite: true,
      staffAdmin: true,
      providerConfig: false,
      casinoConfig: false,
      catalogue: true,
      withdrawals: false,
    })
  })

  it('tenant_admin: brand/staff/provider/casino config, never tenant create, catalogue, or withdrawals', () => {
    expect(flags('tenant_admin')).toEqual({
      tenantWrite: false,
      brandWrite: true,
      staffAdmin: true,
      providerConfig: true,
      casinoConfig: true,
      catalogue: false,
      withdrawals: false,
    })
  })

  it('finance: withdrawals only among these flags', () => {
    expect(flags('finance')).toEqual({
      tenantWrite: false,
      brandWrite: false,
      staffAdmin: false,
      providerConfig: false,
      casinoConfig: false,
      catalogue: false,
      withdrawals: true,
    })
  })

  it('compliance: none of these write flags', () => {
    expect(Object.values(flags('compliance')).every((v) => v === false)).toBe(true)
  })

  it('unknown role or no role gets nothing', () => {
    expect(Object.values(flags('nope')).every((v) => v === false)).toBe(true)
    expect(getNavPermissions(undefined).staffAdmin).toBe(false)
  })
})
