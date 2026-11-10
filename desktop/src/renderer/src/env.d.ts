import type { DesktopBackend } from './features/desktop/backend'

declare global {
  interface Window {
    desktop?: {
      readonly platform: string
      readonly backend: DesktopBackend
    }
  }
}

export {}
