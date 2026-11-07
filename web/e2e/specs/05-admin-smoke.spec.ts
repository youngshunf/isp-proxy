// 管理后台冒烟：真实数据下逐个打开导航里的页面，页面标题出现、没有加载失败提示、控制台没有错误、接口没有 4xx/5xx。
import { expect, test, type Page } from '@playwright/test'
import { adminLogin } from '../support/flows'
import { stack } from '../support/stack'

function watch(page: Page) {
  const problems: string[] = []
  page.on('console', (m) => {
    if (m.type() === 'error') problems.push(`console: ${m.text()}`)
  })
  page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`))
  page.on('response', (r) => {
    if (r.url().includes('/admin-api/') && r.status() >= 400) problems.push(`${r.status()} ${r.request().method()} ${new URL(r.url()).pathname}`)
  })
  return problems
}

test('后台导航页面逐个打开无错误', async ({ page }) => {
  const st = stack()
  await adminLogin(page)
  const problems = watch(page)
  const nav = page.getByRole('navigation').first()
  const links = await nav.getByRole('link').evaluateAll((as) => as.map((a) => [a.textContent?.replace(/\d+$/, '').trim() ?? '', a.getAttribute('href') ?? '']))
  expect(links.length).toBeGreaterThanOrEqual(11)
  for (const [label, href] of links) {
    await test.step(`${label}（${href}）`, async () => {
      await page.goto(`${st.adminUrl}${href}`)
      await expect(page.getByRole('heading', { level: 1 }).first()).toBeVisible()
      // 等数据请求落定，再检查“加载失败”类提示
      await page.waitForLoadState('networkidle')
      await expect(page.getByText(/没有加载出来|加载失败|读取.*失败/)).toHaveCount(0)
      expect(problems, `${label} 页面出现问题`).toEqual([])
    })
  }

  await test.step('详情页：订单、订阅、用户', async () => {
    for (const [list, rowLink] of [
      ['/orders', /\/orders\/[^/]+$/],
      ['/subscriptions', /\/subscriptions\/[^/]+$/],
      ['/users', /\/users\/[^/]+$/],
    ] as const) {
      await page.goto(`${st.adminUrl}${list}`)
      await page.waitForLoadState('networkidle')
      const hrefs = await page.locator('main a[href]').evaluateAll((as) => as.map((a) => a.getAttribute('href') ?? ''))
      const target = hrefs.find((h) => rowLink.test(h))
      if (!target) continue // 没有数据的列表跳过
      await page.goto(`${st.adminUrl}${target}`)
      await expect(page.getByRole('heading', { level: 1 }).first()).toBeVisible()
      await page.waitForLoadState('networkidle')
      expect(problems, `${target} 页面出现问题`).toEqual([])
    }
  })
})
