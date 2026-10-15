// Thin runtime-request and schema-pin helpers over the shared parsed spec (test/openapiContract.ts loadOpenApi).
// The static, per-module call check is api/openapiContract.test.ts (A10a); this file adds only what that cannot do:
// checking an OBSERVED request, and reading schema enums / properties for pins. No YAML parsing lives here.
import { loadOpenApi } from './openapiContract'

/* eslint-disable @typescript-eslint/no-explicit-any */
type J = any
const spec = () => loadOpenApi() as J
const schemas = () => spec().components.schemas as Record<string, J>

export function specPaths(): string[] {
  return Object.keys(spec().paths)
}

export function hasOperation(path: string, method: string): boolean {
  return !!spec().paths[path]?.[method.toLowerCase()]
}

function op(path: string, method: string): J {
  return spec().paths[path]?.[method.toLowerCase()]
}

export function queryParams(path: string, method: string): string[] {
  return ((op(path, method)?.parameters ?? []) as J[]).filter((p) => p.in === 'query').map((p) => p.name as string)
}

function resolve(s: J): J {
  const m = typeof s?.$ref === 'string' ? /#\/components\/schemas\/(\w+)/.exec(s.$ref) : null
  return m ? schemas()[m[1]] : s
}

export function requestBodySchema(path: string, method: string): string | null {
  const ref = op(path, method)?.requestBody?.content?.['application/json']?.schema?.$ref
  const m = typeof ref === 'string' ? /#\/components\/schemas\/(\w+)/.exec(ref) : null
  return m ? m[1] : null
}

export function schemaProps(name: string): string[] {
  return Object.keys(schemas()[name]?.properties ?? {})
}

export function schemaRequired(name: string): string[] {
  return (schemas()[name]?.required ?? []) as string[]
}

export function schemaEnum(name: string, prop?: string): string[] {
  const s = schemas()[name]
  const t = resolve(prop ? s?.properties?.[prop] : s)
  return (t?.enum ?? []) as string[]
}

function templateToRegex(t: string): RegExp {
  return new RegExp('^' + t.replace(/\{[^}]+\}/g, '[^/]+') + '$')
}

export function findTemplate(pathname: string): string | null {
  const hits = specPaths().filter((t) => templateToRegex(t).test(pathname))
  hits.sort((a, b) => b.replace(/\{[^}]+\}/g, '').length - a.replace(/\{[^}]+\}/g, '').length)
  return hits[0] ?? null
}

export interface SeenRequest {
  method: string
  pathname: string
  query: URLSearchParams
  body: unknown
}

/** Asserts one OBSERVED UI request is expressible in the spec (path, method, declared query keys, body fields, required fields). */
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
