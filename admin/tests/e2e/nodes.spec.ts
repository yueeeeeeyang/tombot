import { expect, test, type Locator } from '@playwright/test'
import { randomBytes, randomUUID } from 'node:crypto'

const id = (value: number) => value.toString().padStart(26, '0')

async function expectInlineActions(dialog: Locator, primary: Locator) {
  const close = dialog.getByRole('button', { name: '关闭', exact: true })
  await expect(primary).toBeVisible()
  const closeBounds = (await close.boundingBox())!
  const primaryBounds = (await primary.boundingBox())!
  const footerBounds = (await dialog.locator('.dialog-actions').boundingBox())!
  expect(Math.abs(closeBounds.y - primaryBounds.y)).toBeLessThan(1)
  expect(Math.abs(closeBounds.height - primaryBounds.height)).toBeLessThan(1)
  expect(primaryBounds.x - closeBounds.x - closeBounds.width).toBeGreaterThanOrEqual(10)
  expect(
    Math.abs(primaryBounds.x + primaryBounds.width - footerBounds.x - footerBounds.width),
  ).toBeLessThan(1)
  expect(closeBounds.width).toBeLessThan(footerBounds.width / 2)
  await expect(primary).toHaveCSS('text-decoration-line', 'none')
}

for (const theme of ['浅色', '深色']) {
  test(`节点凭据复制与自动接入目录展示（${theme}）`, async ({ page }, testInfo) => {
    await page.addInitScript(() => {
      Object.defineProperty(navigator, 'clipboard', {
        value: {
          writeText: async (value: string) => {
            Reflect.set(window, 'copiedEnrollment', value)
          },
        },
      })
    })
    const token =
      'tomnode1.' +
      Buffer.from(
        JSON.stringify({
          url: 'https://api.example.test',
          fingerprints: [randomBytes(32).toString('hex')],
          token: randomBytes(32).toString('base64url'),
        }),
      ).toString('base64url')
    let issued = 0
    let attempts = 0
    let connected = false
    await page.route('**/api/v2/admin/**', async (route) => {
      const path = new URL(route.request().url()).pathname.replace('/api/v2/admin', '')
      let body: unknown
      if (path === '/auth/session')
        body = {
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
          expires_at: new Date(Date.now() + 3_600_000).toISOString(),
        }
      else if (path === '/computer-nodes/enrollment-tokens') {
        expect(route.request().method()).toBe('POST')
        expect(route.request().headers()['x-csrf-token']).toBeTruthy()
        expect(route.request().postDataJSON()).toEqual({})
        attempts++
        if (attempts === 1) {
          await route.fulfill({
            status: 503,
            json: { code: 'NODE_CATALOG_UNAVAILABLE', message: '接入服务暂不可用' },
          })
          return
        }
        issued++
        body = { id: id(4), token, expires_at: new Date(Date.now() + 86_400_000).toISOString() }
      } else if (path === '/computer-nodes')
        body = {
          nodes: connected
            ? [
                {
                  id: id(5),
                  name: '自动接入的电脑节点',
                  status: 'bootstrapping',
                  capacity: 4,
                  reserved: 0,
                  image: null,
                },
              ]
            : [],
        }
      else body = { users: [] }
      await route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) })
    })
    await page.goto('/admin/')
    await expect(page.getByRole('heading', { name: '暂无用户', exact: true })).toBeVisible()
    await expect(page.getByText(/本页 0 条/)).toHaveCount(0)
    await expect(page.locator('.pagination')).toHaveCount(0)
    await expect(page.locator('.empty-state')).toHaveCSS('border-top-style', 'dashed')
    await page.getByRole('button', { name: /^账户菜单/ }).click()
    await page.getByRole('menuitemradio', { name: theme }).click()
    await page.screenshot({
      path: testInfo.outputPath('empty-users.png'),
      fullPage: true,
      animations: 'disabled',
    })
    await page.getByRole('link', { name: '电脑节点', exact: true }).click()
    await page.getByRole('button', { name: '生成接入凭据' }).click()
    const dialog = page.getByRole('dialog', { name: '生成接入凭据' })
    const issue = dialog.getByRole('button', { name: '生成凭据', exact: true })
    await expectInlineActions(dialog, issue)
    await issue.click()
    await expect(dialog.getByRole('alert')).toContainText('接入服务暂不可用')
    for (const width of [1280, 390]) {
      await page.setViewportSize({ width, height: 900 })
      await expectInlineActions(dialog, issue)
      await page.screenshot({
        path: testInfo.outputPath(`enrollment-error-${width}.png`),
        fullPage: true,
        animations: 'disabled',
      })
    }
    await issue.click()
    const copyButton = dialog.getByRole('button', { name: '复制凭据码' })
    const code = dialog.getByRole('textbox', { name: '接入凭据码' })
    await expect(code).toHaveValue(token)
    await expect(code).toHaveAttribute('readonly', '')
    await expect(dialog.getByRole('link', { name: /下载/ })).toHaveCount(0)
    for (const width of [390, 1280]) {
      await page.setViewportSize({ width, height: 900 })
      await expectInlineActions(dialog, copyButton)
      await page.screenshot({
        path: testInfo.outputPath(`enrollment-code-${width}.png`),
        fullPage: true,
        mask: [code],
      })
    }
    await copyButton.click()
    await expect(dialog.getByRole('button', { name: '已复制' })).toBeVisible()
    expect(await page.evaluate(() => Reflect.get(window, 'copiedEnrollment'))).toBe(token)
    expect(issued).toBe(1)
    await page.getByRole('button', { name: '关闭', exact: true }).click()
    connected = true
    await page.getByRole('button', { name: '刷新节点' }).click()
    await expect(page.getByText('自动接入的电脑节点')).toBeVisible()
    await page.setViewportSize({ width: 390, height: 844 })
    await expect(page.getByText('自动接入的电脑节点')).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  })
}
