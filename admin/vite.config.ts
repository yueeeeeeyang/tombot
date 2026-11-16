import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { readFileSync } from 'node:fs'

/** 无脚本登录页也复用主题与控件源文件，固定资源路径由 tomd 同源提供。 */
function loginAssets() {
  return {
    name: 'tombot-login-assets',
    generateBundle() {
      const source = (path: string) => readFileSync(new URL(path, import.meta.url), 'utf8')
      const theme = source('../ui/theme.css')
      // 登录页没有脚本设置 data-theme，按浏览器系统偏好应用同一组深色变量。
      const systemDark = theme.replace(':root[data-theme="dark"]', ':root')
      this.emitFile({
        type: 'asset',
        fileName: 'assets/login.css',
        source:
          theme +
          '\n@media (prefers-color-scheme: dark) {\n' +
          systemDark +
          '\n}\n' +
          source('../ui/controls.css') +
          '\n' +
          source('../ui/browser-login.css'),
      })
      this.emitFile({
        type: 'asset',
        fileName: 'assets/user-platform.css',
        source:
          theme +
          '\n@media (prefers-color-scheme: dark) {\n' +
          systemDark.replaceAll(':root', ':root:not([data-theme])') +
          '\n}\n' +
          source('../ui/user-platform.css'),
      })
      for (const name of ['user-platform', 'user-profile', 'avatar-crop', 'profile-save']) {
        this.emitFile({
          type: 'asset',
          fileName: `assets/${name}.js`,
          source: source(`../ui/${name}.js`),
        })
      }
      this.emitFile({
        type: 'asset',
        fileName: 'assets/login-logo.png',
        source: readFileSync(
          new URL('../desktop/src/renderer/src/assets/cats/frames/bombay.png', import.meta.url),
        ),
      })
    },
  } satisfies import('vite').Plugin
}

export default defineConfig({
  resolve: { dedupe: ['react', 'react-dom', 'lucide-react'] },
  base: '/admin/',
  plugins: [react(), loginAssets()],
  build: { outDir: '../server/web/admin/bundle/dist', emptyOutDir: true, sourcemap: false },
})
