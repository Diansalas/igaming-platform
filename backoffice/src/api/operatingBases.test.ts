import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { BACKOFFICE_API_DIR, checkCalls, extractCalls, formatFindings, loadOpenApi } from '../test/openapiContract'
import { classifyBasisFiling } from './operatingBases'

const spec = loadOpenApi()
const src = readFileSync(join(BACKOFFICE_API_DIR, 'operatingBases.ts'), 'utf8')

// Pins that operatingBases.ts contains exactly these calls, so the generic contract test cannot pass vacuously for this module.
const EXPECTED = ['GET /v1/admin/operating-bases', 'GET /v1/admin/operating-bases/{}', 'POST /v1/admin/licensing-changes']

describe('api/operatingBases.ts against docs/api/openapi/platform-api.yaml', () => {
  const calls = extractCalls('api/operatingBases.ts', src)

  it('calls exactly the expected routes and each exists in the spec with the same method, body fields and query keys', () => {
    expect(calls.map((c) => `${c.method} ${c.path}`).sort()).toEqual([...EXPECTED].sort())
    const { findings } = checkCalls(spec, calls, {})
    expect(findings, `\n${formatFindings(findings)}\n`).toEqual([])
  })

  it('pins the methods, body fields and query keys (the check is not blind)', () => {
    const get = calls.find((c) => c.method === 'GET' && c.path === '/v1/admin/operating-bases')
    expect(get?.queryKeys).toEqual(expect.arrayContaining(['tenant_id', 'country_code', 'current', 'limit', 'offset']))
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.bodyKeys).toEqual(['kind', 'payload', 'reason_code'])
    expect(calls.some((c) => c.method !== 'GET' && c.method !== 'POST')).toBe(false)
  })

  it('the OpenAPI filing schema documents the three kinds this client files', () => {
    const kinds = spec.components?.schemas?.LicensingChangeCreateRequest?.properties?.kind?.enum as string[]
    for (const k of ['operating_basis_set', 'operating_basis_withdraw', 'operating_basis_review_record']) expect(kinds).toContain(k)
    expect(src).toContain("'operating_basis_set'")
  })

  it('does no hashing, signing or proof handling in the browser (the actor proof is server-side)', () => {
    expect(src.split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')).not.toMatch(/crypto\.subtle|sha256|hmac|X-Actor-Proof|actorProof|signature/i)
  })
})

describe('classifyBasisFiling', () => {
  const cr = (status: string) => ({ id: 'x', status })
  it('executed:false on an open request is pending; executed:true on an executed request is applied; everything else is unconfirmed', () => {
    expect(classifyBasisFiling({ change_request: cr('requested'), executed: false }).kind).toBe('pending')
    expect(classifyBasisFiling({ change_request: cr('executed'), executed: true }).kind).toBe('applied')
    expect(classifyBasisFiling({ change_request: cr('requested'), executed: true }).kind).toBe('unconfirmed')
    expect(classifyBasisFiling({ change_request: cr('executed'), executed: false }).kind).toBe('unconfirmed')
    expect(classifyBasisFiling({ change_request: cr('rejected'), executed: false }).kind).toBe('unconfirmed')
    expect(classifyBasisFiling({ change_request: cr('requested') }).kind).toBe('unconfirmed')
    expect(classifyBasisFiling({ change_request: {}, executed: false }).kind).toBe('unconfirmed')
    for (const v of [{}, null, undefined, 'ok', 1, []]) expect(classifyBasisFiling(v).kind).toBe('unconfirmed')
  })
})
