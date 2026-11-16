import { chooseOption } from './select'
import { expect, test } from '@playwright/test'
import { randomUUID } from 'node:crypto'

const id = (value: number) => value.toString().padStart(26, '0')
const timestamp = new Date().toISOString()

// 网络替身只在浏览器测试中注册；生产构建没有演示数据或认证旁路。
test('管理页面主题、窄屏、模态焦点与模型启用流程', async ({ page }, testInfo) => {
  const problems: string[] = []
  page.on('pageerror', (error) => problems.push(error.message))
  await page.addInitScript(() => {
    const violations: string[] = []
    Reflect.set(window, '__tombotTestCspViolations', violations)
    document.addEventListener('securitypolicyviolation', (event) =>
      violations.push(event.violatedDirective),
    )
  })
  const provider = {
    id: id(4),
    name: '团队模型服务',
    protocol: 'openai_compat',
    base_url: 'https://models.example.test',
    is_enabled: true,
    revision: 1,
    created_at: timestamp,
  }
  const model = {
    id: id(5),
    provider_id: provider.id,
    model_key: 'team-model',
    display_name: '团队通用模型',
    context_limit: 128000,
    output_limit: 8192,
    supports_tools: true,
    supports_vision: true,
    metadata: {},
    is_enabled: true,
    revision: 1,
  }
  const policy = {
    id: id(6),
    name: '日常工作',
    is_default: false,
    is_enabled: true,
    revision: 1,
    current_version: null as number | null,
  }
  const versions: object[] = []
  let enableCount = 0
  await page.route('**/api/v2/admin/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname.replace('/api/v2/admin', '')
    const method = route.request().method()
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
    else if (path === '/users')
      body = {
        users: [
          {
            id: id(1),
            username: 'administrator',
            display_name: '平台管理员',
            role: 'admin',
            status: 'active',
            computer_status: 'ready',
            creation_operation_id: null,
            created_at: timestamp,
          },
          {
            id: id(8),
            username: 'design',
            display_name: '设计工作账户',
            role: 'user',
            status: 'pending_activation',
            computer_status: 'ready',
            creation_operation_id: id(9),
            created_at: timestamp,
          },
        ],
      }
    else if (path === `/model-providers/${provider.id}`) body = { provider }
    else if (path === '/model-providers') body = { providers: [provider] }
    else if (path === `/model-providers/${provider.id}/models`) body = { models: [model] }
    else if (path === `/model-providers/${provider.id}/credentials`) body = { credentials: [] }
    else if (path === `/model-policies/${policy.id}`) body = { policy }
    else if (path === '/model-policies') body = { policies: [policy] }
    else if (path === `/model-policies/${policy.id}/versions` && method === 'GET')
      body = { versions }
    else if (path === `/model-policies/${policy.id}/versions` && method === 'POST') {
      expect(route.request().postDataJSON()).toEqual({
        routing_mode: 'priority',
        candidates: [{ model_id: model.id, priority: 1, weight: 1, output_limit: 4096 }],
      })
      policy.revision++
      versions.push({
        policy_id: policy.id,
        version: 1,
        routing_mode: 'priority',
        candidates: [
          {
            position: 1,
            provider_id: provider.id,
            model_id: model.id,
            credential_id: null,
            priority: 1,
            weight: 1,
            configuration: {
              ...model,
              protocol: provider.protocol,
              base_url: provider.base_url,
              output_limit: 4096,
              prices: {},
            },
          },
        ],
        is_enabled: false,
        verified_at: null,
      })
      body = { version: versions[0] }
    } else if (path === `/model-policies/${policy.id}/versions/1/enable`) {
      expect(route.request().postDataJSON()).toEqual({ expected_revision: 2 })
      expect(route.request().headers()['x-csrf-token']).toBeTruthy()
      enableCount++
      policy.current_version = 1
      policy.revision++
      versions[0] = { ...versions[0], is_enabled: true, verified_at: timestamp }
      body = { version: versions[0] }
    } else if (path === '/computer-nodes') body = { nodes: [] }
    else {
      await route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '测试接口未配置' } })
      return
    }
    await route.fulfill({ status: 200, json: body })
  })
  const response = await page.goto('/admin/')
  expect(response?.headers()['content-security-policy']).toContain("script-src 'self'")
  expect(response?.headers()['content-security-policy']).not.toContain('unsafe-eval')
  await expect(page.getByRole('heading', { name: '用户', exact: true })).toBeVisible()
  await expect(page.getByText('设计工作账户')).toBeVisible()
  await page.getByRole('button', { name: /^账户菜单/ }).click()
  await page.getByRole('menuitemradio', { name: '浅色' }).click()
  await page.screenshot({
    path: testInfo.outputPath('users-light.png'),
    fullPage: true,
    animations: 'disabled',
  })
  await page.getByRole('button', { name: '创建用户', exact: true }).click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await expect(page.getByRole('button', { name: '创建用户', exact: true })).toBeFocused()
  await page.getByRole('link', { name: '模型供应商', exact: true }).click()
  await page.getByRole('button', { name: /^账户菜单/ }).click()
  await page.getByRole('menuitemradio', { name: '深色' }).click()
  await expect(page.getByText('团队模型服务')).toBeVisible()
  await page.screenshot({
    path: testInfo.outputPath('models-dark.png'),
    fullPage: true,
    animations: 'disabled',
  })
  await page.getByRole('link', { name: '模型策略', exact: true }).click()
  await page.getByRole('link', { name: '版本管理' }).click()
  await page.getByRole('button', { name: '创建策略版本' }).click()
  await chooseOption(page, '候选供应商', '团队模型服务')
  await chooseOption(page, '候选模型', '团队通用模型')
  await page.getByRole('button', { name: '添加', exact: true }).click()
  await page.getByText('参数设置', { exact: false }).click()
  await page.getByLabel('团队通用模型 输出上限').fill('4096')
  await page.getByRole('button', { name: '保存版本' }).click()
  await expect(page.getByText('已停用', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: '启用', exact: true }).click()
  await page.getByRole('button', { name: '确认启用版本 v1' }).click()
  await expect(page.getByText('v1', { exact: true })).toBeVisible()
  await expect.poll(() => enableCount).toBe(1)
  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('button', { name: '切换导航' }).click()
  await page.getByRole('link', { name: '用户', exact: true }).click()
  await expect(page.getByText('设计工作账户')).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true,
  )
  await page.screenshot({
    path: testInfo.outputPath('users-mobile.png'),
    fullPage: true,
    animations: 'disabled',
  })
  expect(problems).toEqual([])
  expect(await page.evaluate(() => Reflect.get(window, '__tombotTestCspViolations'))).toEqual([])
  expect(await page.evaluate(() => Object.keys(localStorage))).toEqual(['tombot-admin-theme'])
})
