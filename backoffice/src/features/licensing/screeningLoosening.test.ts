import { describe, expect, it } from 'vitest'
import { ApiError } from '../../api/types'
import { REFUSAL_TEXT } from '../aml/screeningErrors'
import { describeLicensingError } from './errors'
import { KIND_FILTER_OPTIONS, kindLabel } from './labels'

// c4 (migration 0190): the two screening loosening kinds ride the generic licensing change screens. Filing is API-only (NOT IMPLEMENTED in
// the UI); approving, rejecting and reading work on the generic detail page, which needs the kind labels and the closed refusal wording.
describe('screening loosening kinds in the generic licensing change screens', () => {
  it('labels both kinds and offers them in the kind filter', () => {
    expect(kindLabel('aml_screening_policy_loosen')).toBe('Screening policy loosening (AML)')
    expect(kindLabel('aml_screening_loosening_configure')).toBe('Screening policy loosening capability (AML)')
    const values = KIND_FILTER_OPTIONS.map((o) => o.value)
    expect(values).toContain('aml_screening_policy_loosen')
    expect(values).toContain('aml_screening_loosening_configure')
  })
  it.each(['loosening_not_configured', 'loosening_disabled', 'no_screening_policy_to_loosen', 'not_a_loosening', 'loosening_cooling_period', 'officer_grant_required', 'officer_quorum_not_met', 'sanctions_list_removal_refused', 'loosening_mixes_tightening', 'grace_extension_after_expiry_refused', 'loosening_not_supported_for_tenant_licensing_model', 'gate_environment_refused', 'rescreen_interval_below_floor'])(
    'shows the UI-owned wording of the closed code %s on a guard refusal and never the server message',
    (code) => {
      const v = describeLicensingError(new ApiError('LICENSING_CHANGE_GUARD_REFUSED' as never, 'RAW SERVER MESSAGE', 409, 'req-1', ['LC050', code]))
      expect(v.lcCodes.map((c) => c.code)).toContain(code)
      expect(v.lcCodes.find((c) => c.code === code)?.text).toBe(REFUSAL_TEXT[code])
      expect(JSON.stringify(v)).not.toContain('RAW SERVER MESSAGE')
      expect(v.message).toContain('Nothing was changed.')
    },
  )
  it('ignores a code outside the closed vocabulary', () => {
    const v = describeLicensingError(new ApiError('LICENSING_CHANGE_GUARD_REFUSED' as never, 'x', 409, 'req-1', ['made_up_code']))
    expect(v.lcCodes.map((c) => c.code)).not.toContain('made_up_code')
  })
})
