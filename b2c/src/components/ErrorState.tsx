import { ApiError } from '../api/types'
import { Button } from './Button'

/** Renders any thrown error consistently, including the API's request_id for support correlation. */
export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const message = error instanceof Error ? error.message : 'Something went wrong.'
  const requestId = error instanceof ApiError ? error.requestId : undefined

  return (
    <div className="flex flex-col items-center gap-3 rounded-lg border border-red-200 bg-red-50 p-6 text-center">
      <p className="text-sm font-medium text-red-800">{message}</p>
      {requestId && <p className="text-xs text-red-500">Request ID: {requestId}</p>}
      {onRetry && (
        <Button variant="secondary" onClick={onRetry}>
          Try again
        </Button>
      )}
    </div>
  )
}
