import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { BACKOFFICE_API_DIR, checkCalls, extractCalls, formatFindings, loadOpenApi } from '../test/openapiContract'
import { classifyJurisdictionCreate } from './marketConfig'

const spec = loadOpenApi()
const src = readFileSync(join(BACKOFFICE_API_DIR, 'marketConfig.ts'), 'utf8')
const calls = extractCalls('api/marketConfig.ts', src)
const kindsGo = readFileSync(join(process.cwd(), '../internal/licensingchange/kinds.go'), 'utf8')

describe('api/marketConfig.ts against docs/api/openapi/platform-api.yaml', () => {
  it('calls exactly the filing route and the jurisdiction create route, each documented with the same method and body fields', () => {
    expect(calls.map((c) => `${c.method} ${c.path}`).sort()).toEqual(['POST /v1/admin/jurisdictions', 'POST /v1/admin/licensing-changes'])
    const { findings } = checkCalls(spec, calls, {})
    expect(findings, `\n${formatFindings(findings)}\n`).toEqual([])
    expect(calls.find((c) => c.path === '/v1/admin/licensing-changes')?.bodyKeys).toEqual(['kind', 'payload', 'reason_code'])
  })
  it('the three kinds exist in the OpenAPI vocabulary and are registered by the server under the same names', () => {
    const kinds: string[] = spec.components?.schemas?.LicensingChangeCreateRequest?.properties?.kind?.enum ?? []
    for (const k of ['licence_ceiling_version', 'operating_policy_version', 'tenant_jurisdiction_config_version']) {
      expect(kinds, k).toContain(k)
      expect(src, k).toContain(`'${k}'`)
    }
    expect(kindsGo.length).toBeGreaterThan(1000)
  })
  it('the payload keys the UI types declare equal the server payload structs (json tags in kinds.go)', () => {
    const tags = (name: string) => {
      const body = kindsGo.slice(kindsGo.indexOf(`type ${name} struct`)).split('\n}')[0]
      return [...body.matchAll(/`json:"([a-z_]+)"`/g)].map((m) => m[1]).sort()
    }
    const declared = (iface: string) => {
      const body = src.slice(src.indexOf(`export interface ${iface} {`)).split('\n}')[0]
      return [...body.matchAll(/^\s+([a-z_]+)\??:/gm)].map((m) => m[1]).sort()
    }
    expect(declared('CeilingPayload')).toEqual(tags('LicenceCeilingPayload'))
    expect(declared('PolicyPayload')).toEqual(tags('OperatingPolicyPayload'))
    expect(declared('TenantConfigPayload')).toEqual(tags('TenantJurisdictionConfigPayload'))
  })
  it('does no signing, hashing or proof handling in the browser, and never sends a client-chosen actor', () => {
    const code = src.split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')
    expect(code).not.toMatch(/crypto\.subtle|sha256|hmac|X-Actor-Proof|actorProof|signature|actor_id/i)
  })
})

describe('classifyJurisdictionCreate never fakes success', () => {
  it('only a Jurisdiction body is created', () => {
    expect(classifyJurisdictionCreate({ id: 'a1000000-0000-4000-8000-000000000001', code: 'X', name: 'n', created_at: 't' }).kind).toBe('created')
    for (const v of [null, undefined, {}, 'ok', [], { id: 'x', code: 'X' }, { id: 'a1000000-0000-4000-8000-000000000001' }, { id: 'a1000000-0000-4000-8000-000000000001', code: '' }]) expect(classifyJurisdictionCreate(v).kind).toBe('unconfirmed')
  })
})
