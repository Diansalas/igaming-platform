// One-time display of recovery codes (ADR 0119 section 15 items 2 and 4): used by enrolment and by
// regeneration on "My security".
//
// The codes arrive as a prop and are never copied into this component's state, a query cache, Web Storage or a
// log. There are NO timers anywhere in here (S7a review F1: a timer that wiped the codes after confirm lost them);
// the codes stay until the parent drops them. While they are shown and not acknowledged, the browser is asked to
// confirm before the page is left (S7a review F4), because they cannot be shown again.
import { useEffect, useState } from 'react'
import { Button } from '../components/Button'

export function formatRecoveryCodesFile(codes: string[]): string {
  return [
    'Back office recovery codes',
    'Each code can be used once. Store them somewhere safe and private.',
    '',
    ...codes,
    '',
  ].join('\n')
}

interface Props {
  codes: string[]
  acked: boolean
  onAckChange: (acked: boolean) => void
}

export function RecoveryCodesDisplay({ codes, acked, onAckChange }: Props) {
  const [notice, setNotice] = useState<string | null>(null)

  useEffect(() => {
    if (acked) return
    const h = (e: BeforeUnloadEvent) => {
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', h)
    return () => window.removeEventListener('beforeunload', h)
  }, [acked])

  async function copyCodes() {
    try {
      await navigator.clipboard.writeText(codes.join('\n'))
      setNotice('Recovery codes copied to the clipboard. Clear your clipboard after saving them.')
    } catch {
      setNotice('Copy failed. Select the codes and copy them manually.')
    }
  }

  function downloadCodes() {
    const blob = new Blob([formatRecoveryCodesFile(codes)], { type: 'text/plain' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'recovery-codes.txt'
    document.body.appendChild(a)
    a.click()
    a.remove()
    // Revoke after the browser has started the download.
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }

  return (
    <>
      <ol aria-label="Recovery codes" className="grid grid-cols-2 gap-1 rounded-md border border-border bg-surface-alt p-3 font-mono text-sm">
        {codes.map((c) => (
          <li key={c} className="select-all">
            {c}
          </li>
        ))}
      </ol>
      <div className="flex gap-2">
        <Button type="button" variant="secondary" onClick={copyCodes}>
          Copy codes
        </Button>
        <Button type="button" variant="secondary" onClick={downloadCodes}>
          Download .txt
        </Button>
      </div>
      {notice && (
        <p role="status" className="text-sm text-slate-600">
          {notice}
        </p>
      )}
      <label className="flex items-start gap-2 text-sm text-slate-700">
        <input type="checkbox" checked={acked} onChange={(e) => onAckChange(e.target.checked)} className="mt-0.5" />
        <span>I have stored these codes</span>
      </label>
    </>
  )
}
