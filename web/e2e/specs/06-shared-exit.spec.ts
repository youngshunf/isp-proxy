// 共享出口套餐（D-24）：前台选“美国 洛杉矶 共享”下单 → 后台确认收款 → 分到一个可共享的出口（绑定上限 ≥2），
// 前台显示“共享 · 最多 N 人共用”，经接入到达该出口；后台上游库存显示占用 1/N 并列出受影响订阅。
import { expect, test } from '@playwright/test'
import { adminPage, buyNew, confirmPayment, openProvisionedIp, registerAndVerify } from '../support/flows'
import { expectExit, newEmail, stack } from '../support/stack'

test('共享出口：购买、开通与后台占用', async ({ page, browser }) => {
  const st = stack()
  await registerAndVerify(page, newEmail('shared'))

  const orderId = await test.step('购买页选共享套餐下单', async () => {
    await page.goto(`${st.portalUrl}/buy`)
    const row = page.getByRole('row').filter({ has: page.getByRole('radio', { name: '美国 洛杉矶 共享', exact: true }) })
    await expect(row.getByText('共享', { exact: true })).toBeVisible()
    return buyNew(page, '美国 洛杉矶 共享')
  })

  const admin = await adminPage(browser)
  await confirmPayment(admin, orderId)
  const subId = await openProvisionedIp(page)

  const exit = await test.step('详情显示共享与上限，经接入到达一个可共享的出口', async () => {
    await expect(page.getByText(/共享 · 最多 \d+ 人共用/)).toBeVisible()
    const ip = (await page.getByText(/^203\.0\.113\.\d+$/).first().textContent())!.trim()
    const x = st.exits.find((e) => e.ip === ip)
    expect(x, `出口 ${ip} 应是已录入的出口`).toBeTruthy()
    expect(x!.maxTenants, '共享订阅只分到绑定上限 ≥2 的出口').toBeGreaterThanOrEqual(2)
    const row = page.locator('.conn-row').filter({ hasText: 'HTTP' }).first()
    await row.getByRole('button', { name: /复制/ }).click()
    await expect(row.getByRole('button', { name: '已复制' })).toBeVisible()
    await expectExit(await page.evaluate(() => navigator.clipboard.readText()), x!.label)
    return x!
  })

  await test.step('后台上游库存：占用 x/N，受影响订阅里有这个订阅', async () => {
    await admin.goto(`${st.adminUrl}/upstreams?q=${encodeURIComponent(exit.resourceId!)}`)
    const row = admin.getByRole('row').filter({ hasText: exit.resourceId! })
    await expect(row).toContainText(new RegExp(`共享 \\d+/${exit.maxTenants}`))
    await row.getByRole('button', { name: '受影响订阅' }).click()
    const dlg = admin.getByRole('dialog')
    await expect(dlg.locator(`a[href="/subscriptions/${subId}"]`)).toBeVisible()
  })
})
