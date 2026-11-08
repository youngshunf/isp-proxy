// 跨用例复用的界面操作：前台注册验证、下单，后台登录与确认收款。全部走真实界面与真实控制面。
import { expect, type Browser, type Page } from '@playwright/test'
import { mailCode, newEmail, nextTotp, stack } from './stack'

export const PASSWORD = 'e2e-password-123'

/** 注册 → 读邮件验证码 → 验证即登录，停在“我的 IP”。 */
export async function registerAndVerify(page: Page, email: string, password = PASSWORD) {
  const { portalUrl } = stack()
  const sent = Date.now() - 1000
  await page.goto(`${portalUrl}/register`)
  await page.getByLabel('邮箱').fill(email)
  await page.getByLabel('登录密码').fill(password)
  await page.getByRole('button', { name: '注册并发送验证码' }).click()
  await expect(page).toHaveURL(/\/verify-email/)
  await expect(page.getByLabel('注册邮箱')).toHaveValue(email)
  const code = await mailCode(email, '邮箱验证码', sent)
  await page.getByLabel('验证码').fill(code)
  await page.getByRole('button', { name: '验证并登录' }).click()
  await expect(page).toHaveURL(/\/ips$/)
}

/** 在购买页选地区下单，返回订单 id（停在订单详情，显示付款方式）。 */
export async function buyNew(page: Page, region = '美国 洛杉矶'): Promise<string> {
  const { portalUrl } = stack()
  await page.goto(`${portalUrl}/buy`)
  // exact：独享“美国 洛杉矶”与共享“美国 洛杉矶 共享”是两行（D-24）
  await page.getByRole('radio', { name: region, exact: true }).check()
  await page.getByRole('button', { name: /提交订单/ }).click()
  await expect(page).toHaveURL(/\/orders\/[^/]+$/)
  await expect(page.getByText('付款方式').first()).toBeVisible()
  return page.url().split('/').pop()!
}

/** 管理员登录（密码 + TOTP），停在 from 或总览。 */
export async function adminLogin(page: Page, from = '/') {
  const { adminUrl, admin } = stack()
  await page.goto(`${adminUrl}/login?from=${encodeURIComponent(from)}`)
  await page.getByLabel('邮箱').fill(admin.email)
  await page.getByLabel('密码').fill(admin.password)
  await page.getByRole('button', { name: '下一步' }).click()
  await expect(page.getByRole('heading', { name: '输入验证码' })).toBeVisible()
  await page.getByLabel('验证码').fill(await nextTotp())
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).not.toHaveURL(/\/login/)
}

/** 新开一个已登录的后台页（与前台不同的浏览器上下文，互不共享 cookie）。 */
export async function adminPage(browser: Browser, from = '/'): Promise<Page> {
  const ctx = await browser.newContext()
  const page = await ctx.newPage()
  await adminLogin(page, from)
  return page
}

/** 后台确认收款（实际到账金额默认等于应付）。 */
export async function confirmPayment(admin: Page, orderId: string) {
  const { adminUrl } = stack()
  await admin.goto(`${adminUrl}/orders/${orderId}`)
  await admin.getByRole('button', { name: '确认收款' }).click()
  const dlg = admin.getByRole('dialog')
  await dlg.getByLabel('外部收款参考').fill(`E2E-${orderId.slice(-6)}`)
  await dlg.getByRole('button', { name: /确认收到/ }).click()
  await expect(dlg).toBeHidden()
}

/** 前台订单页（待核款时按间隔轮询）等到开通完成，进入 IP 详情，返回订阅 id。 */
export async function openProvisionedIp(page: Page): Promise<string> {
  // 用户切回前台的标签页（后台在另一个浏览器上下文里操作过）
  await page.bringToFront()
  const link = page.getByRole('link', { name: '查看这个 IP' })
  await expect(link).toBeVisible({ timeout: 45_000 })
  await link.click()
  await expect(page).toHaveURL(/\/ips\/[^/]+$/)
  await expect(page.getByLabel('套餐余量').getByText('可用', { exact: true })).toBeVisible({ timeout: 45_000 })
  return page.url().split('/').pop()!
}

/** 新用户 + 一个已开通的 IP：注册验证、下单、后台确认收款，停在 IP 详情。 */
export async function provisionedUser(page: Page, admin: Page, tag: string, region = '美国 洛杉矶') {
  const email = newEmail(tag)
  await registerAndVerify(page, email)
  const orderId = await buyNew(page, region)
  await confirmPayment(admin, orderId)
  const subId = await openProvisionedIp(page)
  return { email, subId }
}
