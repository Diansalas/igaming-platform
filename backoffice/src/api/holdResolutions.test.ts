import { describe, expect, it } from 'vitest'
import { captureRequests } from '../test/contractCapture'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../test/session'
import { checkAgainstSpec } from '../test/openapiLite'
import { approveHoldResolution, cancelHoldResolution, getHoldResolution, listHoldCandidates, listHoldResolutions, rejectHoldResolution, requestHoldResolution } from './holdResolutions'

const T = '11111111-1111-4111-8111-111111111111'
const R = '44444444-4444-4444-8444-444444444444'
const H = 'b'.repeat(64)

describe('holdResolutions api: UI-to-OpenAPI contract (HSEC)', () => {
  it('every request the module sends is documented', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen = captureRequests()
    await listHoldResolutions(T)
    await getHoldResolution(T, R)
    await requestHoldResolution(T, { withdrawal_request_id: R, kind: 'release_hold_to_player', reason_code: 'tenant_closed', evidence_ref_hash: H, note: 'n' }, 'a')
    await approveHoldResolution(T, R, H, 'ok', 'b')
    await rejectHoldResolution(T, R, H, 'no', 'c')
    await cancelHoldResolution(T, R, 'd')
    await listHoldCandidates(T, { limit: 50 })
    expect(seen).toHaveLength(7)
    for (const s of seen) expect(checkAgainstSpec(s)).toEqual([])
    for (const s of seen) expect(JSON.stringify(s.body ?? {})).not.toMatch(/proof|signature|tenant_id/)
  })
})
