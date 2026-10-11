import { describe, expect, it } from 'vitest'
import { ApiError, LAUNCH_ERROR_CODES } from '../../api/types'
import { getNavPermissions } from '../../auth/permissions'
import { buildNavItems } from '../../layout/nav'
import { LAUNCH_ERROR_MESSAGES, describeLaunchError } from './errors'
import { availableActions, isLegacyBaseline, subjectTransitions } from './actions'
import { groupDecimalString, subjectStatusLabel, subjectStatusTone, LAUNCH_DISCLAIMER } from './labels'
import { transition } from './launchFixtures'

describe('launch permissions mirror (UI hint only; the server decides)', () => {
  const launch = (role: string) => {
    const p = getNavPermissions(role)
    return [p.launchRead, p.launchRequest, p.launchApprove, p.launchSuspend, p.launchQueue]
  }
  it('platform_admin: everything incl. approve and the queue', () => {
    expect(launch('platform_admin')).toEqual([true, true, true, true, true])
  })
  it('tenant_admin and compliance: read/request/suspend, never approve nor the queue', () => {
    expect(launch('tenant_admin')).toEqual([true, true, false, true, false])
    expect(launch('compliance')).toEqual([true, true, false, true, false])
  })
  it('every other role and an unknown role: nothing', () => {
    for (const r of ['support', 'finance', 'risk_manager', 'promotions_manager', 'bonus_operations', 'nobody']) {
      expect(launch(r)).toEqual([false, false, false, false, false])
    }
    expect(getNavPermissions(undefined).launchRead).toBe(false)
  })
  it('only platform roles get the Launch Queue nav item', () => {
    const base = { sub: 's', principal_type: 'staff', exp: 0, iat: 0 }
    const plat = buildNavItems({ ...base, role: 'platform_admin', tenant_id: '00000000-0000-0000-0000-000000000000' })
    const ten = buildNavItems({ ...base, role: 'tenant_admin', tenant_id: '11111111-1111-1111-1111-111111111111' })
    expect(plat.map((i) => i.to)).toContain('/launch-queue')
    expect(ten.map((i) => i.to)).not.toContain('/launch-queue')
  })
})

describe('error mapping', () => {
  it('has a fixed sentence for every LAUNCH_* code and never echoes the server message', () => {
    for (const code of LAUNCH_ERROR_CODES) {
      const err = new ApiError(code, 'RAW SERVER TEXT select * from secrets', 409, 'req-1', ['H-2'])
      const v = describeLaunchError(err)
      expect(v.message, code).toBe(LAUNCH_ERROR_MESSAGES[code])
      expect(v.message).not.toContain('RAW SERVER TEXT')
      expect(v.message.length).toBeGreaterThan(20)
      expect(v.requestId).toBe('req-1')
    }
  })
  it('maps TENANT_CLOSE_BLOCKED_OPEN_ROUNDS and generic statuses without raw text', () => {
    expect(describeLaunchError(new ApiError('TENANT_CLOSE_BLOCKED_OPEN_ROUNDS', 'RAW', 409)).message).toMatch(/open sportsbook bets/)
    expect(describeLaunchError(new ApiError('forbidden', 'RAW', 403)).message).not.toContain('RAW')
    expect(describeLaunchError(new ApiError('not_found', 'RAW', 404)).message).toMatch(/Not found/)
    expect(describeLaunchError(new ApiError('validation_error', 'RAW', 400)).message).not.toContain('RAW')
    expect(describeLaunchError(new ApiError('internal_error', 'RAW pg error', 500)).message).not.toContain('RAW')
    expect(describeLaunchError(new ApiError('network_error', 'x', 0)).message).toMatch(/Could not reach/)
    expect(describeLaunchError(new Error('RAW boom')).message).not.toContain('RAW')
  })
  it('keeps only safe closed check codes, with fixed text', () => {
    const v = describeLaunchError(new ApiError('LAUNCH_PRECONDITION_FAILED', 'm', 409, undefined, ['H-3', 'R-JUR-CONFIG', '<script>alert(1)</script>', 'LA023']))
    expect(v.checkCodes.map((c) => c.code)).toEqual(['H-3', 'R-JUR-CONFIG', 'LA023'])
    expect(v.checkCodes[0].text).toMatch(/jurisdiction/i)
  })
})

