// 浏览器端到端用例（docs/05 §7）：globalSetup 启动开发栈与两个前端，用例跑真实的控制面、节点与假出口。
// 用本机安装的 Chrome（channel: 'chrome'），不下载浏览器。
import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './specs',
  globalSetup: './global-setup.ts',
  // 共用一套开发栈与一个管理员（TOTP 每个时间步只能用一次），串行跑
  workers: 1,
  fullyParallel: false,
  timeout: 150_000,
  expect: { timeout: 15_000 },
  retries: 0,
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'playwright-report' }]],
  outputDir: 'test-results',
  use: {
    ...devices['Desktop Chrome'],
    channel: 'chrome',
    headless: true,
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    permissions: ['clipboard-read', 'clipboard-write'],
  },
})
