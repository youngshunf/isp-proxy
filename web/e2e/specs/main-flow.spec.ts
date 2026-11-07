// 主流程：注册 → 邮件验证码 → 下单（待付款）→ 管理员 TOTP 登录确认收款 → 前台看到 IP 已开通 →
// 复制连接串并经它发 HTTP 请求看到假出口标签 → 换接入（旧凭据被拒、新凭据可用）→
// 申请换出口 → 管理员处理 → 前台看到新出口。
import { expect, test } from '@playwright/test'
import { adminPage, buyNew, confirmPayment, registerAndVerify } from '../support/flows'
import { expectExit, expectRejected, newEmail, stack } from '../support/stack'

test('主流程：从注册到换出口', async ({ page, browser }) => {
  const st = stack()
  const email = newEmail('main')
  const exitByIp = new Map(st.exits.map((e) => [e.ip, e]))

  await test.step('注册并用邮件验证码验证（验证即登录）', async () => {
    await registerAndVerify(page, email)
    await expect(page.getByRole('heading', { name: '我的 IP' })).toBeVisible()
  })

  const orderId = await test.step('下单：订单停在待付款，显示收款说明', async () => {
    const id = await buyNew(page)
    await expect(page.getByText('待核款').first()).toBeVisible()
    return id
  })

  const admin = await adminPage(browser)
  await test.step('管理员确认收款', async () => {
    await confirmPayment(admin, orderId)
  })

  const subId = await test.step('前台订单页轮询到开通完成，IP 显示为可用', async () => {
    // 不刷新：订单页在待核款/开通中按间隔轮询
    // 用户切回前台的标签页（后台页在另一个浏览器上下文里打开过）
    await page.bringToFront()
    const link = page.getByRole('link', { name: '查看这个 IP' })
    await expect(link).toBeVisible({ timeout: 45_000 })
    await link.click()
    await expect(page).toHaveURL(/\/ips\/[^/]+$/)
    await expect(page.getByText('可用').first()).toBeVisible({ timeout: 45_000 })
    return page.url().split('/').pop()!
  })

  // 复制按钮写系统剪贴板，需要页面有焦点；再从剪贴板读回，确认复制的就是可用的连接串
  const copyConn = async (proto: 'HTTP' | 'SOCKS5') => {
    await page.bringToFront()
    const row = page.locator('.conn-row').filter({ hasText: proto }).first()
    await row.getByRole('button', { name: /复制/ }).click()
    await expect(row.getByRole('button', { name: '已复制' })).toBeVisible()
    return page.evaluate(() => navigator.clipboard.readText())
  }

  const firstExit = await test.step('复制 HTTP 连接串，经它请求看到出口标签', async () => {
    const conn = await copyConn('HTTP')
    expect(conn).toMatch(/^http:\/\/[^:]+:[^@]+@127\.0\.0\.1:\d+$/)
    const ip = await page.getByText(/^203\.0\.113\.\d+$/).first().textContent()
    const exit = exitByIp.get(ip!.trim())
    expect(exit, `页面上的出口 IP ${ip} 应是已录入的出口`).toBeTruthy()
    await expectExit(conn, exit!.label)
    // SOCKS5 同一出口
    await expectExit(await copyConn('SOCKS5'), exit!.label)
    return { conn, exit: exit! }
  })

  await test.step('换接入：旧凭据被拒，新凭据可用', async () => {
    await page.getByRole('button', { name: '更换接入' }).click()
    const dlg = page.getByRole('dialog')
    await expect(dlg.getByText('旧连接会立即失效，正在使用的程序需要更新配置。')).toBeVisible()
    await dlg.getByRole('button', { name: '确认更换端口与密码' }).click()
    await expect(page.getByText(/接入已更新到 v2：/)).toBeVisible()
    const fresh = await copyConn('HTTP')
    expect(fresh).not.toBe(firstExit.conn)
    await expectRejected(firstExit.conn)
    await expectExit(fresh, firstExit.exit.label)
    await page.getByRole('button', { name: '我已更新程序' }).click()
    await expect(page.getByText(/接入已更新到 v2：/)).toBeHidden()
  })

  const reqNo = await test.step('申请换出口', async () => {
    await page.getByRole('button', { name: '申请换出口' }).click()
    const dlg = page.getByRole('dialog')
    await dlg.getByLabel('速度或稳定性不佳').check()
    await dlg.getByRole('button', { name: '提交申请' }).click()
    await expect(dlg).toBeHidden()
    const no = page.locator('.callout .mono').first()
    await expect(no).toHaveText(/^REQ-/)
    return (await no.textContent())!.trim()
  })

  const spare = st.spares[0]!
  await test.step('管理员受理申请，在向导里当场录入候选并换出口', async () => {
    await admin.goto(`${st.adminUrl}/exit-requests`)
    const row = admin.getByRole('row').filter({ hasText: reqNo })
    await row.getByRole('button', { name: '受理' }).click()
    const accept = admin.getByRole('dialog')
    await accept.getByRole('button', { name: '受理' }).click()
    await expect(accept).toBeHidden()
    await row.getByRole('button', { name: '执行换出口' }).click()
    await expect(admin).toHaveURL(new RegExp(`/subscriptions/${subId}/exit-change`))
    // 库存外的新上游：当场录入（节点与地区按订阅预填），录入后自动选中
    await admin.getByRole('button', { name: '录入新候选' }).click()
    const add = admin.getByRole('dialog')
    await add.getByLabel('上游地址').fill(spare.url)
    await add.getByLabel('供应商').fill('e2e')
    await add.getByRole('button', { name: '录入' }).click()
    await expect(add).toBeHidden()
    const port = new URL(spare.url).port
    await expect(admin.getByRole('radio', { name: new RegExp(`:${port}$`) })).toBeChecked()
    await admin.getByRole('button', { name: '开始验证' }).click()
    await expect(admin.getByText('三项都通过', { exact: false })).toBeVisible({ timeout: 45_000 })
    await admin.getByRole('button', { name: '下一步：提交' }).click()
    await admin.getByLabel('换出口原因').fill('用户反馈速度不佳，换同城出口')
    await admin.getByRole('button', { name: '提交换出口' }).click()
    await expect(admin.getByRole('status').filter({ hasText: '换出口' })).toContainText('已完成', { timeout: 60_000 })
    await admin.goto(`${st.adminUrl}/exit-requests`)
    await expect(admin.getByRole('row').filter({ hasText: reqNo })).toContainText(`已更换${firstExit.exit.ip} → ${spare.ip}`)
  })

  await test.step('前台看到新出口，经同一接入到达新出口', async () => {
    await page.bringToFront()
    await page.reload()
    await expect(page.getByText(firstExit.exit.ip, { exact: true })).toHaveCount(0, { timeout: 30_000 })
    const ip = (await page.getByText(/^203\.0\.113\.\d+$/).first().textContent())!.trim()
    expect(ip).toBe(spare.ip)
    await expectExit(await copyConn('HTTP'), spare.label)
    await page.goto(`${st.portalUrl}/requests`)
    const req = page.getByRole('listitem').filter({ hasText: reqNo })
    await expect(req).toContainText('已更换')
    await expect(req).toContainText(spare.ip)
  })
})
