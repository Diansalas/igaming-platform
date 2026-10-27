import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ReadinessChecklist } from './LaunchBits'
import { CHECK_NOTES, CHECK_TITLES, checkTitle } from './labels'
import { readiness } from './launchFixtures'

const FORBIDDEN_WORDS = /\blicensed\b|\bauthorised\b|compliant|approved for operation|\bscore\b|traffic/i
const RULE_ID = '99999999-9999-4999-8999-999999999999'

describe('lic7 readiness items (snapshot version 6)', () => {
  it('both codes have titles and notes, and the OpenAPI Check.code description names them', () => {
    const spec = readFileSync(resolve(process.cwd(), '../docs/api/openapi/platform-api.yaml'), 'utf8')
    for (const code of ['R-RISK-RULES-LEGACY', 'R-KYC-POLICY']) {
      expect(CHECK_TITLES[code]).toBeTruthy()
      expect(checkTitle(code)).not.toBe(code)
      expect(CHECK_NOTES[code]).toBeTruthy()
      expect(CHECK_NOTES[code]).not.toMatch(FORBIDDEN_WORDS)
      expect(spec).toContain(code)
    }
  })

  it('a failing item is acknowledgeable (checkbox, nothing pre-ticked), shows the interim note and rule ids only', () => {
    const r = readiness({
      checks: [
        { code: 'R-RISK-RULES-LEGACY', result: 'fail', detail_code: 'jurisdiction_scoped_rules_present' },
        { code: 'R-KYC-POLICY', result: 'fail', detail_code: 'no_active_policy' },
      ],
      non_passing_check_codes: ['R-KYC-POLICY', 'R-RISK-RULES-LEGACY'],
      refs: { risk_rules_legacy_jurisdiction_scoped: [RULE_ID] },
    })
    render(<ReadinessChecklist readiness={r} acknowledged={new Set()} onToggleAck={() => {}} />)
    for (const code of ['R-RISK-RULES-LEGACY', 'R-KYC-POLICY']) {
      expect(screen.getByRole('checkbox', { name: `Acknowledge ${code}` })).not.toBeChecked()
    }
    expect(screen.getByText(CHECK_NOTES['R-KYC-POLICY'])).toBeInTheDocument()
    expect(screen.getByText(CHECK_NOTES['R-RISK-RULES-LEGACY'])).toBeInTheDocument()
    const ids = screen.getByLabelText('Legacy risk rule ids')
    expect(within(ids).getByText(RULE_ID)).toBeInTheDocument()
  })

  it('a passing item shows no note and no rule ids', () => {
    const r = readiness({
      checks: [{ code: 'R-RISK-RULES-LEGACY', result: 'pass', detail_code: 'ok' }],
      non_passing_check_codes: [],
    })
    render(<ReadinessChecklist readiness={r} />)
    expect(screen.queryByText(CHECK_NOTES['R-RISK-RULES-LEGACY'])).toBeNull()
    expect(screen.queryByLabelText('Legacy risk rule ids')).toBeNull()
  })
})
