import { expect, test } from '@playwright/test'
import { randomBytes, randomUUID } from 'node:crypto'

const id = (value: number) => value.toString().padStart(26, '0')

for (const theme of ['浅色', '深色']) {
  test(`授权取消和成功使用顶部消息，不污染原操作区域（${theme}）`, async ({ page }, testInfo) => {
    let authorized = false
    let attempts = 0
    let issued = 0
    let finish: (() => Promise<void>) | undefined
    await page.route('**/api/v2/admin/**', async (route) => {
      const path = new URL(route.request().url()).pathname.replace('/api/v2/admin', '')
      if (path === '/auth/session') {
        await route.fulfill({
          json: {
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
          },
        })
      } else if (path === '/auth/verify') {
        authorized = true
        await route.fulfill({ status: 204 })
      } else if (path === '/computer-nodes/enrollment-tokens') {
        attempts++
        if (!authorized)
          await route.fulfill({
            status: 403,
            json: { code: 'VERIFICATION_REQUIRED', message: '该操作需要验证管理身份' },
          })
        else
          finish = async () => {
            issued++
            await route.fulfill({
              status: 201,
              json: {
                id: id(4),
                token: randomBytes(32).toString('base64url'),
                expires_at: new Date(Date.now() + 86_400_000).toISOString(),
              },
            })
          }
      } else await route.fulfill({ json: { nodes: [] } })
    })
    await page.goto('/admin/computer-nodes')
    await page.getByRole('button', { name: /^账户菜单/ }).click()
    await page.getByRole('menuitemradio', { name: theme }).click()
    await page.getByRole('button', { name: '生成接入凭据' }).click()
    const operation = page.getByRole('dialog', { name: '生成接入凭据' })
    const issue = operation.getByRole('button', { name: '生成凭据', exact: true })
    const verification = page.getByRole('dialog', { name: '验证管理身份' })
    for (const cancel of ['关闭', 'Escape']) {
      await issue.click()
      await expect(verification).toBeVisible()
      if (cancel === '关闭') await verification.getByRole('button', { name: '关闭' }).click()
      else await page.keyboard.press('Escape')
      await expect(verification).toHaveCount(0)
      await expect(page.locator('.messages')).toContainText('已取消授权，操作已终止')
      await expect(operation.getByRole('alert')).toHaveCount(0)
      await expect(issue).toBeEnabled()
      await expect(operation.getByRole('textbox', { name: '接入凭据码' })).toHaveCount(0)
      expect(issued).toBe(0)
    }
    expect(attempts).toBe(2)
    const visibleOnTop = await page
      .locator('.messages')
      .evaluate((element) => element.matches(':popover-open'))
    expect(visibleOnTop).toBe(true)
    await page.screenshot({ path: testInfo.outputPath('canceled-message.png'), fullPage: true })
    await issue.click()
    await verification.getByLabel('当前密码').fill(randomUUID())
    await verification
      .getByLabel('动态验证码')
      .fill(String(randomBytes(4).readUInt32BE() % 1_000_000).padStart(6, '0'))
    await verification.getByRole('button', { name: '确认身份' }).click()
    await expect(page.locator('.messages')).toContainText('授权成功，正在继续操作')
    await expect(page.getByText('操作成功', { exact: true })).toHaveCount(0)
    await expect.poll(() => finish !== undefined).toBe(true)
    await finish!()
    await expect(page.locator('.messages')).toContainText('操作成功')
    await expect(operation.getByRole('textbox', { name: '接入凭据码' })).toBeVisible()
    await expect(operation.getByRole('alert')).toHaveCount(0)
    expect(issued).toBe(1)
    expect(attempts).toBe(4)
    await page.setViewportSize({ width: 390, height: 844 })
    await page.screenshot({
      path: testInfo.outputPath('success-message-mobile.png'),
      fullPage: true,
      mask: [operation.getByRole('textbox', { name: '接入凭据码' })],
    })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  })
}
