import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ErrorBoundary } from './ErrorBoundary'

function Bomb(): never {
  throw new Error('boom')
}

describe('ErrorBoundary', () => {
  let consoleErrorSpy: ReturnType<typeof vi.spyOn>
  let reloadSpy: ReturnType<typeof vi.fn>

  beforeEach(() => {
    // React logs the caught error to the console itself (in addition to
    // this boundary's own componentDidCatch log) - expected noise for
    // this specific test, not something to assert on.
    consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    reloadSpy = vi.fn()
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { ...window.location, reload: reloadSpy },
    })
  })

  afterEach(() => {
    consoleErrorSpy.mockRestore()
  })

  it('renders a recoverable fallback instead of unmounting the whole app on a render error', async () => {
    render(
      <ErrorBoundary>
        <Bomb />
      </ErrorBoundary>,
    )

    expect(await screen.findByText('Something went wrong.')).toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Reload page' }))
    expect(reloadSpy).toHaveBeenCalled()
  })

  it('renders children normally when nothing throws', () => {
    render(
      <ErrorBoundary>
        <div>All good</div>
      </ErrorBoundary>,
    )
    expect(screen.getByText('All good')).toBeInTheDocument()
  })
})
