import { describe, expect, it } from 'vitest'
import { captureRequests } from '../test/contractCapture'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../test/session'
import { checkAgainstSpec, schemaEnum, schemaProps } from '../test/openapiLite'
import { GRANT_CAPABILITIES, approveGrantRequest, cancelGrantRequest, createGrantRequest, listGrantRequests, listGrants, listTenantStaff, rejectGrantRequest, revokeGrant } from './capabilityGrants'

const T = '11111111-1111-4111-8111-111111111111'
const X = '33333333-3333-4333-8333-333333333333'

describe('capabilityGrants api: UI-to-OpenAPI contract (K1)', () => {
  it('every request the module sends is documented, with only documented body fields', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen = captureRequests((r) => (r.method === 'POST' && /cancel|reject|revoke/.test(r.pathname) ? { status: 204 } : { body: [] }))
    await listGrantRequests(T)
    await listGrants(T)
    await listGrantRequests(T, { limit: 26, offset: 25 })
    await listGrants(T, { limit: 26, offset: 0 })
    await createGrantRequest(T, { grantee_staff_id: X, capability: 'payment_force_resolve:request', valid_until: '2026-12-01T00:00:00Z', reason_code: 'rota_cover' }, 'k1')
    await approveGrantRequest(T, X, 'ok', 'k2')
    await rejectGrantRequest(T, X, 'no', 'k3')
    await cancelGrantRequest(T, X, 'k4')
    await revokeGrant(T, X, 'left_team', 'k5')
    await listTenantStaff(T, { status: 'active', role: 'finance' })
    expect(seen).toHaveLength(10)
    for (const s of seen) expect(checkAgainstSpec(s)).toEqual([])
    // no tenant id in any body: the target tenant is the path value only
    for (const s of seen) if (s.body && typeof s.body === 'object') expect(Object.keys(s.body)).not.toContain('tenant_id')
    expect(seen[2].query.get('limit')).toBe('26')
    expect(seen[2].query.get('offset')).toBe('25')
    expect(seen[4].headers.get('Idempotency-Key')).toBe('k1')
  })

  it('the capability list equals the OpenAPI enum and K1 has no payload_hash anywhere', () => {
    expect([...GRANT_CAPABILITIES]).toEqual(schemaEnum('CapabilityGrantRequestBody', 'capability'))
    for (const n of ['CapabilityGrantRequestBody', 'CapabilityGrantDecisionBody', 'CapabilityGrantRevokeBody', 'CapabilityGrantRequest', 'CapabilityGrant']) {
      expect(schemaProps(n)).not.toContain('payload_hash')
    }
  })
})
