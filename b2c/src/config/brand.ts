// Stage 6 brand-awareness mechanism (per the directive: build-time env
// vars are acceptable for this MVP, a full runtime multi-brand theming
// engine is explicitly out of scope/future work).
//
// This is the ONLY place brand display config lives. Every component that
// wants a brand name, color, or default currency reads it from here, and
// NOWHERE else - a second brand's deployment is a different `.env` file
// (see `.env.example`) selecting a different `brand_slug`, DISPLAY_NAME,
// PRIMARY_COLOR and DEFAULT_ASSET, never a code change.
//
// `slug` is NOT a cosmetic value - it is the `brand_slug` sent on every
// register/login call (internal/httpserver/auth_routes.go's
// registerRequest/loginRequest), which is how the server resolves which
// tenant/brand this visitor belongs to. There is no client-side tenant
// concept beyond this string; the server is the sole authority on what
// tenant_id/brand_id that slug maps to (CLAUDE.md: "tenant_id is
// authoritative from server-side authenticated context only").
export interface BrandConfig {
  /** Sent as `brand_slug` on register/login - resolves the tenant server-side. */
  slug: string
  displayName: string
  /** Hex color applied to the `--color-brand-600`/`--color-brand-700` CSS variables at startup (see main.tsx). */
  primaryColorHex: string
  primaryColorHoverHex: string
  /** Pre-selected asset code for the deposit form and bet-slip defaults - a UX default only, never enforced client-side. */
  defaultAssetCode: string
}

const env = import.meta.env

export const brandConfig: BrandConfig = {
  slug: env.VITE_BRAND_SLUG ?? 'demo-casino',
  displayName: env.VITE_BRAND_DISPLAY_NAME ?? 'Demo Casino',
  primaryColorHex: env.VITE_BRAND_PRIMARY_COLOR ?? '#2563eb',
  primaryColorHoverHex: env.VITE_BRAND_PRIMARY_COLOR_HOVER ?? '#1d4ed8',
  defaultAssetCode: env.VITE_BRAND_DEFAULT_ASSET ?? 'USD',
}
