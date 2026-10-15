// Minimal, dependency-free reader of docs/api/openapi/platform-api.yaml for the per-module UI-to-OpenAPI contract
// tests (A10b). It is NOT a YAML parser: it understands exactly the layout this repository's spec uses (2-space
// indented paths, 4-space operations, 4-space component schemas, flow or block `enum`, flow `required`). It is
// deliberately independent of any shared helper; the orchestrator reconciles at merge.
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const SPEC = readFileSync(resolve(process.cwd(), '../docs/api/openapi/platform-api.yaml'), 'utf8').split('\n')

const indentOf = (l: string) => l.length - l.trimStart().length

function blockAfter(start: number, indent: number): string[] {
  const out: string[] = []
  for (let i = start + 1; i < SPEC.length; i++) {
    const l = SPEC[i]
    if (l.trim() === '') {
      out.push(l)
      continue
    }
    if (indentOf(l) <= indent) break
    out.push(l)
  }
  return out
}

/** Every `  /v1/...:` path template in the spec. */
export function specPaths(): string[] {
  return SPEC.filter((l) => /^ {2}\/\S+:\s*$/.test(l)).map((l) => l.trim().slice(0, -1))
}

function pathBlock(path: string): string[] {
  const i = SPEC.findIndex((l) => l === `  ${path}:`)
  return i < 0 ? [] : blockAfter(i, 2)
}

function opBlock(path: string, method: string): string[] {
  const block = pathBlock(path)
  const i = block.findIndex((l) => l === `    ${method.toLowerCase()}:`)
  return i < 0 ? [] : blockAfter0(block, i, 4)
}

function blockAfter0(lines: string[], start: number, indent: number): string[] {
  const out: string[] = []
  for (let i = start + 1; i < lines.length; i++) {
    const l = lines[i]
    if (l.trim() !== '' && indentOf(l) <= indent) break
    out.push(l)
  }
  return out
}

export function hasOperation(path: string, method: string): boolean {
  return opBlock(path, method).length > 0
}

/** Query parameter names the operation declares. */
export function queryParams(path: string, method: string): string[] {
  const op = opBlock(path, method)
  const names: string[] = []
  op.forEach((l, i) => {
    const flow = /^\s*- \{ name: (\w+), in: (\w+)/.exec(l)
    if (flow) {
      if (flow[2] === 'query') names.push(flow[1])
      return
    }
    const m = /^\s*- name: (\w+)\s*$/.exec(l)
    if (m && /^\s*in: query\s*$/.test(op[i + 1] ?? '')) names.push(m[1])
  })
  return names
}

/** The schema name the operation's JSON request body references, or null when it has none. */
export function requestBodySchema(path: string, method: string): string | null {
  const op = opBlock(path, method)
  const i = op.findIndex((l) => /^\s*requestBody:/.test(l))
  if (i < 0) return null
  for (let j = i + 1; j < op.length; j++) {
    const m = /\$ref: "#\/components\/schemas\/(\w+)"/.exec(op[j])
    if (m) return m[1]
    if (/^\s*responses:/.test(op[j])) break
  }
  return null
}

function schemaBlock(name: string): string[] {
  const root = SPEC.findIndex((l) => l === '  schemas:')
  for (let i = root + 1; i < SPEC.length; i++) {
    if (SPEC[i] === `    ${name}:`) return blockAfter(i, 4)
    if (/^ {2}\S/.test(SPEC[i]) && i > root) break
  }
  return []
}

export function schemaProps(name: string): string[] {
  const b = schemaBlock(name)
  const i = b.findIndex((l) => l === '      properties:')
  if (i < 0) return []
  return blockAfter0(b, i, 6)
    .filter((l) => indentOf(l) === 8 && /^\s+\w+:/.test(l))
    .map((l) => l.trim().split(':')[0])
}

export function schemaRequired(name: string): string[] {
  const b = schemaBlock(name)
  const flow = b.find((l) => /^ {6}required: \[/.test(l))
  if (flow) {
    return flow
      .slice(flow.indexOf('[') + 1, flow.lastIndexOf(']'))
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean)
  }
  return []
}

export function schemaForbidsExtra(name: string): boolean {
  return schemaBlock(name).some((l) => /^ {6}additionalProperties: false/.test(l))
}

/** Enum values of a named schema (flow or block list) or of one property of it. */
export function schemaEnum(name: string, prop?: string): string[] {
  let b = schemaBlock(name)
  if (prop) {
    const i = b.findIndex((l) => l === `        ${prop}:`)
    if (i >= 0) b = blockAfter0(b, i, 8)
    else {
      // single-line property: `prop: { type: string, enum: [a, b] }`
      const one = b.find((l) => new RegExp(`^ {8}${prop}:.*enum: \\[`).test(l))
      return one ? flowList(one) : []
    }
  }
  const flow = b.find((l) => /enum: \[/.test(l))
  if (flow) return flowList(flow)
  const i = b.findIndex((l) => /^\s*enum:\s*$/.test(l))
  if (i < 0) return []
  const out: string[] = []
  for (let j = i + 1; j < b.length; j++) {
    const m = /^\s*- "?([^"\s]+)"?\s*$/.exec(b[j])
    if (!m) break
    out.push(m[1])
  }
  return out
}

function flowList(line: string): string[] {
  const s = line.slice(line.indexOf('enum: [') + 7)
  return s
    .slice(0, s.indexOf(']'))
    .split(',')
    .map((x) => x.trim().replace(/^"|"$/g, ''))
    .filter(Boolean)
}

function templateToRegex(t: string): RegExp {
  return new RegExp('^' + t.replace(/\{[^}]+\}/g, '[^/]+') + '$')
}

export function findTemplate(pathname: string): string | null {
  const hits = specPaths().filter((t) => templateToRegex(t).test(pathname))
  // Prefer the template with the most literal segments (a literal route beats a parameter route).
  hits.sort((a, b) => b.replace(/\{[^}]+\}/g, '').length - a.replace(/\{[^}]+\}/g, '').length)
  return hits[0] ?? null
}

export interface SeenRequest {
  method: string
  pathname: string
  query: URLSearchParams
  body: unknown
}

/**
 * Asserts one observed UI request is expressible in the spec: the path+method is documented, every query
 * parameter is declared, and a JSON body only uses documented properties and carries every required one.
 * Returns the matched template for further assertions.
 */
export function checkAgainstSpec(seen: SeenRequest): string[] {
  const problems: string[] = []
  const tpl = findTemplate(seen.pathname)
  if (!tpl) return [`path not in spec: ${seen.pathname}`]
  if (!hasOperation(tpl, seen.method)) return [`${seen.method} ${tpl} not in spec`]
  const declared = queryParams(tpl, seen.method)
  for (const k of seen.query.keys()) if (!declared.includes(k)) problems.push(`query parameter ${k} not declared on ${seen.method} ${tpl}`)
  const schema = requestBodySchema(tpl, seen.method)
  if (seen.body !== undefined && seen.body !== null && typeof seen.body === 'object') {
    if (!schema) problems.push(`${seen.method} ${tpl} declares no request body but one was sent`)
    else {
      const props = schemaProps(schema)
      for (const k of Object.keys(seen.body as object)) if (!props.includes(k)) problems.push(`body field ${k} not in ${schema}`)
      for (const k of schemaRequired(schema)) if (!(k in (seen.body as object))) problems.push(`required body field ${k} missing for ${schema}`)
    }
  } else if (schema && schemaRequired(schema).length > 0) {
    problems.push(`${seen.method} ${tpl} requires a body (${schema}) but none was sent`)
  }
  return problems
}
