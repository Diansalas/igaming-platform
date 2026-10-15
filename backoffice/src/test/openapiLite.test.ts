import { describe, expect, it } from 'vitest'
import { checkAgainstSpec, queryParams, requestBodySchema, schemaEnum, schemaProps, schemaRequired } from './openapiLite'

const T = '11111111-1111-4111-8111-111111111111'
const req = (method: string, pathname: string, body?: unknown, q = '') => ({ method, pathname, query: new URLSearchParams(q), body })

// The contract checker is only worth something if it can fail. These are its own negative controls.
describe('openapiLite negative controls', () => {
  it('reads the spec (non-vacuous)', () => {
    expect(queryParams('/v1/admin/tenants/{tenantID}/payment-attempts', 'get')).toContain('terminal_reason')
    expect(requestBodySchema('/v1/admin/tenants/{tenantID}/payment-force-resolutions', 'post')).toBe('PaymentForceResolutionRequest')
    expect(schemaProps('ResolutionDecisionRequest')).toEqual(['tenant_id', 'payload_hash', 'reason_code'])
    expect(schemaRequired('ResolutionDecisionRequest')).toEqual(['payload_hash', 'reason_code'])
    expect(schemaEnum('PaymentAttemptState').length).toBe(8)
  })
  it('flags an undocumented path, method, query parameter, body field and a missing required field', () => {
    expect(checkAgainstSpec(req('GET', `/v1/admin/tenants/${T}/nope`))[0]).toMatch(/not in spec/)
    expect(checkAgainstSpec(req('DELETE', `/v1/admin/tenants/${T}/payment-attempts`))[0]).toMatch(/not in spec/)
    expect(checkAgainstSpec(req('GET', `/v1/admin/tenants/${T}/payment-attempts`, undefined, 'tenant_id=x'))[0]).toMatch(/not declared/)
    const p = `/v1/admin/tenants/${T}/payment-force-resolutions/${T}/approve`
    expect(checkAgainstSpec(req('POST', p, { payload_hash: 'a', reason_code: 'b', extra: 1 }))[0]).toMatch(/extra/)
    expect(checkAgainstSpec(req('POST', p, { reason_code: 'b' }))[0]).toMatch(/payload_hash/)
    expect(checkAgainstSpec(req('POST', p))[0]).toMatch(/requires a body/)
    expect(checkAgainstSpec(req('POST', p, { payload_hash: 'a', reason_code: 'b' }))).toEqual([])
  })
})
