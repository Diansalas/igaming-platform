// UI-to-OpenAPI contract check for the AML client (ADR 0116 A7). The OpenAPI document is the server contract; this test fails
// when a path, a method or a request-body field of src/api/aml.ts drifts from it, in either direction.
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import {
  AML_ENDPOINTS,
  addNote,
  approveAction,
  fillPath,
  flagSar,
  getCase,
  listCases,
  listPersonHolds,
  liftHold,
  openCase,
  placeHold,
  proposeAction,
  transitionCase,
} from './aml'
import { server } from '../test/mswServer'

const spec = readFileSync(resolve(process.cwd(), '../docs/api/openapi/platform-api.yaml'), 'utf8')

/** The block of one path in the spec (up to the next path key), or null. */
function pathBlock(path: string): string | null {
  const start = spec.indexOf(`\n  ${path}:\n`)
  if (start < 0) return null
  const rest = spec.slice(start + 1)
  const next = rest.slice(1).search(/\n {2}\/v1\/[^\n]*:\n|\ncomponents:\n/)
  return next < 0 ? rest : rest.slice(0, next + 1)
}

function schemaBlock(name: string): string {
  const start = spec.indexOf(`\n    ${name}:\n`)
  if (start < 0) throw new Error(`schema ${name} missing`)
  const rest = spec.slice(start + 1)
  const next = rest.slice(1).search(/\n {4}[A-Za-z]\w*:\n/)
  return next < 0 ? rest : rest.slice(0, next + 1)
}

function schemaProps(name: string): { props: string[]; required: string[] } {
  const b = schemaBlock(name)
  const req = /required: \[([^\]]*)\]/.exec(b)?.[1].split(',').map((s) => s.trim()).filter(Boolean) ?? []
  const propsStart = b.indexOf('properties:\n')
  const props = [...b.slice(propsStart).matchAll(/\n {8}(\w+):/g)].map((m) => m[1])
  return { props, required: req }
}

const T = '11111111-1111-4111-8111-111111111111'
const C = '22222222-2222-4222-8222-222222222222'
const P = '33333333-3333-4333-8333-333333333333'
const H = '44444444-4444-4444-8444-444444444444'
const HASH = 'a'.repeat(64)

