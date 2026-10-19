import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { BACKOFFICE_API_DIR, checkCalls, extractCalls, formatFindings, loadOpenApi } from '../test/openapiContract'
import { getNavPermissions } from '../auth/permissions'
import { classifyFiling, isReasonCode } from './licensing'

const spec = loadOpenApi()
const src = readFileSync(join(BACKOFFICE_API_DIR, 'licensing.ts'), 'utf8')

// Every route the licensing screens use. The generic contract test (openapiContract.test.ts) scans every api module; this
// pins that licensing.ts really contains exactly these calls, so that test cannot pass vacuously for this module.
const EXPECTED = [
  'GET /v1/admin/jurisdictions',
  'GET /v1/admin/jurisdictions/{}',
  'GET /v1/admin/licences',
  'POST /v1/admin/licences',
  'GET /v1/admin/licences/{}',
  'GET /v1/admin/licences/{}/versions',
  'GET /v1/admin/licence-country-ceilings',
  'GET /v1/admin/operating-country-policies',
  'GET /v1/admin/tenants/{}/jurisdiction-configs',
  'PUT /v1/admin/tenants/{}/licence',
  'PUT /v1/admin/licences/{}/status',
  'PUT /v1/admin/licences/{}/validity-dates',
  'PUT /v1/admin/licences/{}/review-due',
  'PUT /v1/admin/licences/{}/evidence-reference',
  'GET /v1/admin/licensing-changes',
  'GET /v1/admin/licensing-changes/{}',
  'POST /v1/admin/licensing-changes/{}/approve',
  'POST /v1/admin/licensing-changes/{}/reject',
  'POST /v1/admin/licensing-changes/{}/cancel',
]

describe('api/licensing.ts against docs/api/openapi/platform-api.yaml', () => {
  const calls = extractCalls('api/licensing.ts', src)

  it('calls exactly the expected routes and each exists in the spec with the same method, body fields and query keys', () => {
    expect(calls.map((c) => `${c.method} ${c.path}`).sort()).toEqual([...EXPECTED].sort())
    const { findings } = checkCalls(spec, calls, {})
    expect(findings, `\n${formatFindings(findings)}\n`).toEqual([])
  })

  it('sends body fields and query keys (the check is not blind)', () => {
    const body = (m: string, p: string) => calls.find((c) => c.method === m && c.path === p)?.bodyKeys
    expect(body('PUT', '/v1/admin/licences/{}/status')).toEqual(['status', 'reason_code'])
    expect(body('PUT', '/v1/admin/tenants/{}/licence')).toEqual(['licence_id', 'reason_code'])
    expect(body('PUT', '/v1/admin/licences/{}/evidence-reference')).toEqual(['licence_evidence_reference', 'reason_code'])
    expect(body('POST', '/v1/admin/licensing-changes/{}/approve')).toEqual(['reason_code'])
    expect(calls.find((c) => c.path === '/v1/admin/licence-country-ceilings')?.queryKeys).toEqual(expect.arrayContaining(['licence_id', 'current']))
    expect(calls.find((c) => c.path === '/v1/admin/operating-country-policies')?.queryKeys).toEqual(expect.arrayContaining(['tenant_id']))
  })

  it('cancel documents and sends no body', () => {
    const cancel = calls.find((c) => c.path === '/v1/admin/licensing-changes/{}/cancel')
    expect(cancel?.bodyKeys ?? []).toEqual([])
  })

  it('never sends a client-chosen tenant id except the documented binding path value and the read target', () => {
    expect(src).not.toMatch(/JSON\.stringify\([^)]*tenant_id/)
  })

  it('does no hashing, signing or proof handling in the browser (the actor proof is server-side)', () => {
    expect(src.split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')).not.toMatch(/crypto\.subtle|sha256|hmac|X-Actor-Proof|actorProof|signature/i)
  })
})

describe('classifyFiling', () => {
  it('202 shape is pending, 200 lifecycle shape is applied, 200 unbind shape is unbound, anything else is unconfirmed', () => {
    expect(classifyFiling({ change_request: { id: 'x' }, executed: false, disclaimer: 'd' }).kind).toBe('pending')
    expect(classifyFiling({ licence: {}, change_kind: 'narrowing' }).kind).toBe('applied')
    expect(classifyFiling({ tenant_id: 't', licence_id: null }).kind).toBe('unbound')
    expect(classifyFiling({}).kind).toBe('unconfirmed')
    expect(classifyFiling(undefined).kind).toBe('unconfirmed')
    expect(classifyFiling(null).kind).toBe('unconfirmed')
    expect(classifyFiling('ok').kind).toBe('unconfirmed')
    expect(classifyFiling({ change_request: {} }).kind).toBe('unconfirmed')
  })
})

describe('reason code format', () => {
  it('accepts 1..64 of [A-Za-z0-9_.:-] and rejects everything else', () => {
    for (const ok of ['a', 'reviewed_ok', 'a.b:c-d_e', 'A'.repeat(64), '1abc']) expect(isReasonCode(ok), ok).toBe(true)
    for (const bad of ['', ' ', 'a b', 'a'.repeat(65), 'a/b', 'é', 'a\n', '_start', 'x@y']) expect(isReasonCode(bad), bad).toBe(false)
  })
})

describe('licensing permission flags (UI hint only; the server decides)', () => {
  it('only platform_admin holds them', () => {
    const f = (role: string) => {
      const p = getNavPermissions(role)
      return [p.licensingRead, p.licensingRequest, p.licensingApprove]
    }
    expect(f('platform_admin')).toEqual([true, true, true])
    for (const r of ['tenant_admin', 'compliance', 'finance', 'support', 'risk_manager', 'promotions_manager', 'bonus_operations', 'nobody']) expect(f(r)).toEqual([false, false, false])
    expect(getNavPermissions(undefined).licensingRead).toBe(false)
  })
})
