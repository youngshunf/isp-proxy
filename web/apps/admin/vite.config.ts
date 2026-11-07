import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// 管理后台独立部署在 admin 子域名（docs/03 §6），开发端口与用户前台（5173）分开。
// 开发时 /admin-api 代理到 isp-control（默认 :8080，ISP_CONTROL_URL 可改；一键开发栈见 cmd/isp-devstack）。
const control = process.env.ISP_CONTROL_URL ?? 'http://127.0.0.1:8080'
const proxy = { '/admin-api': { target: control } }

export default defineConfig({
  plugins: [react()],
  server: { port: Number(process.env.ADMIN_PORT) || 5174, host: '127.0.0.1', strictPort: true, proxy },
  preview: { port: Number(process.env.ADMIN_PORT) || 4174, host: '127.0.0.1', strictPort: true, proxy },
  // ECharts（柱图 + SVG 渲染器）单独成包，约 530 kB / gzip 181 kB，只在打开用量趋势时按需加载
  build: { chunkSizeWarningLimit: 600 },
  test: { environment: 'jsdom', setupFiles: ['./src/test/setup.ts'], testTimeout: 15_000 },
})
