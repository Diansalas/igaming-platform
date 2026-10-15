import { Component, type ErrorInfo, type ReactNode } from 'react'

interface Props {
  children: ReactNode
  /** Changing this value (e.g. the route path) resets a tripped boundary so navigating away recovers. */
  resetKey?: string
}

interface State {
  failed: boolean
  resetKey?: string
}

/**
 * Last-resort render-error boundary. It shows a FIXED message: the thrown error's text and stack are never rendered
 * (they may contain data from a response) and never logged from here, so a render failure cannot leak a token, a
 * password or server text. Per-request failures are still handled where they happen; this only keeps one broken screen
 * from blanking the whole console.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { failed: false, resetKey: this.props.resetKey }

  static getDerivedStateFromError(): Partial<State> {
    return { failed: true }
  }

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    if (props.resetKey !== state.resetKey) return { failed: false, resetKey: props.resetKey }
    return null
  }

  componentDidCatch(_error: Error, _info: ErrorInfo): void {
    // Deliberately empty: see the class comment. A real error-reporting hook would redact before sending.
  }

  render() {
    if (!this.state.failed) return this.props.children
    return (
      <div role="alert" className="mx-auto mt-16 max-w-md rounded-lg border border-red-200 bg-red-50 p-6 text-center">
        <h2 className="text-base font-semibold text-red-800">Something went wrong on this screen</h2>
        <p className="mt-2 text-sm text-red-700">
          The rest of the console is unaffected. Reload the page, or go back and try again. If it keeps happening, tell your administrator.
        </p>
        <button
          type="button"
          className="mt-4 rounded-md border border-border bg-white px-3 py-2 text-sm font-medium text-slate-700 hover:bg-surface-alt"
          onClick={() => window.location.reload()}
        >
          Reload
        </button>
      </div>
    )
  }
}
