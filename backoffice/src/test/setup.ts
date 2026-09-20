import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterAll, afterEach, beforeAll } from 'vitest'
import { clearSession } from '../auth/tokenStore'
import { server } from './mswServer'

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  cleanup()
  server.resetHandlers()
  sessionStorage.clear()
  // tokenStore is a module-level singleton (by design - see its own doc
  // comment) so a session established in one test must not leak into the
  // next.
  clearSession()
})
afterAll(() => server.close())
