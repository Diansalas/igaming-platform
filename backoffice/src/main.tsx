import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './app/App'
import { captureEnrolLinkToken } from './auth/enrolLink'
import './index.css'

// Must run before the router renders: moves the one-time enrolment token out of the URL.
captureEnrolLinkToken()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
