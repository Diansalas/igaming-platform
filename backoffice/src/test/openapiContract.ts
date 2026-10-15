// UI-to-OpenAPI contract checker (A10a). Statically extracts every HTTP call a client api module makes
// (apiFetch / fetch, plus thin local wrappers around them) and checks it against docs/api/openapi/platform-api.yaml:
//   1. the path exists (path parameters match path parameters, literal segments match literally);
//   2. the path has that HTTP method;
//   3. where cheap: every JSON request-body field name the client sends is a documented property of the request
//      schema, and every query-string key is a documented query parameter.
// Nothing is silently skipped: a call whose path cannot be resolved statically is itself reported, and the only
// way to exempt a finding is a named entry in an explicit allowlist (see openapiContract.test.ts).
//
// Shared helper: keep exports stable (A7's per-module harness, backoffice/src/api/aml.test.ts, may import it).
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parse } from 'yaml'

const HERE = dirname(fileURLToPath(import.meta.url))
export const OPENAPI_PATH = resolve(HERE, '../../../docs/api/openapi/platform-api.yaml')
export const BACKOFFICE_API_DIR = resolve(HERE, '../api')

/* eslint-disable @typescript-eslint/no-explicit-any */
type Json = any

export interface ClientCall {
  file: string
  line: number
  method: string
  /** Normalised: path parameters are `{}`, no query string. */
  path: string
  /** Top-level JSON body keys the client sends; undefined when they cannot be determined statically. */
  bodyKeys?: string[]
  /** Query keys the client sends; undefined when they cannot be determined statically. */
  queryKeys?: string[]
  /** Set when the path could not be resolved to a literal; the call is reported as a finding. */
  unresolved?: string
}

export interface Finding {
  file: string
  line: number
  call: string
  problem: string
  /** Stable key for allowlisting: `file METHOD path [field]`. */
  key: string
}

export interface OpenApi {
  paths: Record<string, Json>
  components?: Json
}

let cachedSpec: OpenApi | null = null
export function loadOpenApi(path: string = OPENAPI_PATH): OpenApi {
  if (path === OPENAPI_PATH && cachedSpec) return cachedSpec
  const spec = parse(readFileSync(path, 'utf8')) as OpenApi
  if (path === OPENAPI_PATH) cachedSpec = spec
  return spec
}

// ---------------------------------------------------------------------------------------------------------------
// Source scanning
// ---------------------------------------------------------------------------------------------------------------

/** Blanks out comments (keeping offsets and newlines) so commented-out calls and prose are never extracted. */
export function stripComments(src: string): string {
  let out = ''
  let i = 0
  while (i < src.length) {
    const c = src[i]
    const n = src[i + 1]
    if (c === '/' && n === '/') {
      while (i < src.length && src[i] !== '\n') {
        out += ' '
        i++
      }
    } else if (c === '/' && n === '*') {
      while (i < src.length && !(src[i] === '*' && src[i + 1] === '/')) {
        out += src[i] === '\n' ? '\n' : ' '
        i++
      }
      out += '  '
      i += 2
    } else if (c === "'" || c === '"' || c === '`') {
      // copy the literal verbatim (a `//` inside a string is not a comment)
      const q = c
      out += c
      i++
      let depth = 0
      while (i < src.length) {
        const d = src[i]
        out += d
        if (d === '\\') {
          out += src[i + 1] ?? ''
          i += 2
          continue
        }
        if (q === '`' && d === '$' && src[i + 1] === '{') {
          depth++
          out += '{'
          i += 2
          continue
        }
        if (q === '`' && d === '}' && depth > 0) depth--
        i++
        if (d === q && depth === 0) break
      }
    } else {
      out += c
      i++
    }
  }
  return out
}

function lineOf(src: string, offset: number): number {
  let n = 1
  for (let i = 0; i < offset; i++) if (src[i] === '\n') n++
  return n
}

/** Index just after the string/template literal starting at `i` (which must be a quote). */
function skipLiteral(src: string, i: number): number {
  const q = src[i]
  i++
  let depth = 0
  while (i < src.length) {
    const d = src[i]
    if (d === '\\') {
      i += 2
      continue
    }
    if (q === '`' && d === '$' && src[i + 1] === '{') {
      depth++
      i += 2
      continue
    }
    if (q === '`' && d === '}' && depth > 0) {
      depth--
      i++
      continue
    }
    if (d === q && depth === 0) return i + 1
    i++
  }
  return i
}

