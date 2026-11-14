import { expect, test, type BrowserContext } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { id, provider, session } from '../fixtures'

const policy = {
  id: id(6),
  name: '路由测试策略',
  is_enabled: true,
  is_default: false,
  revision: 1,
  current_version: null,
}

async function mockDirectory(context: BrowserContext, authenticated = true) {
  const reads: string[] = []
  let unavailable = false
  await context.route('**/api/v2/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace('/api/v2/admin', '')
    if (path === '/api/v2/auth/admin-setup') {
      await route.fulfill({ json: { required: false } })
      return
    }
    if (path === '/auth/login') authenticated = true
    if (path.startsWith('/auth/')) {
      await route.fulfill(
        authenticated
          ? { json: session() }
          : { status: 401, json: { code: 'SESSION_INVALID', message: '请登录' } },
      )
      return
    }
    reads.push(path)
    expect(authenticated).toBe(true)
    if (unavailable) {
      await route.fulfill({
        status: 503,
        json: { code: 'MODEL_MANAGEMENT_UNAVAILABLE', message: '暂时无法读取配置' },
      })
      return
    }
    const responses: Record<string, unknown> = {
      '/users': { users: [] },
      '/administrators': { administrators: [] },
      '/computer-nodes': { nodes: [] },
      '/model-providers': { providers: [] },
      '/model-policies': { policies: [] },
      [`/model-providers/${provider.id}`]: { provider },
      [`/model-providers/${provider.id}/models`]: { models: [] },
      [`/model-policies/${policy.id}`]: { policy },
      [`/model-policies/${policy.id}/versions`]: { versions: [] },
    }
    await route.fulfill(
      path in responses
        ? { json: responses[path] }
        : { status: 404, json: { code: 'MODEL_CONFIGURATION_NOT_FOUND', message: '配置不存在' } },
    )
  })
  return {
    reads,
    setUnavailable: (value: boolean) => {
      unavailable = value
    },
  }
}

test('导航使用独立地址，刷新和前进后退恢复对应页面', async ({ page, context }) => {
  await mockDirectory(context)
  await page.goto('/admin/')
  await expect(page).toHaveURL(/\/admin\/users$/)
  for (const [title, path] of [
    ['管理员', 'administrators'],
    ['电脑节点', 'computer-nodes'],
    ['模型供应商', 'model-providers'],
    ['模型策略', 'model-policies'],
    ['用户', 'users'],
  ] as const) {
    const link = page.getByRole('link', { name: title, exact: true })
    await link.click()
    await expect(page).toHaveURL(new RegExp(`/admin/${path}$`))
    await expect(link).toHaveAttribute('aria-current', 'page')
    await page.reload()
    await expect(page.getByRole('heading', { name: title, exact: true })).toBeVisible()
    await expect(page).toHaveURL(new RegExp(`/admin/${path}$`))
  }
  await page.goBack()
  await expect(page).toHaveURL(/\/admin\/model-policies$/)
  await expect(page.getByRole('heading', { name: '模型策略', exact: true })).toBeVisible()
  await page.goForward()
  await expect(page).toHaveURL(/\/admin\/users$/)
  await expect(page.getByRole('heading', { name: '用户', exact: true })).toBeVisible()
})

test('详情链接可直接打开及刷新，不依赖列表第一页，失败可重试', async ({ page, context }) => {
  const fixture = await mockDirectory(context)
  const providerPath = `/admin/model-providers/${provider.id}`
  await page.goto(providerPath)
  await expect(page.getByRole('heading', { name: provider.name, exact: true })).toBeVisible()
  await page.reload()
  await expect(page.getByRole('heading', { name: provider.name, exact: true })).toBeVisible()
  await expect(page).toHaveURL(new RegExp(`${providerPath}$`))
  await expect(page.getByRole('link', { name: '模型供应商', exact: true })).toHaveAttribute(
    'aria-current',
    'page',
  )
  expect(fixture.reads).not.toContain('/model-providers')
  const other = await context.newPage()
  await other.goto(`/admin/model-policies/${policy.id}/versions`)
  await expect(other.getByRole('heading', { name: policy.name, exact: true })).toBeVisible()
  await other.reload()
  await expect(other.getByRole('heading', { name: policy.name, exact: true })).toBeVisible()
  expect(fixture.reads).not.toContain('/model-policies')
  fixture.setUnavailable(true)
  await page.reload()
  await expect(page.getByRole('alert')).toContainText('暂时无法读取配置')
  await expect(page.getByRole('heading', { name: '暂无模型', exact: true })).toHaveCount(0)
  fixture.setUnavailable(false)
  await page.getByRole('button', { name: '重新读取' }).click()
  await expect(page.getByRole('heading', { name: provider.name, exact: true })).toBeVisible()
  await page.goto(`/admin/model-providers/${id(9)}`)
  await expect(page.getByRole('alert')).toContainText('配置不存在')
  await expect(page.getByRole('heading', { name: '暂无模型', exact: true })).toHaveCount(0)
})

test('详情链接登录后恢复原地址，未登录不读取业务数据', async ({ page, context }) => {
  const fixture = await mockDirectory(context, false)
  const path = `/admin/model-policies/${policy.id}/versions`
  await page.goto(path)
  await expect(page.getByRole('heading', { name: 'TomBot 管理平台' })).toBeVisible()
  expect(fixture.reads).toEqual([])
  await page.getByLabel('管理员用户名').fill('test-admin')
  await page.getByLabel('密码', { exact: true }).fill(randomUUID())
  await page
    .getByLabel('动态验证码')
    .fill(String(Math.floor(Math.random() * 1000000)).padStart(6, '0'))
  await page.getByRole('button', { name: '登录管理平台' }).click()
  await expect(page.getByRole('heading', { name: policy.name, exact: true })).toBeVisible()
  await expect(page).toHaveURL(new RegExp(`${path}$`))
})
