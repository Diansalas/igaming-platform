import { describe, expect, it, vi } from 'vitest'
import { makeTestJwt } from '../test/jwt'
import { clearSession, getAccessToken, getRefreshToken, getSessionSnapshot, InsufficientAssuranceError, setSession } from './tokenStore'

describe('tokenStore (ADR 0119 S7a)', () => {
  it('accepts an aal2 session and keeps both tokens in memory only', () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    setSession(makeTestJwt({ aal: 2 }), 'refresh-secret')
    expect(getAccessToken()).not.toBeNull()
    expect(getRefreshToken()).toBe('refresh-secret')
    expect(setItem).not.toHaveBeenCalled()
    expect(localStorage.length + sessionStorage.length).toBe(0)
    setItem.mockRestore()
  })
  it.each([[1], [0], [3]])('refuses aal=%s and leaves no signed-in state', (aal) => {
    setSession(makeTestJwt({ aal: 2 }), 'old')
    expect(() => setSession(makeTestJwt({ aal }), 'new')).toThrow(InsufficientAssuranceError)
    expect(getSessionSnapshot().claims).toBeNull()
    expect(getAccessToken()).toBeNull()
    expect(getRefreshToken()).toBeNull()
  })
  it('refuses a token with no aal claim', () => {
    expect(() => setSession(makeTestJwt({ aal: undefined }), 'r')).toThrow(InsufficientAssuranceError)
    expect(getAccessToken()).toBeNull()
  })
  it('refuses an undecodable token', () => {
    expect(() => setSession('not-a-jwt', 'r')).toThrow(InsufficientAssuranceError)
    expect(getAccessToken()).toBeNull()
  })
  it('clearSession wipes the refresh token', () => {
    setSession(makeTestJwt({}), 'r')
    clearSession()
    expect(getRefreshToken()).toBeNull()
  })
})
