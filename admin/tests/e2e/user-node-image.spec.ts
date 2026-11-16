import { expect, test } from '@playwright/test'
import { id, session } from '../fixtures'

for (const theme of ['dark', 'light'] as const) {
  test(`创建电脑自动关联节点镜像：${theme}`, async ({ page, context }, testInfo) => {
    await page.emulateMedia({ colorScheme: theme })
    const references = ['a', 'b'].map((letter) => `tombot-computer@sha256:${letter.repeat(64)}`)
    const requests: string[] = []
    await context.route('**/api/v2/**', async (route) => {
      const path = new URL(route.request().url()).pathname.replace('/api/v2/admin', '')
      requests.push(path)
      if (path === '/api/v2/auth/admin-setup') {
        await route.fulfill({ json: { required: false } })
      } else if (path.startsWith('/auth/')) {
        await route.fulfill({ json: session() })
      } else if (path === '/computer-nodes') {
        await route.fulfill({
          json: {
            nodes: references.map((reference, index) => ({
              id: id(4 + index),
              name: `机房节点${index + 1}`,
              status: 'ready',
              capacity: 4,
              reserved: 0,
              image: { id: id(6 + index), reference, is_enabled: true },
            })),
          },
        })
      } else if (path === '/users') {
        await route.fulfill({ json: { users: [] } })
      } else {
        await route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '接口不存在' } })
      }
    })
    await page.goto('/admin/users')
    await page.getByRole('button', { name: '创建用户', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: '创建用户与电脑' })
    await expect(dialog.getByRole('combobox', { name: '电脑镜像' })).toHaveCount(0)
    await expect(dialog.getByRole('button', { name: '创建用户与电脑', exact: true })).toBeDisabled()
    await dialog.getByRole('combobox', { name: '电脑节点' }).click()
    await page.getByRole('option', { name: /^机房节点1/ }).click()
    await expect(dialog.getByLabel('电脑镜像')).toHaveText(references[0]!)
    await expect(dialog.getByText(/本页.*条/)).toHaveCount(0)
    await page.screenshot({ path: testInfo.outputPath(`node-image-${theme}.png`), fullPage: true })
    await page.setViewportSize({ width: 390, height: 844 })
    await dialog.getByRole('combobox', { name: '电脑节点' }).click()
    await page.getByRole('option', { name: /^机房节点2/ }).click()
    await expect(dialog.getByLabel('电脑镜像')).toHaveText(references[1]!)
    expect(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
      true,
    )
    await page.screenshot({
      path: testInfo.outputPath(`node-image-mobile-${theme}.png`),
      fullPage: true,
    })
    expect(requests).not.toContain('/computer-images')
  })
}
