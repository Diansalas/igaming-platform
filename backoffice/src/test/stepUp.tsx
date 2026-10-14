// Shared fixtures for the step-up tests (ADR 0119 S7b): a guarded endpoint that answers 403 step_up_required
// until the request carries the stepped-up access token, a mocked POST /v1/staff/auth/step-up, and a harness
// component that fires requests through apiFetch the way every feature does.
import { http, HttpResponse, delay } from 'msw'
import { useState } from 'react'
import { apiFetch } from '../api/client'
import { staffStepUp } from '../api/auth'
import { ApiError } from '../api/types'
import { makeTestJwt } from './jwt'
import { server } from './mswServer'

export const STEPPED_JWT = makeTestJwt({ jti: 'stepped-up-token' })
export const GOOD_CODE = '123456'
export const SERVER_MESSAGE_SENTINEL = 'SERVER-MESSAGE-SENTINEL-do-not-render'

export function stepUpRequiredResponse(maxAge = 300) {
  return HttpResponse.json(
    { code: 'step_up_required', message: SERVER_MESSAGE_SENTINEL, request_id: 'req-su', max_age: maxAge },
    {
      status: 403,
      headers: {
        'WWW-Authenticate': `Bearer realm="platform-api", error="insufficient_user_authentication", max_age="${maxAge}"`,
      },
    },
  )
}

export interface Recorded {
  auth: string | null
  body: string
  idempotencyKey: string | null
}

export interface Fixture {
  guarded: Recorded[]
  stepUps: string[]
  stepUpAuth: Array<string | null>
  refreshCalls: number
}

/** Installs the guarded endpoint and the step-up endpoint. `stepUp` overrides the step-up answer. */
export function installStepUpFixture(
  opts: {
    alwaysRefuse?: boolean
    stepUp?: (code: string) => Response | Promise<Response>
    guardedDelayMs?: number
    maxAge?: number
  } = {},
): Fixture {
  const f: Fixture = { guarded: [], stepUps: [], stepUpAuth: [], refreshCalls: 0 }
  server.use(
    http.post('/v1/admin/thing', async ({ request }) => {
      const body = await request.text()
      const auth = request.headers.get('Authorization')
      f.guarded.push({ auth, body, idempotencyKey: request.headers.get('Idempotency-Key') })
      if (opts.guardedDelayMs) await delay(opts.guardedDelayMs)
      if (!opts.alwaysRefuse && auth === `Bearer ${STEPPED_JWT}`) return HttpResponse.json({ done: true })
      return stepUpRequiredResponse(opts.maxAge)
    }),
    http.post('/v1/staff/auth/step-up', async ({ request }) => {
      const { code } = (await request.json()) as { code: string }
      f.stepUps.push(code)
      f.stepUpAuth.push(request.headers.get('Authorization'))
      if (opts.stepUp) return opts.stepUp(code)
      if (code !== GOOD_CODE) return HttpResponse.json({ code: 'invalid_mfa_code', message: SERVER_MESSAGE_SENTINEL }, { status: 401 })
      return HttpResponse.json({
        access_token: STEPPED_JWT,
        token_type: 'Bearer',
        expires_in: 900,
        mfa_at: '2026-10-13T10:00:00Z',
        step_up_expires_at: { financial: '2026-10-13T10:10:00Z', critical: '2026-10-13T10:05:00Z' },
      })
    }),
    http.post('/v1/auth/refresh', () => {
      f.refreshCalls++
      return HttpResponse.json({ code: 'unauthorized', message: 'no' }, { status: 401 })
    }),
  )
  return f
}

export function errorBody(code: string, status: number, headers?: Record<string, string>) {
  return HttpResponse.json({ code, message: SERVER_MESSAGE_SENTINEL }, { status, headers })
}

/** Fires guarded POSTs through apiFetch and lists each outcome as text. */
export function Harness() {
  const [results, setResults] = useState<string[]>([])
  async function run(i: number) {
    try {
      await apiFetch('/v1/admin/thing', {
        method: 'POST',
        body: JSON.stringify({ i }),
        headers: { 'Idempotency-Key': `key-${i}` },
      })
      setResults((r) => [...r, `ok ${i}`])
    } catch (e) {
      const err = e as ApiError
      setResults((r) => [...r, `err ${i} ${err.code}: ${err.message}`])
    }
  }
  return (
    <div>
      <button onClick={() => void run(1)}>Run</button>
      <button
        onClick={() => {
          void run(1)
          void run(2)
          void run(3)
        }}
      >
        Run all
      </button>
      <button
        onClick={() => {
          // Direct call of the step-up endpoint wrapper (no modal): for the no-recursion test.
          staffStepUp('000000').catch((e: ApiError) => setResults((r) => [...r, `direct ${e.code}`]))
        }}
      >
        Direct
      </button>
      <ul aria-label="results">
        {results.map((r, i) => (
          <li key={i}>{r}</li>
        ))}
      </ul>
    </div>
  )
}