/** Splits the text between the parens that open at `open` into top-level comma separated arguments. */
export function readArgs(src: string, open: number): { args: string[]; end: number } {
  const args: string[] = []
  let depth = 0
  let start = open + 1
  let i = open + 1
  while (i < src.length) {
    const c = src[i]
    if (c === "'" || c === '"' || c === '`') {
      i = skipLiteral(src, i)
      continue
    }
    if (c === '(' || c === '[' || c === '{') depth++
    else if (c === ')' || c === ']' || c === '}') {
      if (depth === 0) {
        const last = src.slice(start, i).trim()
        if (last) args.push(last)
        return { args, end: i }
      }
      depth--
    } else if (c === ',' && depth === 0) {
      args.push(src.slice(start, i).trim())
      start = i + 1
    } else if (c === '<' && depth === 0 && /[\w]/.test(src[i - 1] ?? '')) {
      // generic argument list (apiFetch<T>(...)): not at call-argument level here, skip to the matching '>'
    }
    i++
  }
  return { args, end: i }
}

/** Top-level `key` names of an object literal or an interface/type body `{ ... }` (spreads and index signatures skipped). */
export function topLevelKeys(objectText: string): string[] {
  const t = objectText.trim()
  if (!t.startsWith('{')) return []
  const keys: string[] = []
  let depth = 0
  let i = 0
  let atKeyStart = false
  while (i < t.length) {
    const c = t[i]
    if (c === "'" || c === '"' || c === '`') {
      const j = skipLiteral(t, i)
      if (depth === 1 && atKeyStart) {
        const m = /^\s*\??\s*[:,}]/.exec(t.slice(j))
        if (m) keys.push(t.slice(i + 1, j - 1))
        atKeyStart = false
      }
      i = j
      continue
    }
    if (c === '{' || c === '(' || c === '[') {
      depth++
      if (depth === 1) atKeyStart = true
      i++
      continue
    }
    if (c === '}' || c === ')' || c === ']') {
      depth--
      i++
      continue
    }
    if (depth === 1) {
      if (c === ',' || c === ';' || c === '\n') {
        atKeyStart = true
        i++
        continue
      }
      if (atKeyStart) {
        if (/\s/.test(c)) {
          i++
          continue
        }
        if (t.startsWith('...', i)) {
          atKeyStart = false
          i += 3
          continue
        }
        const m = /^(readonly\s+)?([A-Za-z_$][\w$]*)\s*(\?)?\s*([:,;}\n]|$)/.exec(t.slice(i))
        if (m) {
          keys.push(m[2])
          i += m[0].length - (m[4] === '}' ? 1 : 0)
          atKeyStart = m[4] === ',' || m[4] === ';' || m[4] === '\n'
          continue
        }
        atKeyStart = false
      }
    }
    i++
  }
  return keys
}

/** `{ ... }` body of `interface Name` or `type Name = {` found in any of the given sources. */
function findTypeBody(name: string, sources: string[]): string | undefined {
  for (const src of sources) {
    const re = new RegExp(`(?:interface\\s+${name}\\b[^{]*|type\\s+${name}\\s*=\\s*)\\{`)
    const m = re.exec(src)
    if (!m) continue
    const open = m.index + m[0].length - 1
    let depth = 0
    for (let i = open; i < src.length; i++) {
      if (src[i] === '{') depth++
      else if (src[i] === '}') {
        depth--
        if (depth === 0) return src.slice(open, i + 1)
      }
    }
  }
  return undefined
}

