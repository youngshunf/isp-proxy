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
  // antd 与 React 系依赖拆成独立的长期缓存块，业务代码改动不让用户重新下载它们；
  // 最大的块是 antd 本身（约 760 kB，gzip 约 240 kB），告警阈值按它放宽
  build: {
    chunkSizeWarningLimit: 800,
    rolldownOptions: {
      output: {
        codeSplitting: {
          groups: [
            { name: 'antd', test: /node_modules[\\/](antd|@ant-design|@rc-component|rc-[^\\/]+)[\\/]/ },
            { name: 'react', test: /node_modules[\\/](react|react-dom|react-router|scheduler|@tanstack)[\\/]/ },
          ],
        },
      },
    },
  },
  test: { environment: 'jsdom', setupFiles: ['./src/test/setup.ts'], testTimeout: 15_000 },
})
