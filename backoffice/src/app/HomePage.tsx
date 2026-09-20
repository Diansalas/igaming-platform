import { Link } from 'react-router-dom'
import { useAuth } from '../auth/AuthContext'
import { buildNavItems } from '../layout/nav'
import { PageHeader } from '../components/PageHeader'

export function HomePage() {
  const { claims } = useAuth()
  const items = buildNavItems(claims)

  return (
    <div>
      <PageHeader title="Welcome" description={`Signed in as ${claims?.role ?? 'unknown role'}.`} />
      <ul className="flex flex-col gap-2">
        {items.map((item) => (
          <li key={item.to}>
            <Link to={item.to} className="text-brand-700 hover:underline">
              {item.label}
            </Link>
          </li>
        ))}
      </ul>
    </div>
  )
}
