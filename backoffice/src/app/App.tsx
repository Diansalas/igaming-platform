import { QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter } from 'react-router-dom'
import { ErrorBoundary } from '../components/ErrorBoundary'
import { AuthProvider } from '../auth/AuthContext'
import { StepUpProvider } from '../auth/StepUpProvider'
import { AppRoutes } from './AppRoutes'
import { queryClient } from './queryClient'

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <BrowserRouter>
          <StepUpProvider>
            <ErrorBoundary>
              <AppRoutes />
            </ErrorBoundary>
          </StepUpProvider>
        </BrowserRouter>
      </AuthProvider>
    </QueryClientProvider>
  )
}
