import { readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// Static pins over the lic20 screening screens (a grep, not a proof; the behavioural tests and the server are the guarantees).
const DIR = resolve(process.cwd(), 'src/features/aml')
const FILES = ['ScreeningPage.tsx', 'ScreeningLabels.ts', 'screeningErrors.ts']
const code = (t: string) => t.split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')
const text = Object.fromEntries(FILES.map((f) => [f, code(readFileSync(join(DIR, f), 'utf8'))]))

describe('screening screen source pins', () => {
  it('renders no server HTML, touches no storage, calls no fetch, prints no server error message', () => {
    for (const f of FILES) {
      expect(text[f], f).not.toMatch(/dangerouslySetInnerHTML|innerHTML|insertAdjacentHTML/)
      expect(text[f], f).not.toMatch(/localStorage|sessionStorage|document\.cookie/)
      expect(text[f], f).not.toMatch(/\bfetch\(/)
      expect(text[f], f).not.toMatch(/(err|error|e)\.message/)
    }
  })
  it('does no signing or proof handling', () => {
    for (const f of FILES) expect(text[f], f).not.toMatch(/crypto\.subtle|sha256|hmac|X-Actor-Proof|actorProof|\bsign\(/i)
  })
  it('makes no legal or jurisdiction-approval claim', () => {
    for (const f of FILES) expect(text[f], f).not.toMatch(/legally|fully licensed|is licensed|compliant|approved for operation|jurisdiction (is )?approved/i)
  })
  it('no float math: no parseInt / Number / parseFloat / toFixed', () => {
    for (const f of FILES) expect(text[f], f).not.toMatch(/\bNumber\s*\(|parseInt|parseFloat|BigInt\s*\(|\.toFixed|Math\.(round|floor|ceil)/)
  })
  it('the author form starts empty: no preselected events, gate, time, reference, reason or acknowledgement', () => {
    const t = text['ScreeningPage.tsx']
    for (const re of [
      /\[events, setEvents\] = useState<TriggerEvent\[\]>\(\[\]\)/,
      /\[gate, setGate\] = useState<'' \| 'off' \| 'on'>\(''\)/,
      /\[secs, setSecs\] = useState\(''\)/,
      /\[from, setFrom\] = useState\(''\)/,
      /\[legal, setLegal\] = useState\(''\)/,
      /\[reason, setReason\] = useState\(''\)/,
      /\[ack, setAck\] = useState\(false\)/,
      /\[outcome, setOutcome\] = useState<ResolutionOutcome \| ''>\(''\)/,
    ]) expect(t).toMatch(re)
  })
  it('the UI never offers the error outcome and never sends a tenant or actor field in a body', () => {
    expect(text['ScreeningLabels.ts']).not.toMatch(/error:\s*'/)
    const api = code(readFileSync(resolve(process.cwd(), 'src/api/amlScreening.ts'), 'utf8'))
    expect(api).not.toMatch(/tenant_id\s*:|actor_id\s*:|person_id\s*:/)
  })
  it('path ids are encoded', () => {
    const api = readFileSync(resolve(process.cwd(), 'src/api/amlScreening.ts'), 'utf8')
    expect(api.match(/\$\{(?!encodeURIComponent|buildQuery)[^}]*\}/g) ?? []).toEqual([])
  })
})
