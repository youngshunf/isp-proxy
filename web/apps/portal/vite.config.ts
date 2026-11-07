import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// 开发时 /api 代理到 isp-control（默认 :8080，ISP_CONTROL_URL 可改；一键开发栈见 cmd/isp-devstack）。
// 会话 Cookie 只设在 /api 路径下，经代理同源访问即可，不需要改写 Cookie。
const control = process.env.ISP_CONTROL_URL ?? 'http://127.0.0.1:8080'
const proxy = { '/api': { target: control } }

export default defineConfig({
  plugins: [react()],
  server: { port: Number(process.env.PORTAL_PORT) || 5173, host: '127.0.0.1', strictPort: true, proxy },
  preview: { port: Number(process.env.PORTAL_PORT) || 4173, host: '127.0.0.1', strictPort: true, proxy },
  test: { environment: 'jsdom', setupFiles: ['./src/test/setup.ts'], testTimeout: 15_000 },
})
