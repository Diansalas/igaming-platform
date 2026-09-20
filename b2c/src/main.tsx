import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './app/App'
import { brandConfig } from './config/brand'
import './index.css'

// Applies this deployment's brand color on top of the default CSS
// variables (src/index.css) - the one, deliberately narrow, "theming"
// mechanism this MVP has. A second brand deployment changes
// VITE_BRAND_PRIMARY_COLOR in its own .env file; this is the only code
// that reads it to affect visuals, and it never branches on brand slug.
document.documentElement.style.setProperty('--color-brand-600', brandConfig.primaryColorHex)
document.documentElement.style.setProperty('--color-brand-700', brandConfig.primaryColorHoverHex)
document.title = brandConfig.displayName

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
