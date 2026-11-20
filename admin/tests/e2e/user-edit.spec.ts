import { expect, test } from '@playwright/test'
import { id, session } from '../fixtures'

test('禁用后编辑资料并在迁移中重新打开进度', async ({ page }) => {
  const row = {
    id: id(4),
    username: 'qa-user',
    display_name: '原名称',
    status: 'active',
    revision: 1,
    computer_status: 'ready',
    creation_operation_id: null,
    created_at: new Date().toISOString(),
  }
  const computer = {
    node_id: id(5),
    image_id: id(6),
    resources: {
      cpu_millis: 1000,
      memory_bytes: 2 * 1024 ** 3,
      disk_bytes: 2 * 1024 ** 3,
      process_limit: 128,
    },
  }
  let settings = {
    username: row.username,
    display_name: row.display_name,
    revision: row.revision,
    generation: 1,
    node_name: '原节点',
    image_reference: '原镜像',
    limits: {
      concurrent_model_calls: 2,
      concurrent_desktops: 1,
      queued_runs: 10,
      monthly_token_limit: null,
    },
    computer,
    has_avatar: false,
  }
  let progress: unknown = null
  await page.route('**/api/v2/**', async (route) => {
    const req = route.request()
    const path = new URL(req.url()).pathname
    let body
    if (path.endsWith('/admin-setup')) body = { required: false }
    else if (path.includes('/auth/')) body = session()
    else if (path.endsWith('/users')) body = { users: [row] }
    else if (path.endsWith('/computer-nodes'))
      body = {
        nodes: [
          {
            id: id(5),
            name: '原节点',
            status: 'ready',
            reserved: 1,
            capacity: 1,
            image: { id: id(6), reference: '原镜像', is_enabled: true },
          },
        ],
      }
    else if (path.endsWith('/status')) {
      expect(req.postDataJSON()).toEqual({ enabled: false, revision: 1 })
      row.status = 'disabled'
      row.revision++
      settings.revision = row.revision
      body = { enabled: false }
    } else if (path.endsWith('/settings')) {
      if (req.method() === 'PUT') {
        const changes = req.postDataJSON()
        expect(changes).not.toHaveProperty('username')
        expect(changes.computer.resources.disk_bytes).toBe(3 * 1024 ** 3)
        row.revision++
        row.display_name = changes.display_name
        settings = { ...settings, display_name: row.display_name, revision: row.revision }
        progress = {
          stage: 'copying',
          copied: 1024 ** 3,
          total: 2 * 1024 ** 3,
          paused: true,
          failure: 'disk_copy_digest_mismatch',
          target: changes.computer,
        }
      }
      body = { ...settings, progress }
    } else {
      return route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '接口不存在' } })
    }
    return route.fulfill({ json: body })
  })
  await page.goto('/admin/users')
  await expect(page.getByRole('button', { name: '编辑', exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: '禁用', exact: true }).click()
  await page.getByRole('button', { name: '确认禁用' }).click()
  await page.getByRole('button', { name: '编辑', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '编辑禁用用户' })
  await expect(dialog.getByLabel('用户名')).toHaveAttribute('readonly', '')
  await dialog.getByLabel('显示名称').fill('新名称')
  await dialog.getByLabel('磁盘 GiB').fill('3')
  await dialog.getByRole('button', { name: '保存修改' }).click()
  await expect(dialog.getByRole('status')).toContainText('50.0%')
  await dialog.getByRole('button', { name: '关闭', exact: true }).last().click()
  await expect(page.getByText('新名称', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: '编辑', exact: true }).click()
  await expect(dialog.getByRole('status')).toContainText('复制块校验不一致')
  await expect(dialog.getByRole('button', { name: '保存并重试原迁移' })).toBeVisible()
})
