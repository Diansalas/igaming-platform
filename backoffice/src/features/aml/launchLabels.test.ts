import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { CHECK_TITLES, checkTitle } from '../launch/labels'

describe('launch readiness labels for the AML items (ADR 0116 A7)', () => {
  it('the three R-AML-* check codes have titles, and the OpenAPI Check.code description names them', () => {
    const spec = readFileSync(resolve(process.cwd(), '../docs/api/openapi/platform-api.yaml'), 'utf8')
    for (const code of ['R-AML-QUORUM', 'R-AML-SCREENING', 'R-AML-MONITORING']) {
      expect(CHECK_TITLES[code]).toBeTruthy()
      expect(checkTitle(code)).not.toBe(code)
      expect(spec).toContain(code)
    }
    expect(checkTitle('R-AML-MONITORING')).toMatch(/not implemented/i)
  })
})
