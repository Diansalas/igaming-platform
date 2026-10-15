// Generic UI-to-OpenAPI contract test (A10a): every HTTP call every backoffice api module makes must exist in
// docs/api/openapi/platform-api.yaml with the same method, and the JSON body fields / query keys the client sends
// must be documented there. A new api module is picked up automatically (the directory is scanned).
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  BACKOFFICE_API_DIR,
  checkCalls,
  extractCalls,
  formatFindings,
  loadOpenApi,
  matchSpecPath,
  type ClientCall,
} from '../test/openapiContract'

/**
 * Explicit, named exemptions. key = `<file> <METHOD> <normalised path> [body.field|query.name]`. Every entry needs a
 * reason and is verified to still be needed (a stale entry fails the suite), so this list can only shrink.
 */
const ALLOWLIST: Record<string, string> = {}

/** Modules in api/ that legitimately contain no endpoint call. */
const NO_CALL_MODULES: Record<string, string> = {
  'client.ts': 'the transport itself (apiFetch/rawRefresh): not an endpoint module; its one endpoint is pinned by a dedicated test below',
  'types.ts': 'wire types only',
}

const sources = new Map<string, string>()
for (const f of readdirSync(BACKOFFICE_API_DIR)) {
  if (f.endsWith('.ts') && !f.endsWith('.test.ts')) sources.set(f, readFileSync(join(BACKOFFICE_API_DIR, f), 'utf8'))
}
const spec = loadOpenApi()

function callsOf(file: string): ClientCall[] {
  const siblings = [...sources.entries()].filter(([f]) => f !== file).map(([, s]) => s)
  return extractCalls(`api/${file}`, sources.get(file)!, { siblingSources: siblings })
}

describe('backoffice api modules match docs/api/openapi/platform-api.yaml', () => {
  it('discovers the api modules', () => {
    expect(sources.size).toBeGreaterThan(10)
  })

  describe.each([...sources.keys()].filter((f) => !(f in NO_CALL_MODULES)).sort())('%s', (file) => {
    it('every call exists in the spec with the same method, body fields and query keys', () => {
      const calls = callsOf(file)
      expect(calls.length, `${file} contains no extractable HTTP call; add it to NO_CALL_MODULES with a reason or fix the extractor`).toBeGreaterThan(0)
      const { findings } = checkCalls(spec, calls, { allowlist: ALLOWLIST })
      expect(findings, `\n${formatFindings(findings)}\n`).toEqual([])
    })
  })

  it('client.ts rawRefresh endpoint is in the spec (the transport is not statically extractable, so pinned here)', () => {
    expect(sources.get('client.ts')).toContain('`${API_BASE}/v1/auth/refresh`')
    expect(spec.paths['/v1/auth/refresh']?.post).toBeDefined()
  })

  it('actually verifies bodies and query keys, not just paths (guards against the extractor going blind)', () => {
    const calls = [...sources.keys()].filter((f) => !(f in NO_CALL_MODULES)).flatMap(callsOf)
    expect(calls.length).toBeGreaterThanOrEqual(60)
    expect(calls.filter((c) => c.bodyKeys && c.bodyKeys.length > 0).length).toBeGreaterThanOrEqual(25)
    expect(calls.filter((c) => c.queryKeys && c.queryKeys.length > 0).length).toBeGreaterThanOrEqual(15)
    expect(calls.filter((c) => c.unresolved)).toEqual([])
  })

  it('every allowlist entry is still needed (stale entries must be deleted)', () => {
    const used = new Set<string>()
    for (const file of sources.keys()) {
      const { allowed } = checkCalls(spec, callsOf(file), { allowlist: ALLOWLIST })
      allowed.forEach((a) => used.add(a.key))
    }
    expect(Object.keys(ALLOWLIST).filter((k) => !used.has(k))).toEqual([])
    for (const [k, reason] of Object.entries(ALLOWLIST)) expect(reason.trim().length, k).toBeGreaterThan(10)
  })

  it('the removed admin-password staff-create route is not called by any module', () => {
    const all = [...sources.keys()].filter((f) => !(f in NO_CALL_MODULES)).flatMap(callsOf)
    expect(all.filter((c) => c.method === 'POST' && /\/staff$/.test(c.path))).toEqual([])
  })
})

