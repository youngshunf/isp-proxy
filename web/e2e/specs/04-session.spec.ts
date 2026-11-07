// 会话失效：在另一个标签页退出后，原标签页的下一次请求收到 401，带着原路径去登录，登录后回到原页面；
// 未登录直接打开受保护页面同样去登录。
import { expect, test } from '@playwright/test'
import { stack } from '../support/stack'

test('会话失效后带原路径去登录，登录后回到原页面', async ({ page, context }) => {
  const st = stack()
  await test.step('未登录打开受保护页面 → 登录页（带 next）', async () => {
    await page.goto(`${st.portalUrl}/orders`)
    await expect(page).toHaveURL(/\/login\?next=%2Forders/)
  })

  await test.step('登录后回到原页面', async () => {
    await page.getByLabel('邮箱').fill(st.user.email)
    await page.getByLabel('密码').fill(st.user.password)
    await page.getByRole('button', { name: '登录', exact: true }).click()
    await expect(page).toHaveURL(/\/orders$/)
    await expect(page.getByRole('heading', { name: '订单', exact: true })).toBeVisible()
  })

  await test.step('在另一个标签页退出', async () => {
    const other = await context.newPage()
    await other.goto(`${st.portalUrl}/ips`)
    await other.getByRole('button', { name: st.user.email }).click()
    await other.getByRole('menuitem', { name: /退出登录/ }).click()
    await expect(other).toHaveURL(/\/login/)
    await other.close()
  })

  await test.step('原标签页下一次请求 401 → 登录页提示会话已失效', async () => {
    // 应用内跳转（不整页刷新）：进入申请页即请求列表，服务端返回 401
    await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '申请' }).click()
    await expect(page).toHaveURL(/\/login\?next=%2Frequests&expired=1/)
    await expect(page.getByText('登录已失效，请重新登录')).toBeVisible()
  })

  await test.step('重新登录回到申请页', async () => {
    await page.getByLabel('邮箱').fill(st.user.email)
    await page.getByLabel('密码').fill(st.user.password)
    await page.getByRole('button', { name: '登录', exact: true }).click()
    await expect(page).toHaveURL(/\/requests$/)
  })
})

test('后台会话失效同样带原路径去登录', async ({ page, context }) => {
  const st = stack()
  await page.goto(`${st.adminUrl}/orders`)
  await expect(page).toHaveURL(/\/login\?from=%2Forders/)
  // 没有登录过：只验证跳转与提示，不消耗 TOTP
  await context.clearCookies()
  await page.goto(`${st.adminUrl}/users`)
  await expect(page).toHaveURL(/\/login\?from=%2Fusers/)
  await expect(page.getByRole('heading', { name: '登录管理后台' })).toBeVisible()
})
