import { expect, test } from '@playwright/test'
import { chooseOption } from './select'
import { session } from '../fixtures'
import type { Version } from '../../src/api/schemas'

const id = (value: number) => String(value).padStart(26, '0')

test('多候选路由继承参数、启停及旧版本切换', async ({ page }, testInfo) => {
  const problems: string[] = []
  page.on('pageerror', (error) => problems.push(error.message))
  const provider = {
    id: id(4),
    name: '团队供应商',
    protocol: 'responses',
    base_url: 'https://models.example.test',
    is_enabled: true,
    revision: 1,
    created_at: new Date().toISOString(),
  }
  const models = [
    {
      id: id(5),
      provider_id: provider.id,
      model_key: 'fast',
      display_name: '快速模型',
      context_limit: 131072,
      output_limit: 8192,
      supports_tools: true,
      supports_vision: false,
      reasoning_effort: 'high',
      metadata: { effort: { supported_levels: ['low', 'high'], default_level: 'high' } },
      is_enabled: true,
      revision: 1,
    },
    {
      id: id(7),
      provider_id: provider.id,
      model_key: 'vision',
      display_name: '视觉模型',
      context_limit: 262144,
      output_limit: 16384,
      supports_tools: true,
      supports_vision: true,
      metadata: {},
      is_enabled: true,
      revision: 1,
    },
  ]
  const candidate = (index: number) => ({
    position: index + 1,
    provider_id: provider.id,
    model_id: models[index]!.id,
    credential_id: null,
    priority: index + 1,
    weight: 1,
    configuration: {
      ...models[index]!,
      protocol: provider.protocol,
      base_url: provider.base_url,
      prices: {},
    },
  })
  const versions: Version[] = [1, 2].map((version) => ({
    policy_id: id(6),
    version,
    routing_mode: 'priority',
    candidates: [candidate(0)],
    is_enabled: version === 2,
    verified_at: new Date().toISOString(),
  }))
  const policy = {
    id: id(6),
    name: '团队模型路由',
    is_enabled: false,
    is_default: true,
    revision: 5,
    current_version: 2 as number | null,
  }
  let saved = 0,
    enabled = 0,
    disabled = 0
  await page.route('**/api/v2/admin/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace('/api/v2/admin', '')
    const method = route.request().method()
    let body: unknown
    if (path === '/auth/session') body = session()
    else if (path === '/users') body = { users: [] }
    else if (path === `/model-policies/${policy.id}`) body = { policy }
    else if (path === '/model-policies') body = { policies: [policy] }
    else if (path === `/model-policies/${policy.id}/state`) {
      expect(route.request().postDataJSON()).toEqual({
        expected_revision: policy.revision,
        is_enabled: true,
      })
      policy.is_enabled = true
      policy.revision++
      await route.fulfill({ status: 204 })
      return
    } else if (path === `/model-providers/${provider.id}`) body = { provider }
    else if (path === '/model-providers') body = { providers: [provider] }
    else if (path === `/model-providers/${provider.id}/models`) body = { models }
    else if (path === `/model-providers/${provider.id}/credentials`) body = { credentials: [] }
    else if (path === `/model-policies/${policy.id}/versions` && method === 'GET')
      body = { versions: [...versions].reverse() }
    else if (path === `/model-policies/${policy.id}/versions` && method === 'POST') {
      expect(route.request().postDataJSON()).toEqual({
        routing_mode: 'weighted',
        candidates: [
          { model_id: models[0]!.id, priority: 1, weight: 8, reasoning_effort: 'low' },
          { model_id: models[1]!.id, priority: 2, weight: 2 },
        ],
      })
      saved++
      policy.revision++
      versions.push({
        policy_id: policy.id,
        version: 3,
        routing_mode: 'weighted',
        candidates: [
          { ...candidate(0), weight: 8 },
          { ...candidate(1), weight: 2 },
        ],
        is_enabled: false,
        verified_at: null,
      })
      body = { version: versions.at(-1) }
    } else if (path.match(/\/versions\/\d+\/(enable|disable)$/)) {
      expect(route.request().postDataJSON()).toEqual({ expected_revision: policy.revision })
      const number = Number(path.split('/').at(-2))
      const version = versions.find((value) => value.version === number)!
      if (path.endsWith('/enable')) {
        versions.forEach((value) => {
          value.is_enabled = value === version
        })
        version.verified_at ??= new Date().toISOString()
        policy.current_version = number
        enabled++
      } else {
        version.is_enabled = false
        policy.current_version = null
        disabled++
      }
      policy.revision++
      body = { version }
    } else {
      await route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '未配置测试接口' } })
      return
    }
    await route.fulfill({ json: body })
  })
  await page.goto('/admin/')
  await page.getByRole('link', { name: '模型策略', exact: true }).click()
  await page.getByRole('link', { name: '版本管理' }).click()
  await expect(
    page.getByRole('row').filter({ has: page.getByText('v2', { exact: true }) }),
  ).toContainText('已启用')
  await expect(page.getByText('策略已停用', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '启用', exact: true })).toBeDisabled()
  await page.screenshot({ path: testInfo.outputPath('policy-disabled.png'), fullPage: true })
  await page.getByRole('button', { name: '启用策略', exact: true }).click()
  await page.getByRole('button', { name: '确认启用策略' }).click()
  await expect(page.getByRole('button', { name: '启用', exact: true })).toBeEnabled()
  await expect(page.getByText('策略已停用', { exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: '创建策略版本' }).click()
  await chooseOption(page, '候选供应商', provider.name)
  await expect(page.getByRole('combobox', { name: '候选模型', exact: true })).toBeVisible()
  await expect(page.getByRole('dialog').getByText(/本页\s*\d+\s*条/)).toHaveCount(0)
  await expect(page.getByRole('dialog').locator('.pagination')).toHaveCount(0)
  const picker = page.locator('.policy-add-model')
  const modelSelect = page.getByRole('combobox', { name: '候选模型', exact: true })
  const addButton = picker.getByRole('button', { name: '添加', exact: true })
  for (const width of [1280, 390, 320]) {
    await page.setViewportSize({ width, height: 900 })
    await modelSelect.scrollIntoViewIfNeeded()
    await expect(addButton).toBeDisabled()
    await expect(page.getByRole('combobox', { name: '候选思考级别' })).toBeDisabled()
    await expect(modelSelect).toContainText('选择模型')
    const selectBounds = (await modelSelect.boundingBox())!
    const addBounds = (await addButton.boundingBox())!
    const pickerBounds = (await picker.boundingBox())!
    expect(selectBounds.width).toBeGreaterThan(150)
    expect(addBounds.width).toBeLessThan(120)
    if (width > 520) {
      expect(addBounds.x - selectBounds.x - selectBounds.width).toBeGreaterThanOrEqual(11)
      expect(Math.abs(addBounds.y - selectBounds.y)).toBeLessThan(2)
    } else {
      expect(addBounds.y - selectBounds.y - selectBounds.height).toBeGreaterThanOrEqual(11)
      expect(selectBounds.width).toBeCloseTo(pickerBounds.width, 0)
    }
    expect(await picker.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
      true,
    )
    await chooseOption(page, '候选模型', models[0]!.display_name)
    await expect(modelSelect).toContainText(models[0]!.display_name)
    await expect(addButton).toBeEnabled()
    await chooseOption(page, '候选思考级别', 'low')
    await expect(page.getByRole('combobox', { name: '候选思考级别' })).toContainText('low')
    await page.screenshot({
      path: testInfo.outputPath(`policy-picker-${width}.png`),
      fullPage: true,
    })
    await chooseOption(page, '候选模型', '选择模型')
  }
  await page.setViewportSize({ width: 1280, height: 900 })
  for (const model of models) {
    await chooseOption(page, '候选模型', model.display_name)
    if (model.id === models[0]!.id) await chooseOption(page, '候选思考级别', 'low')
    await page.getByRole('button', { name: '添加', exact: true }).click()
  }
  await chooseOption(page, '路由方式', '加权分配')
  await page.getByLabel('快速模型 分配权重').fill('8')
  await page.getByLabel('视觉模型 分配权重').fill('2')
  await expect(page.getByLabel('快速模型 输出上限')).toBeHidden()
  await page.locator('.policy-editor-body').evaluate((element) => {
    element.scrollTop = 0
  })
  await expect(page.getByRole('button', { name: '保存版本' })).toBeInViewport()
  await page.screenshot({ path: testInfo.outputPath('policy-editor-desktop.png'), fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await expect(page.getByRole('heading', { name: '创建策略版本' })).toBeInViewport()
  await expect(page.getByRole('button', { name: '保存版本' })).toBeInViewport()
  await page.screenshot({ path: testInfo.outputPath('policy-editor-mobile.png'), fullPage: true })
  await page.getByRole('button', { name: '保存版本' }).click()
  await expect.poll(() => saved).toBe(1)
  const versionRow = (number: number) =>
    page.getByRole('row').filter({ has: page.getByText(`v${number}`, { exact: true }) })
  await expect(versionRow(3)).toContainText('已停用')
  await versionRow(3).getByRole('button', { name: '启用', exact: true }).click()
  await page.getByRole('button', { name: '确认启用版本 v3' }).click()
  await expect(versionRow(3)).toContainText('已启用')
  await expect(versionRow(2)).toContainText('已停用')
  await expect.poll(() => enabled).toBe(1)
  await versionRow(1).getByRole('button', { name: '启用', exact: true }).click()
  await page.getByRole('button', { name: '确认启用版本 v1' }).click()
  await expect.poll(() => enabled).toBe(2)
  await expect(versionRow(1)).toContainText('已启用')
  await expect(page.getByText('已启用', { exact: true })).toHaveCount(1)
  await versionRow(1).getByRole('button', { name: '停用', exact: true }).click()
  await page.getByRole('button', { name: '确认停用版本 v1' }).click()
  await expect.poll(() => disabled).toBe(1)
  await expect(page.getByText('已启用', { exact: true })).toHaveCount(0)
  await expect(page.getByText('已停用', { exact: true })).toHaveCount(3)
  await versionRow(1).getByRole('button', { name: '启用', exact: true }).click()
  await page.getByRole('button', { name: '确认启用版本 v1' }).click()
  await expect.poll(() => enabled).toBe(3)
  await expect(page.getByText('已启用', { exact: true })).toHaveCount(1)
  await expect(page.getByText(/激活|撤销|草稿|已发布/)).toHaveCount(0)
  await expect(page.locator('select')).toHaveCount(0)
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.screenshot({
    path: testInfo.outputPath('policy-versions-desktop.png'),
    fullPage: true,
  })
  expect(problems).toEqual([])
})
