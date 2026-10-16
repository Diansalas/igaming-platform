// Client-side idempotency key for the governed four-eyes mutations (ADR 0114 S7/S8).
//
// A10c: the K1, K3 and HSEC mutating routes now honour an OPTIONAL server-side Idempotency-Key (migration 0164,
// staff_idempotency.go): the same key with the same request replays the stored 2xx response without acting again
// (Idempotent-Replayed: true), the same key with a different request is 409 IDEMPOTENCY_MISMATCH, and a request still
// running is 409 IDEMPOTENCY_IN_PROGRESS. The key is generated once when a dialog is opened and reused for every send
// of that dialog, including the single re-send after a step-up (client.ts reuses the same options object), so the
// re-send is byte-identical and a lost response is safely retried. The key grants nothing: step-up, permissions and
// the database guards still decide first, and the synchronous once-guard in the dialogs stays.
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
