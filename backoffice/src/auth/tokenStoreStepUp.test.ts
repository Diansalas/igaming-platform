import { describe, expect, it, vi } from 'vitest'
import { makeTestJwt } from '../test/jwt'
import { getAccessToken, getRefreshToken, InsufficientAssuranceError, replaceAccessToken, setSession } from './tokenStore'

describe('replaceAccessToken (ADR 0119 S7b step-up)', () => {
  it('swaps only the access token and keeps the refresh token', () => {
    setSession(makeTestJwt({ jti: 'a' }), 'refresh-1')
    const next = makeTestJwt({ jti: 'b' })
    replaceAccessToken(next)
    expect(getAccessToken()).toBe(next)
    expect(getRefreshToken()).toBe('refresh-1')
  })
  it.each([
    ['not aal2', { aal: 1 }],
    ['another subject', { sub: 'someone-else' }],
  ])('fails closed for a token that is %s', (_n, claims) => {
    setSession(makeTestJwt({}), 'refresh-1')
    expect(() => replaceAccessToken(makeTestJwt(claims))).toThrow(InsufficientAssuranceError)
    expect(getAccessToken()).toBeNull()
    expect(getRefreshToken()).toBeNull()
  })
  it('refuses when there is no session and stores nothing', () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    expect(() => replaceAccessToken(makeTestJwt({}))).toThrow(InsufficientAssuranceError)
    expect(getAccessToken()).toBeNull()
    expect(setItem).not.toHaveBeenCalled()
    setItem.mockRestore()
  })
})
