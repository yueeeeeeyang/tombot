import { chooseOption } from './select'
import { expect, test } from '@playwright/test'
import { randomBytes, randomInt, randomUUID } from 'node:crypto'

// 浏览器验证使用临时随机材料；真实首建、动态码和 Cookie 协议另由 PostgreSQL＋HTTPS 联合测试覆盖。
test('首位管理员创建的主题、窄屏、键盘与返回登录流程', async ({ page }, testInfo) => {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  const secret = Array.from(randomBytes(32), (value) => alphabet[value % 32]).join('')
  const password = randomUUID()
  const activationCode = randomInt(0, 1_000_000).toString().padStart(6, '0')
  const loginCode = ((Number(activationCode) + 1) % 1_000_000).toString().padStart(6, '0')
  const problems: string[] = []
  page.on('pageerror', (error) => problems.push(error.message))
  await page.addInitScript(() => {
    const violations: string[] = []
    Reflect.set(window, '__tombotTestCspViolations', violations)
    document.addEventListener('securitypolicyviolation', (event) =>
      violations.push(event.violatedDirective),
    )
  })
  let creationCalls = 0
  let authenticated = false
  const session = {
    principal: {
      username: 'test-admin',
      display_name: '测试管理员',
      user_id: '0'.repeat(25) + '1',
      session_id: '0'.repeat(25) + '2',
      device_id: '0'.repeat(25) + '3',
      role: 'admin',
      scope: 'admin',
    },
    csrf_token: randomUUID(),
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
    expires_at: new Date(Date.now() + 3_600_000).toISOString(),
  }
  await page.route('**/api/v2/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    expect(request.url().includes(secret)).toBe(false)
    if (path === '/api/v2/admin/auth/session' || path === '/api/v2/admin/auth/refresh-context') {
      await route.fulfill(
        authenticated
          ? { status: 200, json: session }
          : { status: 401, json: { code: 'SESSION_INVALID', message: '尚未登录' } },
      )
    } else if (path === '/api/v2/auth/admin-setup' && request.method() === 'GET') {
      await route.fulfill({ status: 200, json: { required: creationCalls < 2 } })
    } else if (path === '/api/v2/auth/admin-setup') {
      creationCalls++
      expect(request.method()).toBe('POST')
      const body = request.postDataJSON()
      expect(
        body.username === 'initial-admin' &&
          body.password === password &&
          body.display_name === '初始管理员',
      ).toBe(true)
      expect(request.headers()['x-csrf-token']).toBeUndefined()
      if (creationCalls === 1) {
        await route.fulfill({
          status: 409,
          json: { code: 'ADMINISTRATOR_CONFLICT', message: '管理员用户名不可用' },
        })
      } else {
        await route.fulfill({
          status: 201,
          json: {
            administrator_id: '0'.repeat(25) + '1',
            username: 'initial-admin',
            display_name: '初始管理员',
            secret,
            period: 30,
            digits: 6,
            algorithm: 'SHA1',
            created_at: new Date().toISOString(),
          },
        })
      }
    } else if (path === '/api/v2/admin/auth/login') {
      const body = request.postDataJSON()
      expect(
        body.username === 'initial-admin' && body.password === password && body.code === loginCode,
      ).toBe(true)
      expect(creationCalls).toBe(2)
      authenticated = true
      await route.fulfill({ status: 200, json: session })
    } else if (path === '/api/v2/admin/users') {
      await route.fulfill({ status: 200, json: { users: [] } })
    } else {
      await route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '测试接口未配置' } })
    }
  })
  const response = await page.goto('/admin/')
  expect(response?.headers()['content-security-policy']).toContain("script-src 'self'")
  await page.getByRole('button', { name: '创建首位管理员', exact: true }).click()
  await expect(page.getByRole('heading', { name: '创建首位管理员' })).toBeFocused()
  await chooseOption(page, '外观', '浅色')
  await page.screenshot({
    path: testInfo.outputPath('activation-light.png'),
    fullPage: true,
    animations: 'disabled',
  })
  const fillCreation = async () => {
    await page.getByLabel('管理员用户名').fill('initial-admin')
    await page.getByLabel('显示名称').fill('初始管理员')
    await page.getByLabel('设置密码').fill(password)
    await page.getByLabel('确认密码').fill(password)
  }
  await fillCreation()
  await page.getByRole('button', { name: '创建管理员', exact: true }).click()
  await expect(page.getByRole('alert')).toHaveText('管理员用户名不可用')
  await expect(page.getByLabel('设置密码')).toHaveValue('')
  await expect(page.getByLabel('确认密码')).toHaveValue('')
  await fillCreation()
  await page.getByRole('button', { name: '创建管理员', exact: true }).click()
  await expect(page.getByRole('heading', { name: '创建首位管理员' })).toBeFocused()
  await page.keyboard.press('Tab')
  await expect(page.getByLabel('验证器设置密钥')).toBeFocused()
  await page.getByRole('button', { name: '显示设置密钥' }).click()
  await expect(page.getByLabel('验证器设置密钥')).toHaveAttribute('type', 'text')
  await page.getByRole('button', { name: '隐藏设置密钥' }).click()
  await chooseOption(page, '外观', '深色')
  await page.screenshot({
    path: testInfo.outputPath('activation-setup-dark.png'),
    fullPage: true,
    animations: 'disabled',
    mask: [page.getByLabel('验证器设置密钥')],
  })
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true,
  )
  await page.screenshot({
    path: testInfo.outputPath('activation-mobile.png'),
    fullPage: true,
    animations: 'disabled',
    mask: [page.getByLabel('验证器设置密钥')],
  })
  await page.getByRole('button', { name: '返回登录', exact: true }).click()
  await expect(page.getByLabel('验证器设置密钥')).toHaveCount(0)
  await expect(page.getByLabel('密码', { exact: true })).toHaveValue('')
  await expect(page.getByLabel('动态验证码', { exact: true })).toHaveValue('')
  await page.getByLabel('管理员用户名').fill('initial-admin')
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByLabel('动态验证码', { exact: true }).fill(loginCode)
  await page.getByRole('button', { name: '登录管理平台' }).click()
  await expect(page.getByText('暂无用户')).toBeVisible()
  expect(creationCalls).toBe(2)
  expect(creationCalls).toBe(2)
  expect(problems).toEqual([])
  expect(await page.evaluate(() => Reflect.get(window, '__tombotTestCspViolations'))).toEqual([])
  expect(await page.evaluate(() => Object.keys(localStorage))).toEqual(['tombot-admin-theme'])
  expect(await page.evaluate(() => Object.keys(sessionStorage))).toEqual([])
})