describe('money rendering never uses floats', () => {
  it('groups a 1.8e19 decimal string exactly', () => {
    expect(groupDecimalString('18000000000000000000')).toBe('18,000,000,000,000,000,000')
    // a float would have printed 18000000000000000000 as 1.8e19 or lost the trailing digits
    expect(groupDecimalString('18000000000000000001')).toBe('18,000,000,000,000,000,001')
    expect(groupDecimalString('-1234567')).toBe('-1,234,567')
    expect(groupDecimalString('0')).toBe('0')
  })
  it('returns a non-digit value unchanged instead of coercing it', () => {
    expect(groupDecimalString('1.5e3')).toBe('1.5e3')
  })
})

describe('status vocabulary', () => {
  it('labels all four statuses and never renders an unknown value as if it were known', () => {
    expect(subjectStatusLabel('pending_launch')).toBe('Pending launch')
    expect(subjectStatusLabel('active')).toBe('Active')
    expect(subjectStatusLabel('suspended')).toBe('Suspended')
    expect(subjectStatusLabel('closed')).toBe('Closed')
    expect(subjectStatusLabel('weird')).toMatch(/Unrecognised/)
    expect(subjectStatusTone('weird')).toBe('danger')
  })
  it('the fixed disclaimer is the exact required sentence', () => {
    expect(LAUNCH_DISCLAIMER).toBe(
      'A recorded decision is not a legal authorisation. The operator and legal/compliance decide whether a brand may operate.',
    )
  })
})

describe('available actions (state machine hint)', () => {
  const platform = getNavPermissions('platform_admin')
  const tenantAdmin = getNavPermissions('tenant_admin')
  const base = { subjectKind: 'brand' as const, legacyBaseline: false, platformScope: true, hasPending: false }
  it('follows ADR 0112 section 3.1', () => {
    expect(availableActions({ ...base, status: 'pending_launch', perms: platform })).toEqual({ request: ['activate', 'close'], suspend: false })
    expect(availableActions({ ...base, status: 'active', perms: platform })).toEqual({ request: ['close'], suspend: true })
    expect(availableActions({ ...base, status: 'suspended', perms: platform })).toEqual({ request: ['reactivate', 'close'], suspend: false })
    expect(availableActions({ ...base, status: 'closed', perms: platform })).toEqual({ request: [], suspend: false })
  })
  it('offers ratify only for a legacy baseline subject', () => {
    expect(availableActions({ ...base, status: 'active', legacyBaseline: true, perms: platform }).request).toContain('ratify')
    expect(availableActions({ ...base, status: 'active', perms: platform }).request).not.toContain('ratify')
  })
  it('a tenant-scoped caller gets nothing for the tenant subject but can act on a brand', () => {
    expect(availableActions({ ...base, subjectKind: 'tenant', platformScope: false, status: 'active', perms: tenantAdmin })).toEqual({ request: [], suspend: false })
    expect(availableActions({ ...base, platformScope: false, status: 'active', perms: tenantAdmin }).suspend).toBe(true)
  })
  it('hides request buttons while a request is pending but keeps the emergency suspend', () => {
    expect(availableActions({ ...base, status: 'active', hasPending: true, perms: platform })).toEqual({ request: [], suspend: true })
  })
  it('a role without the permission is offered nothing', () => {
    expect(availableActions({ ...base, status: 'active', perms: getNavPermissions('finance') })).toEqual({ request: [], suspend: false })
  })
})

describe('legacy baseline detection', () => {
  it('is true only when the newest own transition is the legacy baseline', () => {
    const legacy = transition({ kind: 'legacy_baseline', from_status: undefined, to_status: 'active', created_at: '2026-01-01T00:00:00Z' })
    const governed = transition({ id: 't-2', created_at: '2026-10-01T00:00:00Z' })
    expect(isLegacyBaseline(subjectTransitions([legacy], 'brand'))).toBe(true)
    expect(isLegacyBaseline(subjectTransitions([legacy, governed], 'brand'))).toBe(false)
    expect(isLegacyBaseline(subjectTransitions([legacy], 'tenant'))).toBe(false)
  })
})
