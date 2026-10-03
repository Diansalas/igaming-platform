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

/**
 * PRH-2 K2 (ADR 0100): the manual-adjustment flags mirror
 * internal/auth/permission.go's ledger_adjustment:{read,initiate,approve}
 * role sets exactly. UI convenience only.
 */
describe('getNavPermissions manual adjustment flags', () => {
  const ma = (role: string) => {
    const p = getNavPermissions(role)
    return [p.manualAdjustments, p.manualAdjustmentAct]
  }
  it('finance and platform_admin may view and act (the grant is still required server-side)', () => {
    expect(ma('finance')).toEqual([true, true])
    expect(ma('platform_admin')).toEqual([true, true])
  })
  it('compliance views only', () => {
    expect(ma('compliance')).toEqual([true, false])
  })
  it('every other role neither views nor acts', () => {
    for (const role of ['tenant_admin', 'support', 'risk_manager', 'promotions_manager', 'bonus_operations', 'player', undefined]) {
      expect(ma(role as string)).toEqual([false, false])
    }
  })
})
