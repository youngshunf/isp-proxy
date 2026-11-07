// 续时长与买流量：各自下单 → 后台确认收款 → 前台余量入账（续时长只加时长，买流量只加流量）。
import { expect, test, type Page } from '@playwright/test'
import { adminPage, confirmPayment, provisionedUser } from '../support/flows'
import { stack } from '../support/stack'

const meter = (page: Page, label: string) => page.getByLabel('套餐余量').locator('.meter').filter({ hasText: label })

test('续时长与买流量分别入账', async ({ page, browser }) => {
  const st = stack()
  const admin = await adminPage(browser)
  const { subId } = await provisionedUser(page, admin, 'renew')

  await expect(meter(page, '剩余时长')).toContainText('/ 30 天')
  const trafficBefore = (await meter(page, '剩余流量').textContent())!

  await test.step('续时长：+30 天，流量不变', async () => {
    await page.getByRole('button', { name: '续时长' }).first().click()
    await expect(page).toHaveURL(new RegExp(`/buy/renew/${subId}\\?kind=time`))
    await expect(page.getByRole('heading', { name: '续时长' })).toBeVisible()
    await page.getByRole('button', { name: /\+30 天/ }).click()
    await page.getByRole('button', { name: /提交订单/ }).click()
    await expect(page).toHaveURL(/\/orders\/[^/]+$/)
    await expect(page.getByText('续时长').first()).toBeVisible()
    const orderId = page.url().split('/').pop()!
    await confirmPayment(admin, orderId)
    await page.bringToFront()
    await expect(page.getByText('已完成').first()).toBeVisible({ timeout: 30_000 })
    await page.goto(`${st.portalUrl}/ips/${subId}`)
    await expect(meter(page, '剩余时长')).toContainText('/ 60 天')
    expect(await meter(page, '剩余流量').textContent()).toBe(trafficBefore)
  })

  await test.step('买流量：总量增加，到期时间不变', async () => {
    const expiry = (await meter(page, '剩余时长').locator('.plate-note').textContent())!
    await page.getByRole('button', { name: '买流量' }).first().click()
    await expect(page.getByRole('heading', { name: '买流量' })).toBeVisible()
    const pack = page.locator('.choice').first()
    const packText = (await pack.locator('b').textContent())!.trim() // 例如 “+10 GB”
    await pack.click()
    await expect(pack).toHaveAttribute('aria-pressed', 'true')
    await page.getByRole('button', { name: /提交订单/ }).click()
    await expect(page).toHaveURL(/\/orders\/[^/]+$/)
    const orderId = page.url().split('/').pop()!
    await confirmPayment(admin, orderId)
    await page.bringToFront()
    await expect(page.getByText('已完成').first()).toBeVisible({ timeout: 30_000 })
    await page.goto(`${st.portalUrl}/ips/${subId}`)
    const total = (s: string) => Number(s.match(/\/\s*([\d.]+)\s*GB/)?.[1])
    const added = Number(packText.match(/([\d.]+)\s*GB/)?.[1])
    await expect.poll(async () => total((await meter(page, '剩余流量').textContent())!)).toBe(total(trafficBefore) + added)
    expect(await meter(page, '剩余时长').locator('.plate-note').textContent()).toBe(expiry)
  })

  await test.step('订单列表里两笔都已完成', async () => {
    await page.goto(`${st.portalUrl}/orders`)
    await expect(page.getByText('续时长').first()).toBeVisible()
    await expect(page.getByText('买流量').first()).toBeVisible()
  })
})
