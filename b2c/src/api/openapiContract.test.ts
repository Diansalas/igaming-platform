// UI-to-OpenAPI contract test for the B2C app (A10c; generalises the A6 responsible-gaming path pin that lived in
// rg.contract.test.ts). Every HTTP call every b2c api module makes must exist in docs/api/openapi/platform-api.yaml with
// the same method, and the JSON body fields / query keys the client sends must be documented there. The directory is
// scanned, so a new api module (or a new call in an existing one) is checked without touching this file.
// The checker is src/test/openapiContract.ts (a copy of the backoffice one; each app builds on its own).
import { readdirSync, readFileSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { B2C_API_DIR, checkCalls, extractCalls, formatFindings, loadOpenApi, matchSpecPath, type ClientCall } from '../test/openapiContract'

/**
 * Explicit, named exemptions. key = `<file> <METHOD> <normalised path> [body.field|query.name]`. Every entry needs a
 * reason and is verified to still be needed (a stale entry fails the suite), so this list can only shrink.
 * Empty on purpose: the 7 findings the first run produced (A10c) were fixed in the spec, none is waived.
 */
const ALLOWLIST: Record<string, string> = {}

/** Modules in api/ that legitimately contain no endpoint call. */
const NO_CALL_MODULES: Record<string, string> = {
  'client.ts': 'the transport itself (apiFetch/rawRefresh): not an endpoint module; its one endpoint is pinned by a dedicated test below',
  'types.ts': 'wire types only',
}

const sources = new Map<string, string>()
for (const f of readdirSync(B2C_API_DIR)) {
  if (f.endsWith('.ts') && !f.endsWith('.test.ts')) sources.set(f, readFileSync(join(B2C_API_DIR, f), 'utf8'))
}
const spec = loadOpenApi()

function callsOf(file: string): ClientCall[] {
  const siblings = [...sources.entries()].filter(([f]) => f !== file).map(([, s]) => s)
  return extractCalls(`api/${file}`, sources.get(file)!, { siblingSources: siblings })
}

describe('b2c api modules match docs/api/openapi/platform-api.yaml', () => {
  it('discovers the api modules', () => {
    expect(sources.size).toBeGreaterThanOrEqual(10)
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

  it('no source file outside src/api makes an HTTP call (so scanning src/api covers every b2c apiFetch)', () => {
    const SRC = resolve(B2C_API_DIR, '..')
    const offenders: string[] = []
    const walk = (dir: string) => {
      for (const e of readdirSync(dir, { withFileTypes: true })) {
        const p = join(dir, e.name)
        if (e.isDirectory()) walk(p)
        else if (/\.(ts|tsx)$/.test(e.name) && !/\.test\.tsx?$/.test(e.name) && !p.startsWith(B2C_API_DIR) && !p.startsWith(join(SRC, 'test'))) {
          const code = readFileSync(p, 'utf8').replace(/\/\*[\s\S]*?\*\/|\/\/[^\n]*/g, '')
          if (/\bapiFetch\s*[<(]|(?<![.\w])fetch\s*\(/.test(code)) offenders.push(relative(SRC, p))
        }
      }
    }
    walk(SRC)
    expect(offenders).toEqual([])
  })

  it('the responsible-gaming module still makes exactly the calls A6 pinned (guards the extractor on a known module)', () => {
    expect(
      callsOf('rg.ts')
        .map((c) => `${c.method} ${c.path}`)
        .sort(),
    ).toEqual(
      [
        'GET /v1/me/rg/status',
        'GET /v1/me/rg/options',
        'GET /v1/me/rg/limits',
        'POST /v1/me/rg/limits',
        'POST /v1/me/rg/limits/changes/{}/confirm',
        'POST /v1/me/rg/limits/changes/{}/cancel',
        'POST /v1/me/rg/time-out',
        'POST /v1/me/rg/self-exclusion',
      ].sort(),
    )
  })

  it('actually verifies bodies and query keys, not just paths (guards against the extractor going blind)', () => {
    const calls = [...sources.keys()].filter((f) => !(f in NO_CALL_MODULES)).flatMap(callsOf)
    expect(calls.length).toBeGreaterThanOrEqual(35)
    expect(calls.filter((c) => c.bodyKeys && c.bodyKeys.length > 0).length).toBeGreaterThanOrEqual(10)
    expect(calls.filter((c) => c.unresolved)).toEqual([])
  })

  it('the player money and wallet routes the A10b audit found missing are documented with the methods the app uses', () => {
    for (const [path, method] of [
      ['/v1/me/casino/games', 'get'],
      ['/v1/me/deposits', 'get'],
      ['/v1/me/deposits/{id}', 'get'],
      ['/v1/me/wallets', 'get'],
      ['/v1/me/wallets/{assetCode}', 'get'],
    ] as const) {
      expect(spec.paths[path]?.[method], `${method.toUpperCase()} ${path}`).toBeDefined()
    }
    for (const op of ['wager', 'win']) {
      const schema = spec.paths[`/v1/me/casino/sessions/{sessionID}/${op}`].post.requestBody.content['application/json'].schema
      expect(schema.required).toContain('idempotency_key')
      expect(Object.keys(schema.properties)).toContain('idempotency_key')
    }
  })

  it('every allowlist entry is still needed (stale entries must be deleted)', () => {
    const used = new Set<string>()
    for (const file of sources.keys()) {
      if (file in NO_CALL_MODULES) continue
      const { allowed } = checkCalls(spec, callsOf(file), { allowlist: ALLOWLIST })
      allowed.forEach((a) => used.add(a.key))
    }
    expect(Object.keys(ALLOWLIST).filter((k) => !used.has(k))).toEqual([])
  })
})

// The checker must itself be able to fail: these fixtures prove each class of drift is detected in b2c-shaped source
// (generic apiFetch<T> calls), so a green run above is not vacuous.
describe('contract checker self-test (b2c call shapes)', () => {
  const run = (src: string) => {
    const calls = extractCalls('api/fixture.ts', src)
    return { calls, ...checkCalls(spec, calls, {}) }
  }

  it('accepts a correct generic call and resolves a path parameter and a query helper', () => {
    const { calls, findings } = run("export const f = (id: string) => apiFetch<unknown>(`/v1/me/deposits/${id}`)")
    expect(calls[0]).toMatchObject({ method: 'GET', path: '/v1/me/deposits/{}' })
    expect(findings).toEqual([])
  })

  it('detects a path that is not in the spec', () => {
    expect(run("export const f = () => apiFetch<unknown>('/v1/me/does-not-exist')").findings[0].problem).toMatch(/path not in OpenAPI/)
  })

  it('detects a wrong method on an existing path', () => {
    expect(run("export const f = () => apiFetch<unknown>('/v1/me/wallets', { method: 'POST' })").findings[0].problem).toMatch(/method POST not defined/)
  })

  it('detects a body field the schema does not define (the wager idempotency_key class of drift)', () => {
    const f = run(
      "export const f = (id: string) => apiFetch<unknown>(`/v1/me/casino/sessions/${id}/wager`, { method: 'POST', body: JSON.stringify({ stake_amount: 1, bogus: 'x' }) })",
    ).findings
    expect(f.map((x) => x.key)).toEqual(['api/fixture.ts POST /v1/me/casino/sessions/{}/wager body.bogus'])
  })

  it('path matching: a literal client segment never matches a spec path parameter', () => {
    expect(matchSpecPath(spec, '/v1/me/wallets/{}')).toBe('/v1/me/wallets/{assetCode}')
    expect(matchSpecPath(spec, '/v1/me/wallets/nonsense/extra')).toBeUndefined()
  })
})
