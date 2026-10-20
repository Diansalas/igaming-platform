import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ReadinessChecklist } from './LaunchBits'
import { CHECK_NOTES, CHECK_TITLES, checkTitle } from './labels'
import { readiness } from './launchFixtures'
import { kindLabel } from '../licensing/labels'

const FORBIDDEN_WORDS = /\blicensed\b|\bauthorised\b|compliant|approved for operation|\bscore\b|traffic|\bexempt\b/i
const BASIS_ID = '88888888-8888-4888-8888-888888888888'
const RULE_ID = '99999999-9999-4999-8999-999999999999'

describe('lic9 readiness item R-OPERATING-BASIS (snapshot version 7)', () => {
  it('has a title and a note without legal claims, and the OpenAPI Check.code description names it', () => {
    const spec = readFileSync(resolve(process.cwd(), '../docs/api/openapi/platform-api.yaml'), 'utf8')
    expect(CHECK_TITLES['R-OPERATING-BASIS']).toBeTruthy()
    expect(checkTitle('R-OPERATING-BASIS')).not.toBe('R-OPERATING-BASIS')
    expect(CHECK_NOTES['R-OPERATING-BASIS']).toBeTruthy()
    expect(CHECK_NOTES['R-OPERATING-BASIS']).not.toMatch(FORBIDDEN_WORDS)
    expect(CHECK_TITLES['R-OPERATING-BASIS']).not.toMatch(FORBIDDEN_WORDS)
    expect(spec).toContain('R-OPERATING-BASIS')
  })

  it('a failing item is acknowledgeable (nothing pre-ticked), shows the note and the basis ids only; risk rule ids stay separate', () => {
    const r = readiness({
      checks: [
        { code: 'R-OPERATING-BASIS', result: 'fail', detail_code: 'review_overdue' },
        { code: 'R-RISK-RULES-LEGACY', result: 'fail', detail_code: 'deposit_rules_present' },
      ],
      non_passing_check_codes: ['R-OPERATING-BASIS', 'R-RISK-RULES-LEGACY'],
      refs: { operating_basis_review_overdue: [BASIS_ID], risk_rules_legacy_deposit_operation: [RULE_ID] },
    })
    render(<ReadinessChecklist readiness={r} acknowledged={new Set()} onToggleAck={() => {}} />)
    expect(screen.getByRole('checkbox', { name: 'Acknowledge R-OPERATING-BASIS' })).not.toBeChecked()
    expect(screen.getByText(CHECK_NOTES['R-OPERATING-BASIS'])).toBeInTheDocument()
    const basis = screen.getByLabelText('Operating bases due for review')
    expect(within(basis).getByText(BASIS_ID)).toBeInTheDocument()
    expect(within(basis).queryByText(RULE_ID)).toBeNull()
    const rules = screen.getByLabelText('Legacy risk rule ids')
    expect(within(rules).getByText(RULE_ID)).toBeInTheDocument()
    expect(within(rules).queryByText(BASIS_ID)).toBeNull()
  })

  it('a passing item shows no note and no ids', () => {
    const r = readiness({ checks: [{ code: 'R-OPERATING-BASIS', result: 'pass', detail_code: 'ok' }], non_passing_check_codes: [] })
    render(<ReadinessChecklist readiness={r} />)
    expect(screen.queryByText(CHECK_NOTES['R-OPERATING-BASIS'])).toBeNull()
    expect(screen.queryByLabelText('Operating bases due for review')).toBeNull()
  })

  it('the three licensing change kinds have labels (an unrecognised kind never reads as a known one)', () => {
    for (const k of ['operating_basis_set', 'operating_basis_withdraw', 'operating_basis_review_record']) {
      expect(kindLabel(k)).not.toMatch(/Unrecognised/)
    }
    expect(kindLabel('operating_basis_exempt')).toMatch(/Unrecognised/)
  })
})
