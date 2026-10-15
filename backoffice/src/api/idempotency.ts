// Client-side idempotency key for the governed four-eyes mutations (ADR 0114 S7/S8).
//
// HONEST SCOPE: the K1, K3 and HSEC routes do not read an Idempotency-Key header today (no occurrence in
// capability_routes.go, payment_force_resolution_routes.go or withdrawal_hold_resolution_routes.go; the admin
// four-eyes routes are idempotent by state, see launch_authorisation_routes.go:27). The key is generated once when a
// dialog is opened and reused for every send of that dialog, including the single re-send after a step-up
// (client.ts reuses the same options object), so a future server-side dedupe works without a UI change and the
// re-send is byte-identical. The REAL double-submit protection today is the synchronous once-guard in the
// dialogs plus the database state refusals (a repeated approve is 409), which the screens map and never hide.
export function newIdempotencyKey(): string {
  const c = globalThis.crypto
  if (c && typeof c.randomUUID === 'function') return c.randomUUID()
  const b = new Uint8Array(16)
  c.getRandomValues(b)
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')
}

export function idempotencyHeaders(key: string | undefined): Record<string, string> | undefined {
  return key ? { 'Idempotency-Key': key } : undefined
}
