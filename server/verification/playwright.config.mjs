import { createRequire } from 'node:module'
import { isAbsolute } from 'node:path'

const require = createRequire(new URL('../../admin/package.json', import.meta.url))
const { defineConfig } = require('@playwright/test')
const outputDir = process.env.TOM_TEST_BROWSER_REPORT_DIR
if (!outputDir || !isAbsolute(outputDir)) {
  throw new Error('必须指定本次验收专用的绝对输出目录 TOM_TEST_BROWSER_REPORT_DIR')
}

export default defineConfig({
  testDir: '.',
  testMatch: 'shared-browser.spec.mjs',
  outputDir,
  workers: 1,
  timeout: 60_000,
  reporter: 'list',
  use: { trace: 'off' },
})
