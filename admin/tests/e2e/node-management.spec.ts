import { expect, test } from '@playwright/test'
import { nodeDetail } from '../node-fixture'
import { session } from '../fixtures'

for (const theme of ['dark', 'light'] as const) {
  test(`节点链接和编辑弹窗在${theme}主题和窄屏中可用`, async ({ page, context }, testInfo) => {
    await page.emulateMedia({ colorScheme: theme })
    let node = nodeDetail()
    const writes: unknown[] = []
    await context.route('**/api/v2/**', async (route) => {
      const request = route.request()
      const path = new URL(request.url()).pathname.replace('/api/v2/admin', '')
      if (path === '/api/v2/auth/admin-setup') {
        await route.fulfill({ json: { required: false } })
        return
      }
      if (path.startsWith('/auth/')) {
        await route.fulfill({ json: session() })
        return
      }
      if (path === '/computer-nodes') {
        await route.fulfill({ json: { nodes: [{ ...node, verification_status: 'quarantined' }] } })
        return
      }
      if (path === `/computer-nodes/${node.id}`) {
        if (request.method() === 'PUT') {
          const update = request.postDataJSON()
          writes.push(update)
          node = { ...node, ...update, configuration_version: 3 }
        }
        await route.fulfill({ json: node })
        return
      }
      await route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '未找到' } })
    })
    await page.goto('/admin/computer-nodes')
    await expect(page.getByText('验收已隔离')).toBeVisible()
    const nameLink = page.getByRole('link', { name: node.name })
    const detailLink = page.getByRole('link', { name: '详情', exact: true })
    const editButton = page.getByRole('button', { name: '编辑', exact: true })
    for (const link of [nameLink, detailLink, editButton]) {
      await expect(link).toHaveCSS('text-decoration-line', 'none')
      await link.hover()
      await expect(link).toHaveCSS('text-decoration-line', 'none')
    }
    await page.screenshot({ path: testInfo.outputPath(`node-list-${theme}.png`), fullPage: true })
    await editButton.click()
    const dialog = page.getByRole('dialog', { name: '编辑节点' })
    await expect(dialog).toBeVisible()
    await expect(page).toHaveURL(/\/admin\/computer-nodes$/)
    await dialog.getByLabel('节点名称').fill('尚未保存')
    await dialog.getByRole('button', { name: '取消', exact: true }).click()
    await expect(dialog).toHaveCount(0)
    await expect(editButton).toBeFocused()
    expect(writes).toEqual([])
    await page.getByRole('link', { name: node.name }).click()
    await expect(page).toHaveURL(new RegExp(`/admin/computer-nodes/${node.id}$`))
    await page.reload()
    await expect(page.getByRole('heading', { name: node.name })).toBeVisible()
    await expect(page.getByText('container_identity_mismatch')).toBeVisible()
    expect(
      await page.locator('body').evaluate((element) => element.scrollWidth),
    ).toBeLessThanOrEqual(1280)
    await page.screenshot({ path: testInfo.outputPath(`node-detail-${theme}.png`), fullPage: true })
    await page.getByRole('button', { name: '编辑节点', exact: true }).click()
    await expect(dialog).toBeVisible()
    await expect(page).toHaveURL(new RegExp(`/admin/computer-nodes/${node.id}$`))
    await page.getByLabel('节点名称').fill('机房一号节点')
    await page.getByLabel('准入容量', { exact: true }).fill('8')
    await page.getByRole('combobox', { name: '接收新电脑' }).click()
    await page.getByRole('option', { name: '暂停接收' }).click()
    await page.screenshot({ path: testInfo.outputPath(`node-edit-${theme}.png`), fullPage: true })
    await page.getByRole('button', { name: '保存修改' }).click()
    await expect(dialog).toHaveCount(0)
    await expect(page.getByRole('heading', { name: '机房一号节点' })).toBeVisible()
    expect(writes).toEqual([
      { name: '机房一号节点', capacity: 8, is_draining: true, configuration_version: 2 },
    ])
    await page.setViewportSize({ width: 390, height: 844 })
    expect(
      await page.locator('body').evaluate((element) => element.scrollWidth),
    ).toBeLessThanOrEqual(390)
    await page.screenshot({
      path: testInfo.outputPath(`node-detail-mobile-${theme}.png`),
      fullPage: true,
    })
    await page.getByRole('button', { name: '编辑节点', exact: true }).click()
    await expect(page.getByLabel('节点名称')).toHaveValue('机房一号节点')
    expect(
      await page.locator('body').evaluate((element) => element.scrollWidth),
    ).toBeLessThanOrEqual(390)
    expect(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
      true,
    )
    await page.getByRole('button', { name: '保存修改' }).scrollIntoViewIfNeeded()
    await expect(page.getByRole('button', { name: '取消', exact: true })).toBeVisible()
    await page.screenshot({
      path: testInfo.outputPath(`node-edit-mobile-${theme}.png`),
      fullPage: true,
    })
    await page.keyboard.press('Escape')
    await expect(dialog).toHaveCount(0)
    await expect(page.getByRole('button', { name: '编辑节点', exact: true })).toBeFocused()
    await expect(page).toHaveURL(new RegExp(`/admin/computer-nodes/${node.id}$`))
    expect(writes).toHaveLength(1)
  })
}
