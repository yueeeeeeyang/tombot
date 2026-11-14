import { expect, test } from '@playwright/test'
import { randomUUID } from 'node:crypto'

test('刷新、后台唤醒、多标签页与离线恢复保持会话，品牌图标使用猫咪', async ({
  page,
  context,
}, testInfo) => {
  let now = Date.now()
  const initial = now
  let accessExpiry = now + 600_000
  let refreshValid = true
  let offline = false
  let rotations = 0
  let csrf = randomUUID()
  const principal = {
    username: 'test-admin',
    display_name: '测试管理员',
    user_id: '0'.repeat(25) + '1',
    session_id: '0'.repeat(25) + '2',
    device_id: '0'.repeat(25) + '3',
    role: 'admin',
    scope: 'admin',
  }
  const problems: string[] = []
  context.on('page', (opened) => opened.on('pageerror', (error) => problems.push(error.message)))
  page.on('pageerror', (error) => problems.push(error.message))
  await context.route('**/api/v2/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace('/api/v2/admin', '')
    if (offline) {
      await route.abort('internetdisconnected')
      return
    }
    const session = () => ({
      principal,
      csrf_token: csrf,
      access_expires_at: new Date(accessExpiry).toISOString(),
      expires_at: new Date(initial + 86_400_000).toISOString(),
    })
    if (path === '/auth/session' || path === '/auth/refresh-context') {
      const valid = refreshValid && (path.endsWith('/refresh-context') || accessExpiry > now)
      await route.fulfill(
        valid
          ? { status: 200, json: session() }
          : { status: 401, json: { code: 'SESSION_INVALID', message: '会话已失效' } },
      )
    } else if (path === '/auth/refresh') {
      expect(refreshValid).toBe(true)
      expect(route.request().headers()['x-csrf-token'] === csrf).toBe(true)
      rotations++
      csrf = randomUUID()
      accessExpiry = now + 600_000
      await route.fulfill({ status: 200, json: session() })
    } else if (path === '/auth/logout') {
      expect(route.request().headers()['x-csrf-token'] === csrf).toBe(true)
      refreshValid = false
      await route.fulfill({ status: 204 })
    } else if (path === '/api/v2/auth/admin-setup') {
      await route.fulfill({ status: 200, json: { required: false } })
    } else if (path === '/users') {
      await route.fulfill({ status: 200, json: { users: [] } })
    } else
      await route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '测试接口未配置' } })
  })
  await page.clock.install({ time: now })
  await page.goto('/admin/')
  await expect(page.getByText('暂无用户')).toBeVisible()
  await page.reload()
  await expect(page.getByText('暂无用户')).toBeVisible()
  now += 540_000
  await page.clock.fastForward(540_000)
  await expect.poll(() => rotations).toBe(1)

  const other = await context.newPage()
  await other.clock.install({ time: now })
  await other.goto('/admin/')
  await expect(other.getByText('暂无用户')).toBeVisible()
  now += 700_000
  await Promise.all([page.clock.setSystemTime(now), other.clock.setSystemTime(now)])
  await Promise.all(
    [page, other].map((opened) =>
      opened.evaluate(() => window.dispatchEvent(new Event('pageshow'))),
    ),
  )
  await expect.poll(() => rotations).toBe(2)
  await expect(page.getByText('暂无用户')).toBeVisible()
  await expect(other.getByText('暂无用户')).toBeVisible()

  offline = true
  now += 700_000
  await page.clock.setSystemTime(now)
  await page.evaluate(() => window.dispatchEvent(new Event('pageshow')))
  await expect(page.getByRole('alert')).toContainText('会话续期暂未完成')
  await expect(page.getByText('暂无用户')).toBeVisible()
  offline = false
  await page.evaluate(() => window.dispatchEvent(new Event('online')))
  await expect.poll(() => rotations).toBe(3)
  await expect(page.getByRole('alert')).toHaveCount(0)
  const avatar = page.getByRole('img', { name: 'TomBot 猫咪头像' })
  await expect(avatar).toBeVisible()
  expect(await avatar.evaluate((element: HTMLImageElement) => element.naturalWidth)).toBe(256)
  await page.screenshot({ path: testInfo.outputPath('admin-cat-navigation.png'), fullPage: true })
  await page.getByRole('button', { name: /^账户菜单/ }).click()
  await expect(page.getByText('测试管理员', { exact: true })).toBeVisible()
  await expect(page.getByText('@test-admin', { exact: true })).toBeVisible()
  await expect(page.getByRole('menuitemradio', { name: '跟随系统' })).toBeFocused()
  await page.screenshot({ path: testInfo.outputPath('account-menu-desktop.png'), fullPage: true })
  await page.keyboard.press('Escape')
  await expect(page.getByRole('button', { name: /^账户菜单/ })).toBeFocused()
  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('button', { name: '切换导航' }).click()
  await page.getByRole('button', { name: /^账户菜单/ }).click()
  await page.screenshot({ path: testInfo.outputPath('account-menu-mobile.png'), fullPage: true })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.getByRole('menuitem', { name: '退出登录', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'TomBot 管理平台' })).toBeVisible()
  await expect(avatar).toBeVisible()
  const icon = await page.locator('link[rel="icon"]').getAttribute('href')
  expect(icon).toContain('bombay-')
  const imageResponse = await context.request.get(icon!)
  expect(imageResponse.headers()['content-type']).toContain('image/png')
  await page.screenshot({ path: testInfo.outputPath('admin-cat-login.png'), fullPage: true })
  expect(problems).toEqual([])
  expect(await page.evaluate(() => Object.keys(localStorage))).toEqual([])
  expect(await page.evaluate(() => Object.keys(sessionStorage))).toEqual([])
})
