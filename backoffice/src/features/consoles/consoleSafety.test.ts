import { readdirSync, readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// Static pins over the A10b console sources. They are regression aids (a grep, not a proof); the binding guarantees
// are the behavioural tests next to each screen and the server. They exist so that an innocent-looking edit that
// renders a server string raw, hashes in the browser, or puts a secret in a URL fails the build.
const SRC = resolve(process.cwd(), 'src')
const DIRS = ['features/consoles', 'features/payments', 'features/reconciliation', 'features/capability', 'features/resolutions']
const API = ['paymentAttempts', 'reconciliation', 'capabilityGrants', 'forceResolutions', 'holdResolutions', 'idempotency']

function sources(): Array<{ file: string; text: string }> {
  const out: Array<{ file: string; text: string }> = []
  for (const d of DIRS) {
    for (const f of readdirSync(join(SRC, d))) {
      if (/\.(ts|tsx)$/.test(f) && !/\.test\.(ts|tsx)$/.test(f)) out.push({ file: `${d}/${f}`, text: readFileSync(join(SRC, d, f), 'utf8') })
    }
  }
  for (const a of API) out.push({ file: `api/${a}.ts`, text: readFileSync(join(SRC, 'api', `${a}.ts`), 'utf8') })
  return out
}

// Comment lines may name a forbidden thing to explain why it is absent.
const code = (t: string) => t.split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')

describe('console source pins (A10b)', () => {
  const all = sources()

  it('finds the sources it is meant to scan (non-vacuous)', () => {
    expect(all.length).toBeGreaterThan(15)
    expect(all.some((s) => s.file === 'features/resolutions/DecisionDialogs.tsx')).toBe(true)
  })

  it('never renders HTML from a server string, never touches web storage, never calls fetch directly', () => {
    for (const s of all) {
      const c = code(s.text)
      expect(c, s.file).not.toMatch(/dangerouslySetInnerHTML|innerHTML|insertAdjacentHTML|document\.write/)
      expect(c, s.file).not.toMatch(/localStorage|sessionStorage|document\.cookie/)
      expect(c, s.file).not.toMatch(/\bfetch\(/)
    }
  })

  it('never prints a server error message (only the closed code map may read ApiError.message)', () => {
    for (const s of all) {
      if (s.file === 'features/consoles/errors.ts') continue
      expect(code(s.text), s.file).not.toMatch(/(err|error|e)\.message/)
    }
  })

  it('does no hashing or signing in the browser and no proof handling (the operator supplies the digest; the proof is server-side)', () => {
    for (const s of all) {
      const c = code(s.text)
      expect(c, s.file).not.toMatch(/crypto\.subtle|createHash|sha256|SHA-256\(|digest\(|hmac|\bsign\(/i)
      expect(c, s.file).not.toMatch(/X-Actor-Proof|actor_proof|actorProof|signature/i)
    }
  })

  it('has no field, key or URL that could carry a provider reference, merchant reference, evidence reference text or token', () => {
    for (const s of all) {
      const c = code(s.text)
      expect(c, s.file).not.toMatch(/\bprovider_reference\b|providerReference|merchant_reference|idempotency_key/)
      expect(c, s.file).not.toMatch(/evidence_reference\b(?!_present)/)
      expect(c, s.file).not.toMatch(/[?&](token|access_token|refresh_token|password|secret)=/i)
      expect(c, s.file).not.toMatch(/getAccessToken|getRefreshToken/)
    }
  })

  it('never sends a client-chosen tenant id in a body or query (the tenant is the path value only)', () => {
    for (const s of all) {
      if (!s.file.startsWith('api/')) continue
      expect(code(s.text), s.file).not.toMatch(/JSON\.stringify\([^)]*tenant_id/)
      expect(code(s.text), s.file).not.toMatch(/buildQuery\([^)]*tenant_id/)
    }
  })

  it('has no button for a transition no route performs on reconciliation findings (detection only)', () => {
    const recon = all.find((s) => s.file === 'features/reconciliation/ReconciliationPage.tsx')!
    expect(code(recon.text)).not.toMatch(/method:\s*'(POST|PUT|PATCH|DELETE)'|useMutation/)
  })

  it('uses no floating point for money', () => {
    for (const s of all) {
      expect(code(s.text), s.file).not.toMatch(/parseFloat|Number\(\s*\w*(amount|minor)/i)
    }
  })
})
