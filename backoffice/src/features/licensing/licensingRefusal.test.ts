import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { loadOpenApi } from '../../test/openapiContract'
import { ApiError } from '../../api/types'
import { describeLicensingError } from './errors'
import { licensingRefusalWording } from './licensingRefusal'

// c1 drift pin. The closed refusal vocabulary is owned by the server (internal/licensingchange/refusal.go, ClosedRefusals) and
// documented as the OpenAPI enum LicensingRefusalCode. The Go table, the OpenAPI enum and the UI wording map must hold EXACTLY the
// same codes: a new server code without wording, wording for a retired code, or an undocumented code fails here.
const goSource = readFileSync(resolve(process.cwd(), '../internal/licensingchange/refusal.go'), 'utf8')
const start = goSource.indexOf('var ClosedRefusals')
const table = goSource.slice(start, goSource.indexOf('\n}\n', start))
const goCodes = [...table.matchAll(/^\s*"([a-z_]+)":\s+"/gm)].map((m) => m[1])
const specCodes: string[] = loadOpenApi().components?.schemas?.LicensingRefusalCode?.enum ?? []
const uiCodes = Object.keys(licensingRefusalWording)

describe('closed refusal codes (licensingchange.ClosedRefusals) <-> OpenAPI <-> Back Office wording', () => {
  it('reads the Go table (non-vacuous) and the three sets are identical', () => {
    expect(goCodes.length).toBeGreaterThanOrEqual(30)
    expect(new Set(goCodes).size).toBe(goCodes.length)
    expect([...specCodes].sort()).toEqual([...goCodes].sort())
    expect([...uiCodes].sort()).toEqual([...goCodes].sort())
  })

  it('every wording is a real sentence, free of database internals and of legal claims', () => {
    for (const [code, text] of Object.entries(licensingRefusalWording)) {
      expect(text.length, code).toBeGreaterThan(30)
      expect(text, code).not.toMatch(/\bLC\d{3}\b|\bAP\d{3}\b|SQLSTATE|trigger|migration|constraint|[0-9a-f]{8}-[0-9a-f]{4}/i)
      expect(text, code).not.toMatch(/legally|is licensed|compliant|approved for operation|authorised to operate/i)
    }
  })

  // The server answers with these generic code / status pairs (unchanged by c1); the wording is exact on every one that can carry the code.
  const shapes: [string, number][] = [
    ['validation_error', 400],
    ['not_found', 404],
    ['LICENSING_CHANGE_REFUSED', 409],
    ['LICENSING_CHANGE_GUARD_REFUSED', 409],
  ]
  it.each(shapes)('code %s / %i: every closed code renders its exact wording, never the server message', (apiCode, status) => {
    for (const code of goCodes) {
      const v = describeLicensingError(new ApiError(apiCode as never, 'RAW SERVER MESSAGE leak', status, 'req-1', [code]))
      expect(v.message, `${apiCode} ${code}`).toContain(licensingRefusalWording[code])
      expect(v.message).toContain('Nothing was changed.')
      expect(v.message).not.toContain('RAW SERVER MESSAGE')
      expect(v.requestId).toBe('req-1')
      expect(v.status).toBe(status)
    }
  })

  it('an unknown code falls back to the generic wording of the status (never a guess, never the server text)', () => {
    const refused = describeLicensingError(new ApiError('LICENSING_CHANGE_REFUSED' as never, 'RAW SERVER MESSAGE', 409, 'req-1', ['made_up_code']))
    expect(refused.message).toMatch(/refused by the domain rules/)
    expect(refused.message).not.toContain('RAW SERVER MESSAGE')
    const invalid = describeLicensingError(new ApiError('validation_error' as never, 'RAW SERVER MESSAGE', 400, 'req-1', ['made_up_code']))
    expect(invalid.message).toMatch(/rejected the request as invalid/)
    const none = describeLicensingError(new ApiError('LICENSING_CHANGE_GUARD_REFUSED' as never, 'RAW SERVER MESSAGE', 409, 'req-1', []))
    expect(none.message).toMatch(/database safeguard/)
    // the first KNOWN code wins when an unknown one precedes it
    const mixed = describeLicensingError(new ApiError('LICENSING_CHANGE_REFUSED' as never, 'x', 409, 'req-1', ['made_up_code', 'licence_not_active']))
    expect(mixed.message).toContain(licensingRefusalWording.licence_not_active)
  })

  it('a closed code never overrides an authorisation, step-up or server-error answer', () => {
    for (const [apiCode, status] of [['forbidden', 403], ['unauthorized', 401], ['step_up_required', 403], ['internal_error', 500], ['LICENSING_CHANGE_SELF_APPROVAL', 409]] as [string, number][]) {
      const v = describeLicensingError(new ApiError(apiCode as never, 'x', status, 'req-1', ['licence_not_active']))
      expect(v.message, apiCode).not.toContain(licensingRefusalWording.licence_not_active)
    }
  })
})
