import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './tests/e2e',
  workers: 1,
  timeout: 30_000,
  reporter: 'list',
  use: { baseURL: 'http://127.0.0.1:4177', viewport: { width: 1280, height: 900 }, trace: 'off' },
  webServer: {
    command:
      'cd ../server && go run ./internal/testkit/adminpreview -assets ../admin/dist -address 127.0.0.1:4177',
    url: 'http://127.0.0.1:4177/admin/',
    reuseExistingServer: false,
    timeout: 30_000,
  },
})
