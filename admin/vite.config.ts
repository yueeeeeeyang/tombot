import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  resolve: { dedupe: ['react', 'react-dom', 'lucide-react'] },
  base: '/admin/',
  plugins: [react()],
  build: { outDir: '../server/web/admin/bundle/dist', emptyOutDir: true, sourcemap: false },
})
