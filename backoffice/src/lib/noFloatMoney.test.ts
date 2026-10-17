// U-5 static guard (lint-style): production source must not use floating-point idioms that can touch money.
// CLAUDE.md: never floating point for money. Money goes through src/lib/amount.ts (BigInt / string arithmetic).
//
// Banned in every production .ts/.tsx file under src/ (tests and src/test/ excluded), comments ignored:
//   parseFloat(, Number.parseFloat(, Number(, parseInt(, Number.parseInt(, .toFixed(, Math.round/floor/ceil/trunc(,
//   `* 100` / `/ 100`, and `10 **` (float scaling by an exponent).
// The ONLY exceptions are the explicit, audited ALLOW entries below: each names the file, the exact (trimmed) source line
// and the reason it is not an amount. A new use anywhere else fails this test and must either be rewritten with
// lib/amount.ts or added here with a justification that a reviewer can check. Entries may not mention amount-like words.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const SRC = resolve(__dirname, '..')

const BANNED: { name: string; re: RegExp }[] = [
  { name: 'parseFloat', re: /\bparseFloat\s*\(/ },
  { name: 'Number.parseFloat', re: /Number\.parseFloat\s*\(/ },
  { name: 'Number(', re: /\bNumber\s*\(/ },
  { name: 'parseInt', re: /\bparseInt\s*\(|Number\.parseInt\s*\(/ },
  { name: 'toFixed', re: /\.toFixed\s*\(/ },
  { name: 'Math.round/floor/ceil/trunc', re: /\bMath\.(round|floor|ceil|trunc)\s*\(/ },
  { name: '* 100 or / 100', re: /[*/]\s*100\b/ },
  { name: '10 ** exponent', re: /\b10\s*\*\*/ },
]

interface Allow {
  file: string
  line: string
  reason: string
}

// BEGIN-ALLOW (audited non-money uses)
const ALLOW: Allow[] = [
  {
    file: 'api/client.ts',
    line: "const retryAfterRaw = Number(res.headers.get('Retry-After'))",
    reason: 'HTTP Retry-After header in seconds; not an amount.',
  },
  {
    file: 'api/client.ts',
    line: 'const retryAfter = Number.isFinite(retryAfterRaw) && retryAfterRaw > 0 ? Math.floor(retryAfterRaw) : undefined',
    reason: 'Whole seconds of the Retry-After header; not an amount.',
  },
  {
    file: 'api/client.ts',
    line: "const n = typeof body.max_age === 'number' ? body.max_age : Number(fromHeader)",
    reason: 'Step-up max_age in seconds from a header; not an amount.',
  },
  {
    file: 'auth/StepUpProvider.tsx',
    line: 'const minutes = Math.max(1, Math.round(maxAge / 60))',
    reason: 'Step-up freshness window in minutes for display; not an amount.',
  },
  {
    file: 'features/alerts/AlertsPage.tsx',
    line: 'limit: Number(limit),',
    reason: 'Page-size query parameter chosen from a fixed select; not an amount.',
  },
  {
    file: 'features/alerts/AlertsPage.tsx',
    line: 'const mayHaveMore = query.data !== undefined && alerts.length >= Number(limit)',
    reason: 'Page-size comparison against a row count; not an amount.',
  },
  {
    file: 'features/configuration/CasinoCapabilityForm.tsx',
    line: 'priority: Number(priority.trim()),',
    reason: 'Provider routing priority (small validated integer); not an amount.',
  },
  {
    file: 'features/configuration/PaymentCapabilityForm.tsx',
    line: 'priority: Number(priority.trim()),',
    reason: 'Provider routing priority (small validated integer); not an amount.',
  },
  {
    file: 'features/payments/ParkedPaymentsPage.tsx',
    line: 'min_age_seconds: minAge ? Number(minAge) : undefined,',
    reason: 'Age filter in seconds; not an amount.',
  },
  {
    file: 'features/sportsbook/status.ts',
    line: 'return (numerator / denominator).toFixed(2)',
    reason: 'Display of a decimal ODDS ratio (price), never a monetary amount and never sent back to the server.',
  },
  {
    file: 'lib/amount.ts',
    line: 'return Number(value) // exact: bounded to the safe-integer range just above',
    reason: 'toWireInteger: converts a bigint already proven to be within +/-(2^53-1) to the JSON number the int64 DTO needs; lossless.',
  },
  {
    file: 'lib/minorUnits.ts',
    line: 'let s = Math.max(0, Math.floor((now - t) / 1000))',
    reason: 'formatAge: whole seconds of elapsed time; not an amount.',
  },
  {
    file: 'lib/minorUnits.ts',
    line: 'const d = Math.floor(s / 86400)',
    reason: 'formatAge: whole days of elapsed time; not an amount.',
  },
  {
    file: 'lib/minorUnits.ts',
    line: 'const h = Math.floor(s / 3600)',
    reason: 'formatAge: whole hours of elapsed time; not an amount.',
  },
  {
    file: 'lib/minorUnits.ts',
    line: 'const m = Math.floor(s / 60)',
    reason: 'formatAge: whole minutes of elapsed time; not an amount.',
  },
]
// END-ALLOW

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) {
      if (name === 'test' || name === 'node_modules') continue
      walk(p, out)
    } else if (/\.(ts|tsx)$/.test(name) && !/\.test\.(ts|tsx)$/.test(name) && !/\.d\.ts$/.test(name)) {
      out.push(p)
    }
  }
  return out
}

function code(line: string): string {
  const t = line.trim()
  if (t.startsWith('//') || t.startsWith('*') || t.startsWith('/*')) return ''
  return line.replace(/\s\/\/.*$/, '')
}

function scan(): { file: string; line: string; rule: string }[] {
  const found: { file: string; line: string; rule: string }[] = []
  for (const f of walk(SRC)) {
    const rel = relative(SRC, f).split('\\').join('/')
    for (const raw of readFileSync(f, 'utf8').split('\n')) {
      const c = code(raw)
      if (!c.trim()) continue
      for (const b of BANNED) if (b.re.test(c)) found.push({ file: rel, line: raw.trim(), rule: b.name })
    }
  }
  return found
}

describe('no floating-point money idioms in production source (U-5)', () => {
  it('finds nothing outside the audited allow-list', () => {
    const unlisted = scan().filter((f) => !ALLOW.some((a) => a.file === f.file && a.line === f.line))
    expect(unlisted.map((f) => `${f.file}: [${f.rule}] ${f.line}`)).toEqual([])
  })

  it('has no stale allow-list entries (every entry still matches a real line)', () => {
    const found = scan()
    const stale = ALLOW.filter((a) => !found.some((f) => f.file === a.file && f.line === a.line))
    expect(stale.map((a) => `${a.file}: ${a.line}`)).toEqual([])
  })

  it('allow-list entries never concern amounts and always carry a reason', () => {
    const amountLike = /(amount|balance|stake|payout|wager|price|money|minor|cash|ceiling|limit_value|\bwin\b|funds|fee)/i
    for (const a of ALLOW) {
      expect(a.reason.length, a.file).toBeGreaterThan(10)
      expect(amountLike.test(a.line), `${a.file}: ${a.line}`).toBe(false)
    }
  })

  it('the scanner itself detects each banned pattern (self-test)', () => {
    const sample: Record<string, string> = {
      parseFloat: 'const x = parseFloat(amount)',
      'Number(': 'const x = Number(total)',
      parseInt: 'const x = parseInt(s, 10)',
      toFixed: 'v.toFixed(2)',
      'Math.round/floor/ceil/trunc': 'Math.round(v)',
      '* 100 or / 100': 'const c = a * 100',
      '10 ** exponent': 'const d = 10 ** e',
    }
    for (const [name, text] of Object.entries(sample)) {
      const hit = BANNED.some((b) => b.name === name && b.re.test(code(text)))
      expect(hit, name).toBe(true)
    }
    expect(code('// Number(x) in a comment')).toBe('')
    expect(code(' * Number(x) in a block comment')).toBe('')
  })
})
