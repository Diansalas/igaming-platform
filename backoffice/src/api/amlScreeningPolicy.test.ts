import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { BACKOFFICE_API_DIR, checkCalls, extractCalls, formatFindings, loadOpenApi } from '../test/openapiContract'
import { SCREENING_LISTS } from './amlScreening'
import {
  BACKFILL_GRACE_MAX_SECONDS,
  RESCREEN_INTERVAL_FLOOR_SECONDS,
  RESCREEN_INTERVAL_MAX_SECONDS,
  classifySetScreeningPolicy,
  parseScreeningPolicy,
} from './amlScreeningPolicy'

const spec = loadOpenApi()
const src = readFileSync(join(BACKOFFICE_API_DIR, 'amlScreeningPolicy.ts'), 'utf8')
const calls = extractCalls('api/amlScreeningPolicy.ts', src)
const U = 'a1000000-0000-4000-8000-000000000001'

describe('api/amlScreeningPolicy.ts against docs/api/openapi/platform-api.yaml', () => {
  it('calls exactly the two lic29 routes, each in the spec with the same method, body fields and query keys', () => {
    expect(calls.map((c) => `${c.method} ${c.path}`).sort()).toEqual(['GET /v1/admin/tenants/{}/screening/policy', 'POST /v1/admin/tenants/{}/screening/policy'])
    const { findings } = checkCalls(spec, calls, {})
    expect(findings, `\n${formatFindings(findings)}\n`).toEqual([])
  })
  it('request pins: query key and every body key of AMLScreeningPolicyRequest (and no tenant, actor or proof)', () => {
    expect(calls.find((c) => c.method === 'GET')?.queryKeys).toEqual(['jurisdiction_id'])
    const keys = calls.find((c) => c.method === 'POST')?.bodyKeys ?? []
    expect([...keys].sort()).toEqual(['backfill_grace_seconds', 'effective_from', 'jurisdiction_id', 'legal_review_reference', 'lists', 'reason_code', 'rescreen_interval_seconds'])
    expect(spec.components?.schemas?.AMLScreeningPolicyRequest?.required.sort()).toEqual([...keys].sort())
  })
  it('the UI constants equal the spec', () => {
    const req = spec.components?.schemas?.AMLScreeningPolicyRequest?.properties
    expect(req?.rescreen_interval_seconds?.minimum).toBe(RESCREEN_INTERVAL_FLOOR_SECONDS)
    expect(req?.rescreen_interval_seconds?.maximum).toBe(RESCREEN_INTERVAL_MAX_SECONDS)
    expect(req?.backfill_grace_seconds?.maximum).toBe(BACKFILL_GRACE_MAX_SECONDS)
    expect(req?.backfill_grace_seconds?.minimum).toBe(0)
    expect(req?.lists?.items?.enum).toEqual([...SCREENING_LISTS])
  })
  it('path ids are encoded and nothing is signed or hashed', () => {
    expect(src.match(/\$\{(?!encodeURIComponent|buildQuery)[^}]*\}/g) ?? []).toEqual([])
    const code = src.split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')
    expect(code).not.toMatch(/crypto\.subtle|sha256|hmac|X-Actor-Proof|actorProof|signature/i)
    expect(code).not.toMatch(/parseFloat|toFixed|Number\s*\(/)
  })
})

describe('classifiers never fake success', () => {
  it('write', () => {
    expect(classifySetScreeningPolicy({ policy_id: U }).kind).toBe('confirmed')
    for (const v of [null, undefined, {}, 'ok', [], { policy_id: 'x' }, { policy_id: 5 }]) expect(classifySetScreeningPolicy(v).kind).toBe('unconfirmed')
  })
  it('read shape is checked, not trusted', () => {
    expect(parseScreeningPolicy({ configured: false, lists: [] })).toEqual({ configured: false, lists: [], rescreen_interval_seconds: null, backfill_grace_seconds: null, effective_from: null })
    expect(parseScreeningPolicy({ configured: true, lists: ['pep'], rescreen_interval_seconds: 3600, backfill_grace_seconds: 0, effective_from: '2026-01-01T00:00:00Z' })?.backfill_grace_seconds).toBe(0)
    for (const v of [null, {}, [], { configured: 'y', lists: [] }, { configured: true, lists: ['bogus'] }, { configured: true, lists: [], rescreen_interval_seconds: 1.5 }, { configured: true, lists: [], backfill_grace_seconds: -1 }, { configured: true, lists: [], effective_from: 5 }]) expect(parseScreeningPolicy(v)).toBeNull()
  })
})