describe('AML client vs OpenAPI', () => {
  it('every endpoint of the client is documented under the same path and method', () => {
    for (const [name, e] of Object.entries(AML_ENDPOINTS)) {
      const block = pathBlock(e.path)
      expect(block, `${name}: ${e.path} must be a documented path`).not.toBeNull()
      expect(block!, `${name}: ${e.method} must be documented on ${e.path}`).toContain(`\n    ${e.method.toLowerCase()}:\n`)
    }
  })

  it('nothing under /aml/ is documented but unused by the client (set equality, both directions)', () => {
    const documented = new Set<string>()
    for (const m of spec.matchAll(/^ {2}(\/v1\/admin\/tenants\/\{tenantID\}\/aml\/[^:\n]*):\n((?: {4}[^\n]*\n|\n)*)/gm)) {
      for (const op of m[2].matchAll(/^ {4}(get|post|put|patch|delete):$/gm)) documented.add(`${op[1].toUpperCase()} ${m[1]}`)
    }
    const used = new Set(Object.values(AML_ENDPOINTS).map((e) => `${e.method} ${e.path}`))
    expect([...documented].sort()).toEqual([...used].sort())
  })

  it('the golden classification and the server permission names are documented on every operation', () => {
    for (const e of Object.values(AML_ENDPOINTS)) {
      const block = pathBlock(e.path)!
      expect(block).toMatch(/aml_(case|hold):(read|work|close_approve|manage)/)
      expect(block).toContain('no-store')
    }
  })

  it('every client function sends exactly the documented method and path (ids URL-encoded, tenant in the path only)', async () => {
    const seen: Array<{ method: string; path: string; body: unknown }> = []
    server.use(
      http.all(/\/v1\/admin\/tenants\/.*\/aml\/.*/, async ({ request }) => {
        const text = await request.text()
        seen.push({ method: request.method, path: new URL(request.url).pathname, body: text ? JSON.parse(text) : undefined })
        return HttpResponse.json({ items: [], case: {}, events: [], notes: [], hold_id: H, lift_id: H, case_id: C, last_seq: 2, proposal_seq: 2, content_hash: HASH })
      }),
    )
    await listCases(T, 20)
    await getCase(T, C)
    await listPersonHolds(T, P)
    await placeHold(T, { person_id: P, scope: 'all_value_movement', reason_class: 'manual', reason_code: 'r_1' })
    await liftHold(T, H, 'r_1')
    await openCase(T, { person_id: P, kind: 'manual', reason_code: 'r_1' })
    await transitionCase(T, C, 1, 'in_review', 'r_1')
    await addNote(T, C, 2, 'checked')
    await flagSar(T, C, 3, 'r_1')
    await proposeAction(T, C, { expected_seq: 3, kind: 'lift_hold', hold_id: H, reason_code: 'r_1' })
    await approveAction(T, C, 4, HASH, 'r_1')
    const sent = seen.map((s) => `${s.method} ${s.path.replace(T, '{tenantID}').replace(C, '{caseID}').replace(P, '{personID}').replace(H, '{holdID}')}`)
    const want = Object.values(AML_ENDPOINTS).map((e) => `${e.method} ${e.path}`)
    expect(sent.sort()).toEqual(want.sort())
    // No body ever carries a tenant or an actor.
    for (const s of seen) expect(JSON.stringify(s.body ?? {})).not.toMatch(/tenant|actor|staff/i)
  })

  it('request bodies use exactly the documented fields, and carry every required one', async () => {
    const bodies = new Map<string, Record<string, unknown>>()
    server.use(
      http.post(/\/v1\/admin\/tenants\/.*\/aml\/.*/, async ({ request }) => {
        bodies.set(new URL(request.url).pathname.replace(/[0-9a-f-]{36}/g, 'ID'), (await request.json()) as Record<string, unknown>)
        return HttpResponse.json({})
      }),
    )
    await placeHold(T, { person_id: P, player_account_id: null, scope: 'all_value_movement', reason_class: 'manual', reason_code: 'r_1' })
    await liftHold(T, H, 'r_1')
    await openCase(T, { person_id: P, kind: 'manual', reason_code: 'r_1', alert_ids: [] })
    await transitionCase(T, C, 1, 'in_review', 'r_1')
    await addNote(T, C, 2, 'checked')
    await flagSar(T, C, 3, 'r_1')
    await proposeAction(T, C, { expected_seq: 3, kind: 'close_sar_filed', external_filing_reference: 'REF-1', reason_code: 'r_1' })
    await approveAction(T, C, 4, HASH, 'r_1')
    const bySchema: Record<string, string> = {
      '/v1/admin/tenants/ID/aml/holds': 'AmlPlaceHoldRequest',
      '/v1/admin/tenants/ID/aml/holds/ID/lift': 'AmlReasonRequest',
      '/v1/admin/tenants/ID/aml/cases': 'AmlOpenCaseRequest',
      '/v1/admin/tenants/ID/aml/cases/ID/transition': 'AmlTransitionRequest',
      '/v1/admin/tenants/ID/aml/cases/ID/notes': 'AmlNoteRequest',
      '/v1/admin/tenants/ID/aml/cases/ID/sar-flag': 'AmlSeqReasonRequest',
      '/v1/admin/tenants/ID/aml/cases/ID/proposals': 'AmlProposeRequest',
      '/v1/admin/tenants/ID/aml/cases/ID/approvals': 'AmlApproveRequest',
    }
    expect([...bodies.keys()].sort()).toEqual(Object.keys(bySchema).sort())
    for (const [path, schema] of Object.entries(bySchema)) {
      const { props, required } = schemaProps(schema)
      const keys = Object.keys(bodies.get(path)!)
      for (const k of keys) expect(props, `${schema}: ${k} is not a documented field`).toContain(k)
      for (const r of required) expect(keys, `${schema}: required ${r} is missing from the sent body`).toContain(r)
    }
  })

  it('fillPath URL-encodes every value', () => {
    expect(fillPath('/a/{x}/b/{y}', { x: 'a b', y: '../z' })).toBe('/a/a%20b/b/..%2Fz')
  })
})
