import { readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { effectiveOutsideWindow, graceProblem, intervalProblem, looseningReasons, minIntervalForGate, wholeSeconds } from './screeningPolicyRules'

const policy = (o: Partial<Parameters<typeof looseningReasons>[0] & object> = {}) => ({ configured: true, lists: ['sanctions', 'pep'] as ('sanctions' | 'pep')[], rescreen_interval_seconds: 7200, backfill_grace_seconds: 100, effective_from: '2026-10-01T10:00:00Z', ...o })

describe('screening policy rules', () => {
  it('whole seconds are digit-only exact integers', () => {
    expect(wholeSeconds('0')).toBe(0)
    expect(wholeSeconds('86400')).toBe(86400)
    for (const bad of ['', '-1', '01', '1.5', '1e3', ' 1', '1 ', '12345678901', 'abc']) expect(wholeSeconds(bad), bad).toBeNull()
  })
  it('the interval floor is 3600 and the maximum 31536000; the grace may be 0', () => {
    expect(intervalProblem('3599')).toMatch(/floor of 3600/)
    expect(intervalProblem('3600')).toBeNull()
    expect(intervalProblem('31536000')).toBeNull()
    expect(intervalProblem('31536001')).toMatch(/maximum/)
    expect(intervalProblem('')).toMatch(/required/)
    expect(graceProblem('0')).toBeNull()
    expect(graceProblem('31536001')).toMatch(/maximum/)
    expect(graceProblem('')).toMatch(/required/)
  })
  it('twice the gate threshold is an integer', () => {
    expect(minIntervalForGate(3600)).toBe(7200)
    expect(minIntervalForGate(2592000)).toBe(5184000)
  })
  it('the effective window is now-2min .. now+30d', () => {
    const now = Date.parse('2026-10-20T12:00:00Z')
    expect(effectiveOutsideWindow(now - 119_000, now)).toBe(false)
    expect(effectiveOutsideWindow(now - 121_000, now)).toBe(true)
    expect(effectiveOutsideWindow(now + 30 * 86_400_000, now)).toBe(false)
    expect(effectiveOutsideWindow(now + 30 * 86_400_000 + 1, now)).toBe(true)
  })
  it('loosening: list removal, longer interval or grace, earlier effective time; nothing in force means nothing to loosen', () => {
    const t = Date.parse('2026-10-02T10:00:00Z')
    expect(looseningReasons(null, { lists: ['pep'], interval: 99999, grace: 9, effectiveMs: t })).toEqual([])
    expect(looseningReasons({ ...policy(), configured: false }, { lists: ['pep'], interval: 99999, grace: 9, effectiveMs: t })).toEqual([])
    expect(looseningReasons(policy(), { lists: ['sanctions', 'pep'], interval: 7200, grace: 100, effectiveMs: t })).toEqual([])
    expect(looseningReasons(policy(), { lists: ['sanctions'], interval: 7200, grace: 100, effectiveMs: t })).toHaveLength(1)
    expect(looseningReasons(policy(), { lists: ['sanctions', 'pep'], interval: 7201, grace: 101, effectiveMs: Date.parse('2026-09-01T00:00:00Z') })).toHaveLength(3)
    expect(looseningReasons(policy(), { lists: ['sanctions', 'pep', ], interval: 3600, grace: 0, effectiveMs: t })).toEqual([])
  })
  it('no float arithmetic in the rules or the tab', () => {
    for (const f of ['screeningPolicyRules.ts', 'ScreeningPolicyTab.tsx', 'ScreeningBits.tsx']) {
      const text = readFileSync(join(resolve(process.cwd(), 'src/features/aml'), f), 'utf8').split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')
      expect(text, f).not.toMatch(/\bNumber\s*\(|parseInt|parseFloat|BigInt\s*\(|\.toFixed|Math\.(round|floor|ceil)|\/ ?100\b|\* ?1\.\d/)
      expect(text, f).not.toMatch(/dangerouslySetInnerHTML|innerHTML|localStorage|sessionStorage|document\.cookie|\bfetch\(|(err|error|e)\.message/)
      expect(text, f).not.toMatch(/crypto\.subtle|sha256|hmac|X-Actor-Proof|actorProof|\bsign\(/i)
      expect(text, f).not.toMatch(/legally|fully licensed|is licensed|compliant|approved for operation|jurisdiction (is )?approved/i)
    }
  })
  it('the author form starts empty', () => {
    const t = readFileSync(join(resolve(process.cwd(), 'src/features/aml'), 'ScreeningPolicyTab.tsx'), 'utf8')
    for (const re of [/\[lists, setLists\] = useState<ScreeningList\[\]>\(\[\]\)/, /\[intervalText, setIntervalText\] = useState\(''\)/, /\[grace, setGrace\] = useState\(''\)/, /\[from, setFrom\] = useState\(''\)/, /\[legal, setLegal\] = useState\(''\)/, /\[reason, setReason\] = useState\(''\)/]) expect(t).toMatch(re)
  })
})
