import { defineConfig } from 'electron-vite'
import react from '@vitejs/plugin-react'
import { randomUUID } from 'node:crypto'

export default defineConfig(({ command }) => {
  const nonce = randomUUID()
  const isDevelopment = command === 'serve'
  return {
    main: {},
    // 沙盒预加载只能执行 CommonJS，不能跟随主进程输出 ESM。
    preload: {
      build: { rollupOptions: { output: { format: 'cjs', entryFileNames: 'index.cjs' } } },
    },
    renderer: {
      build: { minify: true },
      html: isDevelopment ? { cspNonce: nonce } : undefined,
      plugins: [
        react(),
        {
          name: 'tombot-development-csp',
          // 只为本次开发进程生成的热更新脚本放行 nonce，生产脚本策略保持严格。
          transformIndexHtml(html) {
            return isDevelopment
              ? html.replace("script-src 'self'", `script-src 'self' 'nonce-${nonce}'`)
              : html
          },
        },
      ],
    },
  }
})
