import { Component, type ErrorInfo, type ReactNode } from 'react'
import { Button } from '../components/Button'

interface Props {
  children: ReactNode
}

interface State {
  error: Error | null
}

/**
 * Top-level render-error boundary (Stage 9 production hardening finding).
 *
 * Before this existed, an unhandled exception thrown during render/commit
 * ANYWHERE in the tree - a bug, an unexpected null/shape from an API
 * response this page's types didn't anticipate - unmounted the entire
 * React tree, leaving a blank white page with no way back and no
 * explanation. That is strictly worse than every other error path in this
 * app, all of which (ErrorState, form-level error banners) already render
 * a message and a way to recover.
 *
 * This is a LAST-RESORT safety net, not a substitute for the explicit
 * loading/error/empty states every data-fetching page already renders via
 * react-query + <ErrorState/>: a normal API error (4xx/5xx/network) never
 * reaches here, because every API call site catches it. This only ever
 * catches a genuine programming error.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // No client-side error-reporting/telemetry pipeline exists in this
    // MVP yet (a future PROVIDER DEPENDENT integration) - console logging
    // is the only avenue available today, so this stays visible to
    // whatever captures browser console output.
    // eslint-disable-next-line no-console
    console.error('Unhandled error in the application tree:', error, info.componentStack)
  }

  private handleReload = (): void => {
    window.location.reload()
  }

  render(): ReactNode {
    if (this.state.error) {
      return (
        <div className="flex h-screen flex-col items-center justify-center gap-4 p-6 text-center">
          <h1 className="text-lg font-semibold text-slate-900">Something went wrong.</h1>
          <p className="max-w-sm text-sm text-slate-600">
            An unexpected error occurred. Reloading the page usually fixes this. If it keeps happening, please
            contact support.
          </p>
          <Button onClick={this.handleReload}>Reload page</Button>
        </div>
      )
    }
    return this.props.children
  }
}