const HELPER_RE = /(?:const|let)\s+(\w+)\s*=\s*\(\s*\w+\s*:\s*string\s*\)\s*(?::\s*string\s*)?=>\s*([`'"])/g

function collectHelperTemplates(src: string): Map<string, string> {
  const map = new Map<string, string>()
  for (const m of src.matchAll(HELPER_RE)) {
    const litStart = m.index! + m[0].length - 1
    const litEnd = skipLiteral(src, litStart)
    map.set(m[1], src.slice(litStart + 1, litEnd - 1))
  }
  return map
}

function normaliseTemplate(inner: string, helpers: Map<string, string>, depth = 0): { path: string; query?: string } {
  let queryArg: string | undefined
  let out = ''
  let i = 0
  while (i < inner.length) {
    if (inner[i] === '$' && inner[i + 1] === '{') {
      let d = 1
      let j = i + 2
      while (j < inner.length && d > 0) {
        if (inner[j] === '{') d++
        else if (inner[j] === '}') d--
        j++
      }
      const expr = inner.slice(i + 2, j - 1).trim()
      i = j
      const call = /^(\w+)\s*\(/.exec(expr)
      if (/^import\.meta\.env/.test(expr)) {
        // API base prefix of a raw fetch(): not part of the path
      } else if (call?.[1] === 'buildQuery') {
        const { args } = readArgs(expr, expr.indexOf('('))
        queryArg = args[0]
      } else if (call && helpers.has(call[1]) && depth < 4) {
        out += normaliseTemplate(helpers.get(call[1])!, helpers, depth + 1).path
      } else {
        out += '{}'
      }
      continue
    }
    out += inner[i]
    i++
  }
  // `${API_BASE}/v1/...` (a base-URL constant) is not part of the API path.
  if (out.startsWith('{}/')) out = out.slice(2)
  const q = out.indexOf('?')
  return { path: q >= 0 ? out.slice(0, q) : out, query: queryArg }
}

function literalContent(arg: string): string | undefined {
  const t = arg.trim()
  if (t.length < 2) return undefined
  const q = t[0]
  if ((q === '`' || q === "'" || q === '"') && skipLiteral(t, 0) === t.length) return t.slice(1, -1)
  return undefined
}

interface EnclosingFn {
  name: string
  params: Map<string, string>
  start: number
  bodyStart: number
}

function enclosingFunction(src: string, offset: number): EnclosingFn | undefined {
  const re = /(?:export\s+)?(?:async\s+)?function\s+(\w+)\s*(?:<[^>]*>)?\s*\(/g
  let found: EnclosingFn | undefined
  for (const m of src.matchAll(re)) {
    if (m.index! > offset) break
    const open = m.index! + m[0].length - 1
    const { args, end } = readArgs(src, open)
    const params = new Map<string, string>()
    for (const a of args) {
      const pm = /^(?:\.\.\.)?(\w+)\s*\??\s*:\s*(\{|[A-Za-z_][\w.]*)/.exec(a)
      if (pm && pm[2] === '{') {
        // inline object type: keep the balanced `{ ... }` text itself as the "type name"
        const open = a.indexOf('{', pm[0].length - 1)
        let depth = 0
        let close = open
        for (; close < a.length; close++) {
          if (a[close] === '{') depth++
          else if (a[close] === '}' && --depth === 0) break
        }
        params.set(pm[1], a.slice(open, close + 1))
      } else if (pm) params.set(pm[1], pm[2])
      else {
        const bare = /^(\w+)/.exec(a)
        if (bare) params.set(bare[1], '')
      }
    }
    found = { name: m[1], params, start: m.index!, bodyStart: end }
  }
  return found
}

export interface ExtractOptions {
  /** Other module sources, used only to find an imported request/params interface by name. */
  siblingSources?: string[]
}

/** Extracts every client HTTP call in one module's source. */
export function extractCalls(file: string, rawSource: string, opts: ExtractOptions = {}): ClientCall[] {
  const src = stripComments(rawSource)
  const helpers = collectHelperTemplates(src)
  const typeSources = [src, ...(opts.siblingSources ?? []).map(stripComments)]
  const calls: ClientCall[] = []
  const wrappers = new Map<string, string>() // wrapper function name -> HTTP method

  const emit = (offset: number, pathExprs: string[], method: string, options: string, bodyArg: string | undefined, fn?: EnclosingFn) => {
    const line = lineOf(src, offset)
    for (const expr of pathExprs) {
      const norm = normaliseTemplate(expr, helpers)
      const call: ClientCall = { file, line, method, path: norm.path }
      call.bodyKeys = bodyKeysOf(bodyArg ?? bodyFromOptions(options), fn, typeSources)
      call.queryKeys = norm.query === undefined ? undefined : queryKeysOf(norm.query, fn, typeSources)
      if (norm.query === undefined) call.queryKeys = []
      calls.push(call)
    }
  }

  const callRe = /\b(apiFetch|fetch)\s*(?:<[^()]*>)?\s*\(/g
  for (const m of src.matchAll(callRe)) {
    const open = m.index! + m[0].length - 1
    // skip the definition of apiFetch itself / member access like res.fetch
    if (/function\s+$/.test(src.slice(Math.max(0, m.index! - 12), m.index!))) continue
    if (src[m.index! - 1] === '.') continue
    const { args } = readArgs(src, open)
    if (args.length === 0) continue
    const first = args[0]
    const options = args.slice(1).join(',')
    const method = (/method\s*:\s*['"](\w+)['"]/.exec(options)?.[1] ?? 'GET').toUpperCase()
    const fn = enclosingFunction(src, m.index!)
    const lit = literalContent(first)
    if (lit !== undefined) {
      emit(m.index!, [lit], method, options, undefined, fn)
      continue
    }
    if (/^\w+$/.test(first)) {
      if (fn?.params.has(first)) {
        // `function post(path, body) { return apiFetch(path, { method: 'POST' ... }) }`: a wrapper; resolve at its call sites
        wrappers.set(fn.name, method)
        continue
      }
      // `const path = cond ? `a` : `b``: every literal in the declaration is a candidate
      const decl = new RegExp(`(?:const|let)\\s+${first}\\s*=\\s*([^\\n]*)`).exec(src.slice(0, m.index!).split('\n').slice(-6).join('\n'))
      if (decl) {
        const lits: string[] = []
        const t = decl[1]
        for (let i = 0; i < t.length; ) {
          if (t[i] === "'" || t[i] === '"' || t[i] === '`') {
            const j = skipLiteral(t, i)
            lits.push(t.slice(i + 1, j - 1))
            i = j
          } else i++
        }
        if (lits.length > 0) {
          emit(m.index!, lits, method, options, undefined, fn)
          continue
        }
      }
    }
    calls.push({ file, line: lineOf(src, m.index!), method, path: '', unresolved: `cannot resolve path expression \`${first.slice(0, 80)}\`` })
  }

  // Call sites of wrappers: `postAnonymous<T>('/v1/x', body)`.
  for (const [name, method] of wrappers) {
    const re = new RegExp(`\\b${name}\\s*(?:<[^()]*>)?\\s*\\(`, 'g')
    for (const m of src.matchAll(re)) {
      if (/function\s+$/.test(src.slice(Math.max(0, m.index! - 12), m.index!))) continue
      const open = m.index! + m[0].length - 1
      const { args } = readArgs(src, open)
      const lit = args[0] !== undefined ? literalContent(args[0]) : undefined
      const fn = enclosingFunction(src, m.index!)
      if (lit === undefined) {
        calls.push({ file, line: lineOf(src, m.index!), method, path: '', unresolved: `wrapper ${name}: cannot resolve path \`${(args[0] ?? '').slice(0, 80)}\`` })
        continue
      }
      emit(m.index!, [lit], method, '', args[1], fn)
    }
  }
  return calls.sort((a, b) => a.line - b.line)
}

function bodyFromOptions(options: string): string | undefined {
  const m = /body\s*:\s*JSON\.stringify\s*\(/.exec(options)
  if (!m) return undefined
  const { args } = readArgs(options, m.index + m[0].length - 1)
  return args[0]
}

function bodyKeysOf(arg: string | undefined, fn: EnclosingFn | undefined, typeSources: string[]): string[] | undefined {
  if (arg === undefined) return undefined
  const t = arg.trim()
  if (t.startsWith('{')) return topLevelKeys(t)
  if (/^\w+$/.test(t) && fn) return keysOfNamedType(fn.params.get(t), typeSources)
  return undefined
}

function queryKeysOf(arg: string, fn: EnclosingFn | undefined, typeSources: string[]): string[] | undefined {
  const t = arg.trim()
  if (t.startsWith('{')) return topLevelKeys(t)
  if (/^\w+$/.test(t) && fn) return keysOfNamedType(fn.params.get(t), typeSources)
  return undefined
}

function keysOfNamedType(typeName: string | undefined, typeSources: string[]): string[] | undefined {
  if (!typeName) return undefined
  if (typeName.startsWith('{')) return topLevelKeys(typeName)
  const body = findTypeBody(typeName, typeSources)
  return body ? topLevelKeys(body) : undefined
}

// ---------------------------------------------------------------------------------------------------------------
// OpenAPI side
// ---------------------------------------------------------------------------------------------------------------

function resolveRef(spec: OpenApi, node: Json): Json {
  let cur = node
  for (let i = 0; i < 20 && cur && typeof cur === 'object' && typeof cur.$ref === 'string'; i++) {
    const ref = cur.$ref as string
    if (!ref.startsWith('#/')) return undefined
    cur = ref
      .slice(2)
      .split('/')
      .reduce((acc: Json, seg) => (acc ? acc[seg.replace(/~1/g, '/').replace(/~0/g, '~')] : undefined), spec)
  }
  return cur
}

/** Union of property names across a schema, following $ref / allOf / oneOf / anyOf. `open` = unknown extra keys allowed. */
function schemaProperties(spec: OpenApi, schema: Json, depth = 0): { props: Set<string>; known: boolean } {
  const props = new Set<string>()
  let known = false
  const s = resolveRef(spec, schema)
  if (!s || depth > 8) return { props, known }
  if (s.properties && typeof s.properties === 'object') {
    known = true
    for (const k of Object.keys(s.properties)) props.add(k)
  }
  for (const key of ['allOf', 'oneOf', 'anyOf'] as const) {
    if (Array.isArray(s[key])) {
      for (const sub of s[key]) {
        const r = schemaProperties(spec, sub, depth + 1)
        if (r.known) known = true
        r.props.forEach((p) => props.add(p))
      }
    }
  }
  return { props, known }
}

const SEG_PARAM = /^\{[^}]*\}$/

/** Finds the spec path item for a normalised client path (`{}` segments are path parameters). */
export function matchSpecPath(spec: OpenApi, clientPath: string): string | undefined {
  const cs = clientPath.replace(/\/+$/, '').split('/')
  let best: string | undefined
  for (const sp of Object.keys(spec.paths)) {
    const ss = sp.replace(/\/+$/, '').split('/')
    if (ss.length !== cs.length) continue
    let ok = true
    for (let i = 0; i < cs.length && ok; i++) {
      const c = cs[i]
      const s = ss[i]
      if (c === '{}' || /^\{.*\}$/.test(c)) ok = SEG_PARAM.test(s)
      else ok = c === s // a literal client segment only ever matches the same literal
    }
    if (ok) {
      // prefer the most literal spec path when several match
      if (!best || (best.match(/\{/g)?.length ?? 0) > (sp.match(/\{/g)?.length ?? 0)) best = sp
    }
  }
  return best
}

function queryParamNames(spec: OpenApi, item: Json, op: Json): Set<string> {
  const names = new Set<string>()
  for (const list of [item.parameters, op.parameters]) {
    if (!Array.isArray(list)) continue
    for (const p of list) {
      const r = resolveRef(spec, p)
      if (r?.in === 'query' && typeof r.name === 'string') names.add(r.name)
    }
  }
  return names
}

export interface CheckOptions {
  /** Named, reasoned exemptions: key = Finding.key. */
  allowlist?: Record<string, string>
}

export function describeCall(c: ClientCall): string {
  return `${c.method} ${c.path || '<unresolved>'}`
}

/** Checks the calls against the spec; returns one Finding per problem (allowlisted ones are returned in `allowed`). */
export function checkCalls(spec: OpenApi, calls: ClientCall[], opts: CheckOptions = {}): { findings: Finding[]; allowed: Finding[] } {
  const findings: Finding[] = []
  const allowed: Finding[] = []
  const push = (c: ClientCall, problem: string, field?: string) => {
    const key = `${c.file} ${c.method} ${c.path}${field ? ` ${field}` : ''}`
    const f: Finding = { file: c.file, line: c.line, call: describeCall(c), problem, key }
    if (opts.allowlist && key in opts.allowlist) allowed.push(f)
    else findings.push(f)
  }
  for (const c of calls) {
    if (c.unresolved) {
      push(c, c.unresolved)
      continue
    }
    const specPath = matchSpecPath(spec, c.path)
    if (!specPath) {
      push(c, `path not in OpenAPI`)
      continue
    }
    const item = spec.paths[specPath]
    const op = item[c.method.toLowerCase()]
    if (!op) {
      const have = ['get', 'post', 'put', 'patch', 'delete'].filter((m) => item[m]).map((m) => m.toUpperCase())
      push(c, `method ${c.method} not defined for ${specPath} (spec has: ${have.join(', ') || 'none'})`)
      continue
    }
    if (c.bodyKeys && c.bodyKeys.length > 0) {
      const schema = resolveRef(spec, op.requestBody)?.content?.['application/json']?.schema
      if (!schema) {
        push(c, `client sends a JSON body (${c.bodyKeys.join(', ')}) but ${c.method} ${specPath} documents no JSON request body`, 'body')
      } else {
        const { props, known } = schemaProperties(spec, schema)
        if (known) {
          for (const k of c.bodyKeys) if (!props.has(k)) push(c, `body field "${k}" is not a property of the request schema of ${specPath}`, `body.${k}`)
        }
      }
    }
    if (c.queryKeys && c.queryKeys.length > 0) {
      const names = queryParamNames(spec, item, op)
      for (const k of c.queryKeys) if (!names.has(k)) push(c, `query parameter "${k}" is not documented on ${c.method} ${specPath}`, `query.${k}`)
    }
  }
  return { findings, allowed }
}

export function formatFindings(findings: Finding[]): string {
  return findings.map((f) => `  ${f.file}:${f.line}  ${f.call}  -> ${f.problem}\n      allowlist key: ${f.key}`).join('\n')
}
