import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, act } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { makeTestJwt } from '../test/jwt'
import { AuthProvider } from './AuthContext'
import { clearSession, setSession } from './tokenStore'

describe('AuthProvider cache hygiene (security F5)', () => {
  it('clears every cached query when the session ends', () => {
    const qc = new QueryClient()
    render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <MemoryRouter />
        </AuthProvider>
      </QueryClientProvider>,
    )
    act(() => setSession(makeTestJwt({}), 'r'))
    qc.setQueryData(['players'], { secret: 'other-user-data' })
    expect(qc.getQueryData(['players'])).toBeDefined()
    act(() => clearSession())
    expect(qc.getQueryData(['players'])).toBeUndefined()
  })

  it('clears the cache when an aal-less token is refused', () => {
    const qc = new QueryClient()
    render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <MemoryRouter />
        </AuthProvider>
      </QueryClientProvider>,
    )
    act(() => setSession(makeTestJwt({}), 'r'))
    qc.setQueryData(['players'], { x: 1 })
    act(() => {
      expect(() => setSession(makeTestJwt({ aal: 1 }), 'r2')).toThrow()
    })
    expect(qc.getQueryData(['players'])).toBeUndefined()
  })
})
