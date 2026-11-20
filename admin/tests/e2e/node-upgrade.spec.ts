import { expect, test } from '@playwright/test'
import { createHash } from 'node:crypto'
import { id, session } from '../fixtures'
import { nodeDetail } from '../node-fixture'

for (const theme of ['light', 'dark'] as const) {
  test(`远程升级在 ${theme} 主题可切换版本与上传视图，窄屏保留操作按钮`, async ({
    page,
  }, testInfo) => {
    await page.emulateMedia({ colorScheme: theme })
    const node = { ...nodeDetail(), name: 'yueyang-server0-7w3mgg' }
    const nodes = [node, { ...node, id: id(7), name: 'tomnode1-sembka' }]
    const bytes = Buffer.from('测试发布包内容')
    const digest = (value: Buffer | string) => createHash('sha256').update(value).digest('hex')
    const artifact = digest(digest(bytes))
    const approved = { id: artifact, label: '新版', platform: 'linux/amd64' }
    let received = false
    await page.route('**/api/v2/**', async (route) => {
      const path = new URL(route.request().url()).pathname.replace('/api/v2/admin', '')
      if (path === '/api/v2/auth/admin-setup') return route.fulfill({ json: { required: false } })
      if (path.startsWith('/auth/')) return route.fulfill({ json: session() })
      if (path === '/node-updates/uploads') return route.fulfill({ json: { next: 0 } })
      if (path === '/node-updates/releases') {
        if (route.request().postDataJSON().artifact === artifact)
          return route.fulfill({ json: approved })
        return route.fulfill({
          status: 400,
          json: { code: 'PACKAGE_INVALID', message: '离线包校验失败' },
        })
      }
      if (path.startsWith('/node-updates/parts/')) {
        if (path === '/node-updates/parts/' + artifact + '/0/' + digest(bytes)) {
          expect(route.request().postDataBuffer()).toEqual(bytes)
          received = true
        }
        return route.fulfill({ json: {} })
      }
      if (path === '/computer-nodes') return route.fulfill({ json: { nodes } })
      if (path === '/node-updates/status')
        return route.fulfill({
          json: {
            releases: [
              { id: 'release-a', label: '0.2.2-rc.1', platform: 'linux/amd64' },
              ...(received ? [approved] : []),
            ],
            jobs: [
              {
                id: 'job-a',
                label: '0.2.1',
                node: node.id,
                state: 'failed',
                step: 'verification_quarantined/agent_retirement_entry_unknown',
              },
            ],
          },
        })
      return route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '未找到' } })
    })
    await page.goto('/admin/computer-nodes')
    const trigger = page.getByRole('button', { name: '远程升级' })
    await trigger.click()
    const dialog = page.getByRole('dialog')
    const version = dialog.getByRole('combobox', { name: '目标版本' })
    await expect(dialog.getByText(/节点验收已隔离 · agent_retirement_entry_unknown/)).toBeVisible()
    await version.focus()
    await page.keyboard.press('Enter')
    await page.keyboard.press('Enter')
    await expect(version).toContainText('0.2.2-rc.1')
    await dialog.getByRole('checkbox', { name: node.name }).check()
    await expect(dialog.getByRole('button', { name: '升级所选节点（1）' })).toBeEnabled()
    await dialog.screenshot({ path: testInfo.outputPath(`dispatch-${theme}.png`) })
    await dialog.getByRole('button', { name: '上传新版本' }).click()
    await expect(dialog.locator('input[type=file]')).toHaveCount(1)
    await expect(dialog.getByLabel('程序包或电脑镜像包')).toHaveAccessibleDescription(/无需解压/)
    await dialog.screenshot({ path: testInfo.outputPath(`upload-${theme}.png`) })
    await page.setViewportSize({ width: 360, height: 540 })
    const archive = dialog.getByLabel('程序包或电脑镜像包')
    await archive.setInputFiles({
      name: 'tomnode-linux-amd64-' + 'release-'.repeat(10) + 'offline.tar.gz',
      mimeType: 'application/gzip',
      buffer: Buffer.from('test'),
    })
    await dialog.getByRole('button', { name: '上传发布包' }).click()
    await expect(dialog.getByRole('alert')).toContainText('离线包校验失败')
    await expect(dialog.getByRole('button', { name: '返回', exact: true })).toBeInViewport()
    expect(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
      true,
    )
    await dialog.screenshot({ path: testInfo.outputPath(`upload-mobile-${theme}.png`) })
    await dialog.getByRole('button', { name: '返回', exact: true }).click()
    await expect(version).toContainText('0.2.2-rc.1')
    await dialog.getByRole('radio', { name: '所有同架构节点' }).check()
    await expect(dialog.getByText(/包含其他分页/)).toBeVisible()
    await expect(dialog.getByRole('button', { name: '升级所有同架构节点' })).toBeInViewport()
    expect(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
      true,
    )
    await dialog.screenshot({ path: testInfo.outputPath(`dispatch-mobile-${theme}.png`) })
    await dialog.getByRole('button', { name: '上传新版本' }).click()
    await archive.setInputFiles({
      name: 'node.tar.gz',
      mimeType: 'application/gzip',
      buffer: bytes,
    })
    await dialog.getByRole('button', { name: '上传发布包' }).click()
    await expect(version).toContainText('新版')
    expect(received).toBe(true)
    await page.keyboard.press('Escape')
    await expect(dialog).toHaveCount(0)
    await expect(trigger).toBeFocused()
  })
}
