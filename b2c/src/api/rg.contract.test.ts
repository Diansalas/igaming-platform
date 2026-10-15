import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// Contract pin: every path the B2C responsible-gaming api module calls exists in
// docs/api/openapi/platform-api.yaml with the HTTP method the module uses. The module is read as source, so a
// new apiFetch call added to rg.ts without a spec entry fails here. (The spec is the Go server's contract; its
// own drift tests pin it against the routes.)

const SPEC = readFileSync(resolve(__dirname, '../../../docs/api/openapi/platform-api.yaml'), 'utf8')
const SOURCE = readFileSync(resolve(__dirname, './rg.ts'), 'utf8')

interface Call {
  path: string
  method: string
}

/** Extracts every apiFetch('<path>' | `<path>`, { method: 'X' }) call of the module. */
function apiFetchCalls(src: string): Call[] {
  const calls: Call[] = []
  const re = /apiFetch<[^>]*>\(\s*(['`])((?:\\.|(?!\1)[^\\])*)\1\s*(?:,\s*\{([^}]*)\})?/g
  for (const m of src.matchAll(re)) {
    const raw = m[2]
    const opts = m[3] ?? ''
    const method = /method:\s*'([A-Z]+)'/.exec(opts)?.[1] ?? 'GET'
    // drop the query suffix and turn template expressions into OpenAPI path parameters
    const path = raw.replace(/\$\{buildQuery\(\{[^}]*\}\)\}/g, '').replace(/\$\{[^}]*\}/g, '{param}')
    calls.push({ path, method })
  }
  return calls
}

/** The documented operations: path -> set of lower-case methods. */
function documented(spec: string): Map<string, Set<string>> {
  const out = new Map<string, Set<string>>()
  const lines = spec.split('\n')
  let current: string | null = null
  for (const line of lines) {
    const p = /^ {2}(\/v1\/\S*):\s*$/.exec(line)
    if (p) {
      current = p[1]
      out.set(current, new Set())
      continue
    }
    if (/^\S/.test(line) && !line.startsWith('#')) current = null
    const m = /^ {4}(get|post|put|patch|delete):\s*$/.exec(line)
    if (m && current) out.get(current)?.add(m[1])
  }
  return out
}

/** A path with {param} placeholders matches a documented path whose parameters are named differently. */
function findPath(docs: Map<string, Set<string>>, path: string): Set<string> | undefined {
  const shape = (p: string) => p.replace(/\{[^}]+\}/g, '{}')
  for (const [k, v] of docs) if (shape(k) === shape(path)) return v
  return undefined
}

describe('rg.ts API contract', () => {
  const calls = apiFetchCalls(SOURCE)
  const docs = documented(SPEC)

  it('finds the module’s calls (guards the extractor itself)', () => {
    expect(calls.map((c) => `${c.method} ${c.path}`).sort()).toEqual(
      [
        'GET /v1/me/rg/status',
        'GET /v1/me/rg/options',
        'GET /v1/me/rg/limits',
        'POST /v1/me/rg/limits',
        'POST /v1/me/rg/limits/changes/{param}/confirm',
        'POST /v1/me/rg/limits/changes/{param}/cancel',
        'POST /v1/me/rg/time-out',
        'POST /v1/me/rg/self-exclusion',
      ].sort(),
    )
  })

  it.each(calls.map((c) => [`${c.method} ${c.path}`, c] as const))('%s is documented in the OpenAPI spec', (_name, call) => {
    const methods = findPath(docs, call.path)
    expect(methods, `path ${call.path} is missing from docs/api/openapi/platform-api.yaml`).toBeDefined()
    expect(methods?.has(call.method.toLowerCase()), `${call.method} ${call.path} is not documented`).toBe(true)
  })

  it('the spec parser recognises a path it must not find', () => {
    expect(findPath(docs, '/v1/me/rg/does-not-exist')).toBeUndefined()
  })
})
