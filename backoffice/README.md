# Operator Back Office (Stage 5)

React + TypeScript + Vite front end for tenant/platform staff. Consumes
the Platform API only - no business logic (financial, RBAC, KYC/RG/bonus
decisions) is duplicated here; every mutating action is authorized and
audited server-side.

## Development

```bash
npm install
npm run dev
```

The dev server proxies `/v1/*` requests to the Platform API (see
`vite.config.ts`); set `VITE_API_PROXY_TARGET` if the API isn't running on
`http://localhost:8080`.

## Build

```bash
npm run build
```

## Tests

```bash
npm test
```

Uses Vitest + React Testing Library + MSW (mocked API layer - no test
hits a real backend).

## Layout

- `src/api/` - the only layer that knows about HTTP/JSON wire shapes.
- `src/auth/` - login, JWT decode (UI convenience only, never a security
  boundary), session storage, route guards.
- `src/components/` - generic, domain-agnostic UI primitives (Table,
  Pagination, Modal, ConfirmDialog, etc.).
- `src/features/` - one folder per domain (tenants, players, kyc, rg,
  bonus, withdrawals, audit); domain display logic lives here, not in
  `components/`.
- `src/layout/` - app shell (sidebar, top bar, role-aware nav).