// The checker must itself be able to fail: these fixtures prove each class of drift is detected, so a green run above
// is not vacuous.
describe('contract checker self-test', () => {
  const run = (src: string, allowlist?: Record<string, string>) => {
    const calls = extractCalls('api/fixture.ts', src)
    return { calls, ...checkCalls(spec, calls, { allowlist }) }
  }

  it('accepts a correct call, resolving path parameters and a query helper', () => {
    const { calls, findings } = run("export const f = (id: string) => apiFetch(`/v1/admin/tenants/${id}/brands${buildQuery({ limit: 1 })}`)")
    expect(calls[0]).toMatchObject({ method: 'GET', path: '/v1/admin/tenants/{}/brands', queryKeys: ['limit'] })
    expect(findings).toEqual([])
  })

  it('detects a path that is not in the spec', () => {
    expect(run("export const f = () => apiFetch('/v1/admin/does-not-exist')").findings[0].problem).toMatch(/path not in OpenAPI/)
  })

  it('detects a wrong method on an existing path', () => {
    const f = run("export const f = (id: string) => apiFetch(`/v1/admin/tenants/${id}/staff`, { method: 'POST', body: JSON.stringify({}) })").findings
    expect(f[0].problem).toMatch(/method POST not defined/)
  })

  it('detects a body field the schema does not define', () => {
    const f = run(
      "export const f = (id: string) => apiFetch(`/v1/admin/tenants/${id}/staff/invitations`, { method: 'POST', body: JSON.stringify({ email: 'a', password: 'b' }) })",
    ).findings
    expect(f.map((x) => x.key)).toEqual(['api/fixture.ts POST /v1/admin/tenants/{}/staff/invitations body.password'])
  })

  it('detects an undocumented query key, taken from a params interface', () => {
    const f = run(
      "interface P { limit?: number; bogus?: string }\nexport function f(p: P) { return apiFetch(`/v1/admin/tenants${buildQuery(p)}`) }",
    ).findings
    expect(f.map((x) => x.key)).toEqual(['api/fixture.ts GET /v1/admin/tenants query.bogus'])
  })

  it('reports (never silently skips) a path it cannot resolve', () => {
    expect(run('export const f = (p: string) => apiFetch(compute(p))').findings[0].problem).toMatch(/cannot resolve/)
  })

  it('resolves a thin wrapper through its call sites', () => {
    const { calls, findings } = run(
      "function post<T>(path: string, body: unknown) { return apiFetch<T>(path, { method: 'POST', body: JSON.stringify(body) }) }\nexport const f = () => post('/v1/staff/invitations/accept', { token: 't', password: 'p' })",
    )
    expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual(['POST /v1/staff/invitations/accept'])
    expect(findings).toEqual([])
  })

  it('an allowlisted finding is moved out of findings but still reported as used', () => {
    const key = 'api/fixture.ts GET /v1/admin/does-not-exist'
    const { findings, allowed } = run("export const f = () => apiFetch('/v1/admin/does-not-exist')", { [key]: 'reason text here' })
    expect(findings).toEqual([])
    expect(allowed.map((a) => a.key)).toEqual([key])
  })

  it('path matching: a literal client segment never matches a spec path parameter', () => {
    expect(matchSpecPath(spec, '/v1/admin/tenants/{}/staff/invitations')).toBe('/v1/admin/tenants/{tenantID}/staff/invitations')
    expect(matchSpecPath(spec, '/v1/admin/tenants/{}/staff/nonsense-literal')).toBeUndefined()
  })
})
