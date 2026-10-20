import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { BACKOFFICE_API_DIR, checkCalls, extractCalls, formatFindings, loadOpenApi } from '../test/openapiContract'
import { classifyResolution, classifySetPolicy, parseRequests, parseTriggerPolicy, TRIGGER_EVENTS, SCREENING_LISTS } from './amlScreening'

const spec = loadOpenApi()
const src = readFileSync(join(BACKOFFICE_API_DIR, 'amlScreening.ts'), 'utf8')
const calls = extractCalls('api/amlScreening.ts', src)
const U = 'a1000000-0000-4000-8000-000000000001'

describe('api/amlScreening.ts against docs/api/openapi/platform-api.yaml', () => {
  it('calls exactly the four lic18 routes, each in the spec with the same method, body fields and query keys', () => {
    expect(calls.map((c) => `${c.method} ${c.path}`).sort()).toEqual(
      [
        'GET /v1/admin/tenants/{}/screening/trigger-policy',
        'POST /v1/admin/tenants/{}/screening/trigger-policy',
        'GET /v1/admin/tenants/{}/screening/persons/{}/requests',
        'POST /v1/admin/tenants/{}/screening/persons/{}/requests/{}/resolution',
      ].sort(),
    )
    const { findings } = checkCalls(spec, calls, {})
    expect(findings, `\n${formatFindings(findings)}\n`).toEqual([])
  })
  it('request pins: query key, body keys', () => {
    expect(calls.find((c) => c.method === 'GET' && c.path.endsWith('/trigger-policy'))?.queryKeys).toEqual(['jurisdiction_id'])
    const post = calls.find((c) => c.method === 'POST' && c.path.endsWith('/trigger-policy'))
    expect(post?.bodyKeys).toBeDefined()
  })
  it('the UI event and list vocabularies equal the spec enums', () => {
    const s = spec.components?.schemas
    expect(s?.AMLScreeningTriggerPolicyRequest?.properties?.events?.items?.enum).toEqual([...TRIGGER_EVENTS])
    expect(s?.AMLScreeningResolutionRequest?.properties?.outcome?.enum).toEqual(['clear', 'potential_match', 'confirmed', 'error'])
    expect([...SCREENING_LISTS]).toEqual(['sanctions', 'pep'])
  })
  it('does no signing, hashing or proof handling in the browser, and no float conversion', () => {
    const code = src.split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')
    expect(code).not.toMatch(/crypto\.subtle|sha256|hmac|X-Actor-Proof|actorProof|signature/i)
    expect(code).not.toMatch(/parseFloat|toFixed|Number\s*\(/)
  })
})

describe('classifiers never fake success', () => {
  it('policy write', () => {
    expect(classifySetPolicy({ policy_id: U }).kind).toBe('confirmed')
    for (const v of [null, undefined, {}, 'ok', [], { policy_id: 'x' }, { policy_id: 5 }]) expect(classifySetPolicy(v).kind).toBe('unconfirmed')
  })
  it('resolution', () => {
    expect(classifyResolution({ result_id: U, replayed: false })).toEqual({ kind: 'confirmed', resultId: U, replayed: false, holdId: null })
    expect(classifyResolution({ result_id: U, replayed: true, hold_id: U })).toEqual({ kind: 'confirmed', resultId: U, replayed: true, holdId: U })
    for (const v of [null, {}, { result_id: U }, { replayed: true }, { result_id: 'x', replayed: true }, { result_id: U, replayed: 'no' }, { result_id: U, replayed: true, hold_id: 'x' }]) expect(classifyResolution(v).kind).toBe('unconfirmed')
  })
  it('policy and list shapes are checked, not trusted', () => {
    expect(parseTriggerPolicy({ configured: false, events: [], gate_withdrawal: false })).not.toBeNull()
    for (const v of [null, {}, { configured: true, events: ['bogus'], gate_withdrawal: false }, { configured: true, events: [], gate_withdrawal: false, gate_alert_after_seconds: 1.5 }]) expect(parseTriggerPolicy(v)).toBeNull()
    expect(parseRequests({ items: [] })).toEqual([])
    expect(parseRequests({ items: [{ id: 1 }] })).toBeNull()
    expect(parseRequests([])).toBeNull()
  })
})
