import { afterEach, describe, expect, it, vi } from 'vitest'
import { captureEnrolLinkToken, clearEnrolLinkToken, getEnrolLinkToken } from './enrolLink'

afterEach(clearEnrolLinkToken)

describe('captureEnrolLinkToken', () => {
  it('reads the token from /#/enrol?token= and strips it from the address bar', () => {
    const replaceState = vi.fn()
    captureEnrolLinkToken({ pathname: '/', search: '', hash: '#/enrol?token=abc_DEF-1' }, { replaceState })
    expect(getEnrolLinkToken()).toBe('abc_DEF-1')
    expect(replaceState).toHaveBeenCalledWith(null, '', '/enrol')
  })
  it('does NOT accept the /enrol?token= path form: no token kept, URL rewritten', () => {
    const replaceState = vi.fn()
    captureEnrolLinkToken({ pathname: '/enrol', search: '?token=xyz', hash: '' }, { replaceState })
    expect(getEnrolLinkToken()).toBeNull()
    expect(replaceState).toHaveBeenCalledWith(null, '', '/enrol')
  })
  it('an enrol link with no token still strips the URL and stores nothing', () => {
    const replaceState = vi.fn()
    captureEnrolLinkToken({ pathname: '/', search: '', hash: '#/enrol' }, { replaceState })
    expect(getEnrolLinkToken()).toBeNull()
    expect(replaceState).toHaveBeenCalled()
  })
  it('leaves every other URL alone', () => {
    const replaceState = vi.fn()
    captureEnrolLinkToken({ pathname: '/login', search: '?token=nope', hash: '' }, { replaceState })
    expect(getEnrolLinkToken()).toBeNull()
    expect(replaceState).not.toHaveBeenCalled()
  })
  it('works against the real window.location and history', () => {
    window.history.replaceState(null, '', '/#/enrol?token=real-tok')
    captureEnrolLinkToken()
    expect(getEnrolLinkToken()).toBe('real-tok')
    expect(window.location.href).not.toContain('real-tok')
    expect(window.location.pathname).toBe('/enrol')
    window.history.replaceState(null, '', '/')
  })
})
