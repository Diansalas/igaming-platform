import { Link } from 'react-router-dom'
import { useAuth } from '../auth/AuthContext'
import { Button } from '../components/Button'
import { brandConfig } from '../config/brand'

/** Public landing page - server-rendering for SEO is a future concern once this app moves to a framework with SSR; this MVP is a client-rendered SPA (see the Stage 6 report's disclosed scope note). */
export function HomePage() {
  const { isAuthenticated } = useAuth()

  return (
    <div className="flex flex-col gap-8">
      <section className="rounded-lg border border-border bg-surface p-8 text-center sm:p-12">
        <h1 className="text-2xl font-bold text-slate-900 sm:text-3xl">{brandConfig.displayName}</h1>
        <p className="mt-2 text-slate-600">Sports betting, built on our own platform.</p>
        <div className="mt-6 flex flex-wrap justify-center gap-3">
          <Link to="/sports">
            <Button>Browse sports</Button>
          </Link>
          {!isAuthenticated && (
            <Link to="/register">
              <Button variant="secondary">Create an account</Button>
            </Link>
          )}
        </div>
      </section>
    </div>
  )
}
