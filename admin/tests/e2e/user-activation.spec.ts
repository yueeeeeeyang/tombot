import { expect, test } from '@playwright/test'
import { randomBytes, randomUUID } from 'node:crypto'
import { chooseOption } from './select'

test('签发后打开公开激活链接，刷新、窄屏及激活流程可用', async ({ page, context }, testInfo) => {
  const token = randomBytes(32).toString('base64url')
  const password = randomUUID()
  const id = (value: number) => String(value).padStart(26, '0')
  const session = {
    principal: {
      username: 'test-admin',
      display_name: '测试管理员',
      user_id: id(1),
      session_id: id(2),
      device_id: id(3),
      role: 'admin',
      scope: 'admin',
    },
    csrf_token: randomUUID(),
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
    expires_at: new Date(Date.now() + 3600_000).toISOString(),
  }
  let activations = 0
  const publicRequests: string[] = []
  const problems: string[] = []
  await context.addInitScript(() => {
    const violations: string[] = []
    Reflect.set(window, '__activationCspViolations', violations)
    document.addEventListener('securitypolicyviolation', (event) =>
      violations.push(event.violatedDirective),
    )
  })
  await context.route('**/api/v2/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (request.frame().url().endsWith('/activate')) publicRequests.push(path)
    expect(request.url().includes(token) || request.url().includes(password)).toBe(false)
    if (path === '/api/v2/admin/auth/session') await route.fulfill({ json: session })
    else if (path === '/api/v2/admin/users')
      await route.fulfill({
        json: {
          users: [
            {
              id: id(4),
              username: 'new-user',
              display_name: '新用户',
              role: 'user',
              status: 'pending_activation',
              computer_status: 'ready',
              creation_operation_id: id(5),
              created_at: new Date().toISOString(),
            },
          ],
        },
      })
    else if (path === `/api/v2/admin/users/${id(4)}/activation`)
      await route.fulfill({
        json: {
          user_id: id(4),
          activation_token: token,
          expires_at: new Date(Date.now() + 3600_000).toISOString(),
        },
      })
    else if (path === '/api/v2/auth/activate') {
      activations++
      expect(request.method()).toBe('POST')
      expect(
        request.postDataJSON().activation_token === token &&
          request.postDataJSON().password === password,
      ).toBe(true)
      expect(request.headers()['x-csrf-token']).toBeUndefined()
      await route.fulfill(
        activations === 1
          ? { status: 400, json: { code: 'ACTIVATION_INVALID', message: '激活信息无效或已过期' } }
          : { status: 204 },
      )
    } else
      await route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '未预期的请求' } })
  })
  await page.goto('/admin/users')
  await page.getByRole('button', { name: '签发激活材料' }).click()
  await page.getByRole('button', { name: '签发新的激活材料' }).click()
  const link = page.getByRole('link', { name: '打开激活页面' })
  await expect(link).toHaveAttribute('href', new URL('/activate', page.url()).href)
  await page.screenshot({
    path: testInfo.outputPath('activation-material.png'),
    mask: [page.getByLabel('一次性激活码')],
  })
  const opened = context.waitForEvent('page')
  await link.click()
  const activation = await opened
  activation.on('pageerror', (error) => problems.push(error.message))
  await expect(activation.getByRole('heading', { name: '激活 TomBot 账户' })).toBeVisible()
  await activation.reload()
  await expect(activation.getByRole('heading', { name: '激活 TomBot 账户' })).toBeVisible()
  expect(publicRequests).toEqual([])
  await chooseOption(activation, '外观', '浅色')
  await expect(activation.getByRole('button', { name: '激活账户', exact: true })).toHaveCSS(
    'background-color',
    'rgb(37, 37, 37)',
  )
  await activation.screenshot({ path: testInfo.outputPath('user-activation-light.png') })
  await activation.setViewportSize({ width: 390, height: 844 })
  await chooseOption(activation, '外观', '深色')
  await expect(activation.getByRole('button', { name: '激活账户', exact: true })).toHaveCSS(
    'background-color',
    'rgb(242, 242, 242)',
  )
  await activation.screenshot({
    path: testInfo.outputPath('user-activation-mobile.png'),
    fullPage: true,
  })
  expect(await activation.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(
    true,
  )
  for (let attempt = 0; attempt < 2; attempt++) {
    await activation.getByLabel('激活码', { exact: true }).fill(token)
    await activation.getByLabel('设置密码', { exact: true }).fill(password)
    await activation.getByLabel('确认密码', { exact: true }).fill(password)
    if (attempt === 0)
      await activation.getByRole('button', { name: '激活账户', exact: true }).click()
    else await activation.getByLabel('确认密码', { exact: true }).press('Enter')
    if (attempt === 0) {
      await expect(activation.getByRole('alert')).toContainText('激活码无效、已过期或已使用')
      await expect(activation.getByLabel('激活码', { exact: true })).toHaveValue('')
    }
  }
  await expect(activation.getByRole('heading', { name: '账户已激活' })).toBeFocused()
  await expect(activation.getByRole('status')).toContainText('登录用户平台')
  await expect(activation.getByRole('link', { name: '登录用户平台' })).toHaveAttribute(
    'href',
    '/login',
  )
  expect(publicRequests).toEqual(['/api/v2/auth/activate', '/api/v2/auth/activate'])
  const stored = await activation.evaluate(() =>
    JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }),
  )
  expect(stored.includes(token) || stored.includes(password)).toBe(false)
  expect(await activation.evaluate(() => Reflect.get(window, '__activationCspViolations'))).toEqual(
    [],
  )
  expect(problems).toEqual([])
})
