import { QueryClient } from '@tanstack/react-query'
import { ApiError } from '../api/types'

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: (failureCount, error) => {
        // Never retry an auth/permission/validation/not-found failure -
        // those are not transient. Retry a couple of times for anything
        // else (network blips, 5xx).
        if (error instanceof ApiError) {
          if (['unauthorized', 'forbidden', 'not_found', 'validation_error', 'conflict'].includes(error.code)) {
            return false
          }
        }
        return failureCount < 2
      },
      refetchOnWindowFocus: false,
    },
    mutations: {
      retry: false,
    },
  },
})
