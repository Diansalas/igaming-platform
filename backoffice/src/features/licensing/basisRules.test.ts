import { describe, expect, it } from 'vitest'
import { codeProblem, countryProblem, dateProblem, optRefProblem, productListProblem, refProblem, reviewClass, reviewState, roleProblem, splitList } from './basisRules'

describe('basis client rules (mirror of the server; the server stays authoritative)', () => {
  it('refProblem', () => {
    expect(refProblem('DOC-0001')).toBeNull()
    expect(refProblem('2026-10-19 note')).toBeNull() // an ISO date is not a phone number
    for (const bad of ['', ' x', 'x ', 'a@b.c', 'a\nb', 'a\tb', 'a​b', 'x'.repeat(201), 'call +44 20 7946 0958', '1234 5678 9012', 'someone at example dot com', '0x' + 'a'.repeat(40)]) expect(refProblem(bad), JSON.stringify(bad)).not.toBeNull()
    expect(refProblem('x'.repeat(200))).toBeNull()
    expect(refProblem('é'.repeat(101))).not.toBeNull() // 202 bytes
    expect(optRefProblem('')).toBeNull()
    expect(optRefProblem('a@b.c')).not.toBeNull()
  })
  it('roleProblem accepts function codes and refuses names', () => {
    for (const ok of ['head_of_compliance', 'legal.counsel', 'cco:eu', 'ab']) expect(roleProblem(ok), ok).toBeNull()
    for (const bad of ['', 'a', 'Jane Doe', 'Head', '1abc', 'x@y', 'a'.repeat(101)]) expect(roleProblem(bad), bad).not.toBeNull()
  })
  it('country, code, list and date', () => {
    expect(countryProblem('MT')).toBeNull()
    for (const bad of ['', 'mt', 'MTA', 'M']) expect(countryProblem(bad)).not.toBeNull()
    expect(codeProblem('wagering')).toBeNull()
    for (const bad of ['', ' x', 'a'.repeat(65)]) expect(codeProblem(bad)).not.toBeNull()
    expect(productListProblem('')).toBeNull()
    expect(productListProblem('a, b')).toBeNull()
    expect(productListProblem('a, a')).not.toBeNull()
    expect(productListProblem(Array.from({ length: 65 }, (_, i) => `p${i}`).join(','))).not.toBeNull()
    expect(splitList(' a , ,b ')).toEqual(['a', 'b'])
    expect(dateProblem('2027-03-31')).toBeNull()
    for (const bad of ['', '2027-3-1', '2027-02-30', 'tomorrow']) expect(dateProblem(bad)).not.toBeNull()
  })
  it('reviewState: the due date itself stands, the day after is overdue, 30 days is due soon, a flag overrides', () => {
    const today = '2026-10-20'
    expect(reviewState('2026-10-19', false, today).state).toBe('overdue')
    expect(reviewState('2026-10-20', false, today).state).toBe('due_soon')
    expect(reviewState('2026-11-19', false, today).state).toBe('due_soon')
    expect(reviewState('2026-11-20', false, today).state).toBe('ok')
    expect(reviewState(null, false, today).state).toBe('none')
    expect(reviewState('2099-01-01', true, today).state).toBe('rereview')
    expect(reviewState('garbage', false, today).state).toBe('none')
  })
  it('reviewClass: later date or cleared flag widens; otherwise narrows', () => {
    expect(reviewClass('2027-01-01', false, '2027-06-01', false)).toBe('widening')
    expect(reviewClass('2027-01-01', false, '2026-12-01', false)).toBe('narrowing')
    expect(reviewClass('2027-01-01', false, '2027-01-01', true)).toBe('narrowing')
    expect(reviewClass('2027-01-01', true, '2027-01-01', false)).toBe('widening')
  })
})
