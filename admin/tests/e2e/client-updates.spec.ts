import { expect, test } from '@playwright/test'
import { createHash } from 'node:crypto'
import { id, session } from '../fixtures'

test('刷新升级管理页后选择平台、填写说明并发布实际文件', async ({ page }) => {
  const content = Buffer.from('客户端发布验收')
  const digest = createHash('sha256').update(content).digest('hex')
  const auth = session()
  const releases: Record<string, unknown>[] = []
  let uploaded = false
  await page.route('**/api/v2/admin/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname.replace('/api/v2/admin', '')
    if (path.startsWith('/auth/')) return route.fulfill({ json: auth })
    if (path === '/client-updates' && request.method() === 'GET')
      return route.fulfill({
        json: {
          releases,
          platforms: { 'linux-x64': ['.deb'] },
          chunk_size: 4194304,
          max_size: 1073741824,
        },
      })
    expect(request.headers()['x-csrf-token']).toBe(auth.csrf_token)
    if (request.method() === 'PUT') {
      expect(path).toBe('/client-updates/parts/' + digest)
      expect(request.postDataBuffer()?.equals(content)).toBe(true)
      uploaded = true
      return route.fulfill({ json: {} })
    }
    expect(uploaded).toBe(true)
    expect(request.postDataJSON()).toEqual({
      version: '2.10.0',
      platform: 'linux-x64',
      extension: '.deb',
      size: content.length,
      chunks: [digest],
      notes: '改进重连',
    })
    releases.push(request.postDataJSON())
    return route.fulfill({ status: 201, json: { id: id(4) } })
  })
  await page.goto('/admin/client-updates')
  await page.getByRole('button', { name: '发布版本', exact: true }).click()
  await page.getByRole('combobox', { name: '目标平台' }).click()
  await page.getByRole('option', { name: 'Linux · x64' }).click()
  await page.getByLabel('版本号').fill('2.10.0')
  await page.getByLabel('升级内容').fill('改进重连')
  await page
    .getByLabel('安装包', { exact: true })
    .setInputFiles({ name: 'TomBot.deb', mimeType: 'application/octet-stream', buffer: content })
  await page.getByRole('button', { name: '上传并发布' }).click()
  await expect(page.getByRole('row', { name: /Linux · x64/ })).toContainText('改进重连')
  await page.reload()
  await expect(page.getByRole('row', { name: /Linux · x64/ })).toContainText('2.10.0')
})
