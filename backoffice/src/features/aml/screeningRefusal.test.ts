import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { loadOpenApi } from '../../test/openapiContract'
import { ApiError } from '../../api/types'
import { REFUSAL_TEXT, describeScreeningError, type ScreeningOp } from './screeningErrors'

// The closed refusal vocabulary is owned by the server (internal/aml/refusal.go, aml.RefusalCodes) and documented in the OpenAPI
// enum AMLScreeningRefusalCode. The UI wording table must cover EXACTLY that set: a new server code without wording, or wording
// for a code the server no longer sends, fails here.
const goSource = readFileSync(resolve(process.cwd(), '../internal/aml/refusal.go'), 'utf8')
const table = goSource.slice(goSource.indexOf('var RefusalCodes'), goSource.indexOf('\n}\n', goSource.indexOf('var RefusalCodes')))
const serverCodes = [...table.matchAll(/^\s*"([a-z_]+)":\s+"/gm)].map((m) => m[1])
const specCodes: string[] = loadOpenApi().components?.schemas?.AMLScreeningRefusalCode?.enum ?? []

describe('closed refusal codes (aml.RefusalCodes) <-> UI wording', () => {
  it('reads the server table (non-vacuous) and the OpenAPI enum agrees with it', () => {
    expect(serverCodes.length).toBeGreaterThanOrEqual(24)
    expect([...serverCodes].sort()).toEqual([...specCodes].sort())
  })
  it('the UI wording covers exactly the server codes', () => {
    expect(Object.keys(REFUSAL_TEXT).sort()).toEqual([...serverCodes].sort())
    for (const [code, text] of Object.entries(REFUSAL_TEXT)) {
      expect(text.length, code).toBeGreaterThan(30)
      expect(text, code).not.toMatch(/legally|is licensed|compliant|approved for operation/i)
    }
  })
  it.each(['policy_read', 'policy_set', 'requests', 'resolve', 'screening_policy_read', 'screening_policy_set'] as ScreeningOp[])('op %s: every code is shown in its exact wording for 400, 403 and 409, and the server message never is', (op) => {
    for (const code of serverCodes) {
      for (const status of [400, 403, 409]) {
        const v = describeScreeningError(new ApiError('conflict' as never, 'RAW SERVER MESSAGE', status, 'req-1', [code]), op)
        expect(v.message, `${op} ${status} ${code}`).toContain(REFUSAL_TEXT[code])
        expect(v.message).toContain('Nothing was changed.')
        expect(v.message).not.toContain('RAW SERVER MESSAGE')
      }
    }
  })
  it('an unknown code falls back to the existing wording (never a guess, never the server text)', () => {
    const v = describeScreeningError(new ApiError('conflict' as never, 'RAW SERVER MESSAGE', 409, 'req-1', ['made_up_code']), 'screening_policy_set')
    expect(v.message).not.toContain('RAW SERVER MESSAGE')
    expect(v.message).toContain('Nothing was changed.')
  })
  it('quorum and four-eyes refusals keep their existing wording', () => {
    const v = describeScreeningError(new ApiError('conflict' as never, 'x', 409, 'req-1', ['quorum_not_met', 'policy_loosening_refused']), 'screening_policy_set')
    expect(v.message).toMatch(/officer quorum/)
  })
})
