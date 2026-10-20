import { readFileSync, writeFileSync, existsSync } from 'node:fs'
const [inFile, outDir, rc] = process.argv.slice(2)
const rows = []
if (existsSync(inFile)) {
  const walk = (s, file) => {
    for (const sp of s.specs ?? []) for (const t of sp.tests ?? []) {
      const r = t.results?.at(-1)
      rows.push({ file: sp.file ?? file, title: sp.title, status: t.status === 'skipped' ? 'skipped' : r?.status ?? t.status, ms: r?.duration ?? 0, error: r?.error?.message?.split('\n')[0] ?? null })
    }
    for (const c of s.suites ?? []) walk(c, s.file ?? file)
  }
  for (const s of JSON.parse(readFileSync(inFile, 'utf8')).suites ?? []) walk(s, s.file)
}
const count = (k) => rows.filter((r) => r.status === k).length
const summary = { exitCode: Number(rc), total: rows.length, passed: count('passed'), failed: rows.filter((r) => ['failed', 'timedOut', 'interrupted'].includes(r.status)).length, skipped: count('skipped'), tests: rows }
writeFileSync(`${outDir}/summary.json`, JSON.stringify(summary, null, 2))
writeFileSync(`${outDir}/summary.md`, `# Back Office real-stack e2e\n\nExit ${rc}. passed ${summary.passed}, failed ${summary.failed}, skipped/fixme ${summary.skipped}, total ${summary.total}.\n\n| Spec | Test | Status | ms |\n|---|---|---|---|\n` + rows.map((r) => `| ${r.file} | ${r.title}${r.error ? ' (' + r.error.replace(/\|/g, '/') + ')' : ''} | ${r.status} | ${r.ms} |`).join('\n') + '\n')
