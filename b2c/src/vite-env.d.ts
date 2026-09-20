/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_API_BASE_URL?: string
  readonly VITE_BRAND_SLUG?: string
  readonly VITE_BRAND_DISPLAY_NAME?: string
  readonly VITE_BRAND_PRIMARY_COLOR?: string
  readonly VITE_BRAND_PRIMARY_COLOR_HOVER?: string
  readonly VITE_BRAND_DEFAULT_ASSET?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
