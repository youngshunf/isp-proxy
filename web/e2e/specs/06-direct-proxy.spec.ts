// 服务器直出代理（D-25，A-50）：用户有一个付费 IP → 管理员在用户详情里勾选赠送并设带宽 →
// 前台“我的 IP”出现独立的直出卡片（节点未填出口地域时不显示国旗、写明跟随付费 IP 与不限流量、没有续购入口）→
// 管理员在节点上填直出出口地域 → 卡片显示该地域与国旗，不显示出口 IP →
// 从卡片复制连接串，经它请求到达的是节点本机直连（开发栈的回显服务返回 DIRECT），不是任何假 ISP 出口 →
// 管理员收回后连接被拒、卡片消失。
import { expect, test } from '@playwright/test'
import { adminPage, provisionedUser } from '../support/flows'
import { expectExit, expectRejected, stack } from '../support/stack'

test('管理员赠送服务器直出代理，前台看到直出卡片并能复制可用的连接', async ({ page, browser }) => {
  const st = stack()
  const problems: string[] = []
  page.on('pageerror', (e) => problems.push(e.message))
  page.on('response', (r) => {
    const path = new URL(r.url()).pathname
    if (path.startsWith('/api/') && r.status() >= 400 && !(r.status() === 401 && path === '/api/me')) problems.push(`${r.status()} ${r.request().method()} ${path}`)
  })

  const admin = await adminPage(browser)
  const { email } = await test.step('新用户买一个付费 IP（直出跟随它有效）', () => provisionedUser(page, admin, 'direct'))

  const userUrl = await test.step('后台：用户详情里勾选赠送，带宽 20 Mbps', async () => {
    await admin.bringToFront()
    await admin.goto(`${st.adminUrl}/users?q=${encodeURIComponent(email)}`)
    await admin.getByRole('link', { name: email }).click()
    await expect(admin).toHaveURL(/\/users\/[^/]+$/)
    const box = admin.getByRole('checkbox', { name: '赠送服务器直出代理' })
    await expect(box).not.toBeChecked()
    await box.check()
    const bw = admin.getByLabel('带宽上限（Mbps）')
    await bw.fill('20')
    await admin.getByRole('button', { name: '赠送', exact: true }).click()
    await expect(admin.getByText('已赠送服务器直出代理')).toBeVisible()
    // 订阅表格里能区分直出订阅
    await expect(admin.getByText(/64 并发 · 20 Mbps · 不限流量/)).toBeVisible()
    return admin.url()
  })

  // 直出卡片：只有它写着“不限流量”（ISP 卡片都有流量额度）
  const card = page.getByRole('article').filter({ hasText: '不限流量' })
  await test.step('前台：我的 IP 出现直出卡片（无国旗、跟随付费 IP、不限流量）', async () => {
    await page.bringToFront()
    await page.goto(`${st.portalUrl}/ips`)
    await expect(card).toBeVisible()
    await expect(card.getByRole('img', { name: '服务器直出' })).toBeVisible()
    await expect(card.locator('.flag img')).toHaveCount(0)
    await expect(card.getByText(/20 Mbps/)).toBeVisible()
    await expect(card.getByText(/跟随付费 IP，至/)).toBeVisible()
    await expect(card.getByText(/不限流量/)).toBeVisible()
    await expect(card.getByText('可用', { exact: true })).toBeVisible({ timeout: 45_000 })
    for (const name of ['续时长', '买流量']) await expect(card.getByRole('button', { name })).toHaveCount(0)
  })

  await test.step('后台在节点上填直出出口地域，前台卡片显示地域与国旗、不显示 IP', async () => {
    await admin.bringToFront()
    await admin.goto(`${st.adminUrl}/monitoring`)
    // 开发栈承载订阅的节点叫 devstack（另有一个已停用的种子节点）
    await admin.getByRole('button', { name: '修改节点 devstack' }).click()
    const dlg = admin.getByRole('dialog')
    await dlg.getByLabel('直出出口地域').fill('新加坡')
    await dlg.getByLabel('地域代码').fill('sg')
    await dlg.getByRole('button', { name: /保\s*存/ }).click()
    await expect(dlg).toBeHidden()
    await expect(admin.getByText('新加坡（SG）')).toBeVisible()
    await page.bringToFront()
    await page.reload()
    await expect(card.getByRole('img', { name: '新加坡' })).toBeVisible()
    await expect(card.locator('.sim-exit')).toContainText('新加坡')
    await expect(card.locator('.sim-exit')).toContainText('服务器直出')
    await expect(card.getByText(/\b\d{1,3}(\.\d{1,3}){3}\b/)).toHaveCount(0)
  })

  const conn = await test.step('从直出卡片复制 HTTP 连接，经它请求到达节点本机直连', async () => {
    await card.getByRole('button', { name: '复制 HTTP 连接' }).click()
    await expect(card.getByRole('button', { name: '已复制 HTTP 连接' })).toBeVisible()
    const c = await page.evaluate(() => navigator.clipboard.readText())
    expect(c).toMatch(/^http:\/\/[^:]+:[^@]+@127\.0\.0\.1:\d+$/)
    await expectExit(c, 'DIRECT')
    // SOCKS5 同样直连
    await card.getByRole('button', { name: '复制 SOCKS5 连接' }).click()
    await expect(card.getByRole('button', { name: '已复制 SOCKS5 连接' })).toBeVisible()
    await expectExit(await page.evaluate(() => navigator.clipboard.readText()), 'DIRECT')
    return c
  })

  await test.step('直出详情：可复制、可换接入，没有续购与换出口', async () => {
    await card.getByRole('link', { name: /详情与换接入/ }).click()
    await expect(page).toHaveURL(/\/ips\/[^/]+$/)
    await expect(page.getByText('直出说明')).toBeVisible()
    await expect(page.getByRole('button', { name: '更换接入' })).toBeVisible()
    for (const name of ['续时长', '买流量', '申请换出口']) await expect(page.getByRole('button', { name })).toHaveCount(0)
  })

  await test.step('后台收回：确认框原因必填，收回后旧连接被拒、卡片消失', async () => {
    await admin.bringToFront()
    await admin.goto(userUrl)
    // 取消勾选先弹确认框（勾选状态在确认前不变），所以用 click 而不是 uncheck
    await admin.getByRole('checkbox', { name: '赠送服务器直出代理' }).click()
    const dlg = admin.getByRole('dialog')
    await dlg.getByLabel('收回原因').fill('E2E 收回')
    await dlg.getByRole('button', { name: '收回' }).click()
    await expect(dlg).toBeHidden()
    await expect(admin.getByText('已收回服务器直出代理')).toBeVisible()
    await expectRejected(conn)
    await page.bringToFront()
    await page.goto(`${st.portalUrl}/ips`)
    await expect(page.getByRole('heading', { name: '我的 IP' })).toBeVisible()
    await expect(card).toHaveCount(0)
  })

  expect(problems).toEqual([])
})
