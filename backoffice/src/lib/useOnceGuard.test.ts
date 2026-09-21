import { act, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { useOnceGuard } from './useOnceGuard'

describe('useOnceGuard', () => {
  it('runs the action on the first call', () => {
    const { result } = renderHook(() => useOnceGuard())
    const action = vi.fn()

    act(() => result.current.run(action))

    expect(action).toHaveBeenCalledTimes(1)
  })

  it('ignores a second call before release() - the synchronous double-submit race this hook exists to close', () => {
    const { result } = renderHook(() => useOnceGuard())
    const action = vi.fn()

    act(() => {
      result.current.run(action)
      result.current.run(action)
    })

    expect(action).toHaveBeenCalledTimes(1)
  })

  it('allows another run after release()', () => {
    const { result } = renderHook(() => useOnceGuard())
    const action = vi.fn()

    act(() => result.current.run(action))
    act(() => result.current.release())
    act(() => result.current.run(action))

    expect(action).toHaveBeenCalledTimes(2)
  })

  it('stays guarded if release() is never called (e.g. a mutation that never settles)', () => {
    const { result } = renderHook(() => useOnceGuard())
    const action = vi.fn()

    act(() => {
      result.current.run(action)
      result.current.run(action)
      result.current.run(action)
    })

    expect(action).toHaveBeenCalledTimes(1)
  })
})
