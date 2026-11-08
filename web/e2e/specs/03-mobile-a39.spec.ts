// A-39：手机视口（390×844）上完成注册、选购、复制连接、换接入、续购；主要表单只用键盘操作
// （Tab 移动焦点、输入、空格选择、回车提交），每一页都没有横向滚动。
import { expect, test, type Locator, type Page } from '@playwright/test'
import { adminPage, confirmPayment, openProvisionedIp } from '../support/flows'
import { expectExit, mailCode, newEmail, stack } from '../support/stack'

test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 3 })

/** 只按 Tab 把焦点移到目标上（最多 max 次）；到不了说明键盘不可达。 */
async function tabTo(page: Page, target: Locator, max = 40) {
  for (let i = 0; i < max; i++) {
    if (await target.evaluate((el) => el === document.activeElement || el.contains(document.activeElement))) return
    await page.keyboard.press('Tab')
  }
  throw new Error(`按 ${max} 次 Tab 也没有到达 ${target}`)
}

async function noHorizontalScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth])
  expect(sw, `${page.url()} 出现横向滚动`).toBeLessThanOrEqual(cw)
}

test('手机上注册、选购、复制连接、换接入、续购（键盘操作）', async ({ page, browser }) => {
  const st = stack()
  const email = newEmail('mobile')

  await test.step('注册（键盘）', async () => {
    const sent = Date.now() - 1000
    await page.goto(`${st.portalUrl}/register`)
    await noHorizontalScroll(page)
    await tabTo(page, page.getByLabel('邮箱'))
    await page.keyboard.type(email)
    await tabTo(page, page.getByLabel('登录密码'))
    await page.keyboard.type('e2e-password-123')
    await page.keyboard.press('Enter')
    await expect(page).toHaveURL(/\/verify-email/)
    await noHorizontalScroll(page)
    const code = await mailCode(email, '邮箱验证码', sent)
    await tabTo(page, page.getByLabel('验证码'))
    await page.keyboard.type(code)
    await page.keyboard.press('Enter')
    await expect(page).toHaveURL(/\/ips$/)
    await noHorizontalScroll(page)
  })

  const orderId = await test.step('选购（键盘）', async () => {
    await page.goto(`${st.portalUrl}/buy`)
    await expect(page.getByRole('heading', { name: '购买 IP' })).toBeVisible()
    await noHorizontalScroll(page)
    await tabTo(page, page.getByRole('radio', { name: '美国 洛杉矶', exact: true }))
    await page.keyboard.press('Space')
    await expect(page.getByRole('radio', { name: '美国 洛杉矶', exact: true })).toBeChecked()
    await tabTo(page, page.getByRole('button', { name: /提交订单/ }))
    await page.keyboard.press('Enter')
    await expect(page).toHaveURL(/\/orders\/[^/]+$/)
    await expect(page.getByText('付款方式').first()).toBeVisible()
    await noHorizontalScroll(page)
    return page.url().split('/').pop()!
  })

  const admin = await adminPage(browser)
  await confirmPayment(admin, orderId)
  const subId = await openProvisionedIp(page)
  await noHorizontalScroll(page)

  const copyHttp = async () => {
    const row = page.locator('.conn-row').filter({ hasText: 'HTTP' }).first()
    await tabTo(page, row.getByRole('button', { name: /复制/ }), 60)
    await page.keyboard.press('Enter')
    await expect(row.getByRole('button', { name: '已复制' })).toBeVisible()
    return page.evaluate(() => navigator.clipboard.readText())
  }

  const firstConn = await test.step('复制连接（键盘）并经它访问', async () => {
    const conn = await copyHttp()
    expect(conn).toMatch(/^http:\/\//)
    const ip = (await page.getByText(/^203\.0\.113\.\d+$/).first().textContent())!.trim()
    await expectExit(conn, st.exits.find((e) => e.ip === ip)!.label)
    return conn
  })

  await test.step('换接入（键盘）', async () => {
    await tabTo(page, page.getByRole('button', { name: '重置密码' }), 80)
    await page.keyboard.press('Enter')
    const dlg = page.getByRole('dialog')
    await expect(dlg.getByText('旧连接会立即失效，正在使用的程序需要更新配置。')).toBeVisible()
    await noHorizontalScroll(page)
    await tabTo(page, dlg.getByRole('button', { name: '确认重置密码' }))
    await page.keyboard.press('Enter')
    await expect(page.getByText(/接入已更新到 v2：/)).toBeVisible()
    const fresh = await copyHttp()
    expect(fresh).not.toBe(firstConn)
    expect(new URL(fresh).port).toBe(new URL(firstConn).port) // 只重置密码，端口不变
    await tabTo(page, page.getByRole('button', { name: '我已更新程序' }), 80)
    await page.keyboard.press('Enter')
    await expect(page.getByText(/接入已更新到 v2：/)).toBeHidden()
  })

  await test.step('续购（键盘）', async () => {
    await page.goto(`${st.portalUrl}/ips/${subId}`)
    await tabTo(page, page.getByLabel('套餐余量').getByRole('button', { name: '续时长' }), 80)
    await page.keyboard.press('Enter')
    await expect(page).toHaveURL(new RegExp(`/buy/renew/${subId}\\?kind=time`))
    await noHorizontalScroll(page)
    await tabTo(page, page.getByRole('button', { name: /提交订单/ }), 60)
    await page.keyboard.press('Enter')
    await expect(page).toHaveURL(/\/orders\/[^/]+$/)
    await expect(page.getByText('付款方式').first()).toBeVisible()
    await noHorizontalScroll(page)
  })
})
