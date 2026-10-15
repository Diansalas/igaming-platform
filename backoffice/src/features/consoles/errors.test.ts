import { describe, expect, it } from 'vitest'
import { ApiError } from '../../api/types'
import { schemaEnum } from '../../test/openapiLite'
import { HSEC_TOKENS, K3_TOKENS, REFUSAL_TEXT, describeConsoleError, describeRefusalCode, refusalIsStale } from './errors'

const SENTINEL = 'RAW SERVER MESSAGE <b>x</b>'
const err = (status: number, code: string, message: string, requestId = 'req-1') => new ApiError(code as never, message, status, requestId)

describe('K3 / HSEC closed tokens (ADR 0114 invariant 7.9)', () => {
  it('maps every token the OpenAPI documents, by status, to UI-owned text', () => {
    for (const [token, t] of Object.entries(K3_TOKENS)) {
      const v = describeConsoleError(err(t.status, t.status === 403 ? 'forbidden' : t.status === 404 ? 'not_found' : 'conflict', token), 'k3', 'approve')
      expect(v.message).toBe(t.text)
      expect(v.code).toBe(token)
      expect(v.message).not.toContain(token)
    }
    for (const [token, t] of Object.entries(HSEC_TOKENS)) {
      const v = describeConsoleError(err(t.status, t.status === 403 ? 'forbidden' : t.status === 404 ? 'not_found' : 'conflict', token), 'hsec', 'approve')
      expect(v.message).toBe(t.text)
    }
  })

  it('the token tables are exactly the tokens the spec documents (a new server token must be mapped here)', () => {
    // Collect every force_resolve_* / hold_resolution_* token named in the spec text.
    const spec = specText()
    const k3 = new Set(spec.match(/force_resolve_[a-z_]+/g) ?? [])
    const hsec = new Set(spec.match(/hold_resolution_[a-z_]+/g) ?? [])
    for (const t of k3) {
      if (t === 'force_resolve_') continue
      expect(Object.keys(K3_TOKENS), `K3 token ${t} is in the spec but not mapped`).toContain(t)
    }
    for (const t of hsec) expect(Object.keys(HSEC_TOKENS), `HSEC token ${t} is in the spec but not mapped`).toContain(t)
    expect(schemaEnum('PaymentForceResolutionRequest', 'basis_code').length).toBeGreaterThan(0)
  })

  it('never prints an unknown server message, and ignores a known token that arrives with the wrong status', () => {
    for (const [status, code] of [[400, 'validation_error'], [403, 'forbidden'], [404, 'not_found'], [409, 'conflict'], [500, 'internal_error'], [503, 'service_unavailable'], [429, 'rate_limited']] as const) {
      for (const family of ['attempts', 'recon', 'k1', 'k3', 'hsec', 'staff'] as const) {
        const v = describeConsoleError(err(status, code, SENTINEL), family, 'approve')
        expect(v.message).not.toContain('RAW SERVER')
        expect(v.message.length).toBeGreaterThan(10)
      }
    }
    // a 409 token delivered on a 403 is NOT trusted as that token
    const v = describeConsoleError(err(403, 'forbidden', 'force_resolve_expired'), 'k3', 'approve')
    expect(v.message).toContain('The server refused this')
    expect(v.code).toBe('forbidden')
  })

  it('flags stale only where a refresh is the right next step', () => {
    expect(refusalIsStale(err(409, 'conflict', 'force_resolve_expired'), 'k3', 'approve')).toBe(true)
    expect(refusalIsStale(err(409, 'conflict', 'force_resolve_disabled'), 'k3', 'request')).toBe(false)
    expect(refusalIsStale(err(403, 'forbidden', 'force_resolve_not_permitted'), 'k3', 'request')).toBe(false)
    expect(refusalIsStale(new Error('x'), 'k3', 'request')).toBe(false)
  })

  it('maps the client-side step-up outcomes without any server text', () => {
    for (const code of ['step_up_cancelled', 'step_up_failed', 'step_up_required']) {
      const v = describeConsoleError(new ApiError(code as never, 'client fixed text', 403), 'k3', 'approve')
      expect(v.message).not.toContain('RAW')
      expect(v.message.length).toBeGreaterThan(10)
    }
    expect(describeConsoleError(new ApiError('network_error', 'x', 0), 'k1', 'list').message).toContain('Nothing was confirmed')
  })

  it('K1 conflicts are explained per operation, including the platform-session rule', () => {
    expect(describeConsoleError(err(409, 'conflict', SENTINEL), 'k1', 'request').message).toContain('platform session may not request')
    expect(describeConsoleError(err(409, 'conflict', SENTINEL), 'k1', 'cancel').message).toContain('only the person who filed')
    expect(describeConsoleError(err(409, 'conflict', SENTINEL), 'k1', 'revoke').message).toContain('already revoked')
  })
})

describe('refusal codes in a 200 body', () => {
  it('prints UI text for the documented codes and only a safe token for anything else', () => {
    for (const c of Object.keys(REFUSAL_TEXT)) expect(describeRefusalCode(c)).toBe(REFUSAL_TEXT[c])
    expect(describeRefusalCode('<b>x</b>')).toContain('unprintable')
    expect(describeRefusalCode(undefined)).toContain('no longer held')
    // the documented hold-resolution refusal codes are all covered
    for (const c of ['withdrawal_not_approved', 'attempt_exists', 'tenant_brand_active_again', 'policy_disabled']) expect(REFUSAL_TEXT[c]).toBeTruthy()
  })
})

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
function specText(): string {
  const t = readFileSync(resolve(process.cwd(), '../docs/api/openapi/platform-api.yaml'), 'utf8')
  const a = t.indexOf('    ForceResolutionForbidden:')
  const b = t.indexOf('    ProviderCredentialInvalidRequest:')
  return t.slice(a, b)
}
