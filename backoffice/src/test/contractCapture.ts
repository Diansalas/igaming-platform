import { http, HttpResponse } from 'msw'
import { server } from './mswServer'
import type { SeenRequest } from './openapiLite'

/**
 * Installs one catch-all handler for /v1/admin/* that records every request exactly as the UI api module sent it
 * (method, path, query, parsed JSON body, headers) and answers with `reply`. Used by the per-module UI-to-OpenAPI
 * contract tests (A10b).
 */
export function captureRequests(reply: (req: SeenRequest) => { status?: number; body?: unknown } = () => ({ body: {} })) {
  const seen: Array<SeenRequest & { headers: Headers }> = []
  server.use(
    http.all('*/v1/admin/*', async ({ request }) => {
      const url = new URL(request.url)
      const text = await request.text()
      const rec = {
        method: request.method,
        pathname: url.pathname,
        query: url.searchParams,
        body: text ? (JSON.parse(text) as unknown) : undefined,
        headers: request.headers,
      }
      seen.push(rec)
      const r = reply(rec)
      if (r.status === 204) return new HttpResponse(null, { status: 204 })
      return HttpResponse.json(r.body ?? {}, { status: r.status ?? 200 })
    }),
  )
  return seen
}
