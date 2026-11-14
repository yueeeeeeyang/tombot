import { chooseOption } from './select'
import { expect, test } from '@playwright/test'
import { randomUUID } from 'node:crypto'

const id = (value: number) => value.toString().padStart(26, '0')

// 浏览器替身验证真实页面交互；数据库编辑约束与供应商 SSE 由后端联合测试覆盖。
test('停用供应商编辑、模型测试结果与窄屏展示', async ({ page }, testInfo) => {
  const problems: string[] = []
  page.on('pageerror', (error) => problems.push(error.message))
  const provider = {
    id: id(4),
    name: '测试供应商',
    protocol: 'anthropic',
    base_url: 'https://models.example.test/anthropic',
    is_enabled: false,
    revision: 2,
    created_at: new Date().toISOString(),
  }
  const model = {
    id: id(5),
    provider_id: provider.id,
    model_key: 'team-model',
    display_name: '团队模型',
    context_limit: 8192,
    output_limit: 1024,
    supports_tools: true,
    supports_vision: false,
    metadata: {},
    is_enabled: false,
    revision: 1,
  }
  let edits = 0
  let probes = 0
  let modelEdits = 0
  let deleted = false
  let deletionAttempts = 0
  await page.route('**/api/v2/admin/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname.replace('/api/v2/admin', '')
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
    else if (path === '/users') body = { users: [] }
    else if (path === `/model-providers/${provider.id}`) body = { provider }
    else if (path === '/model-providers') body = { providers: [provider] }
    else if (path === `/model-providers/${provider.id}/edit`) {
      expect(request.method()).toBe('POST')
      expect(request.headers()['x-csrf-token']).toBeTruthy()
      expect(request.postDataJSON()).toEqual({
        expected_revision: 2,
        name: '更新后的供应商',
        protocol: 'responses',
        base_url: 'https://models.example.test',
        credential: '',
      })
      edits++
      Object.assign(provider, {
        name: '更新后的供应商',
        protocol: 'responses',
        base_url: 'https://models.example.test',
        revision: 3,
      })
      body = { provider }
    } else if (path === `/model-providers/${provider.id}/models`)
      body = { models: deleted ? [] : [model] }
    else if (path === `/models/${model.id}/edit`) {
      const submitted = request.postDataJSON()
      expect(submitted.expected_revision).toBe(model.revision)
      expect(submitted).not.toHaveProperty('provider_id')
      expect(submitted).not.toHaveProperty('is_enabled')
      modelEdits++
      Object.assign(model, submitted, { revision: model.revision + 1 })
      body = { model }
    } else if (path === `/models/${model.id}/state`) {
      expect(request.postDataJSON().expected_revision).toBe(model.revision)
      model.is_enabled = request.postDataJSON().is_enabled
      model.revision++
      await route.fulfill({ status: 204 })
      return
    } else if (path === `/models/${model.id}/delete`) {
      expect(request.postDataJSON()).toEqual({ expected_revision: model.revision })
      deletionAttempts++
      if (deletionAttempts === 1) {
        await route.fulfill({
          status: 409,
          json: {
            code: 'MODEL_REFERENCED',
            message: '模型仍被策略版本引用，不能删除；可以保持停用',
          },
        })
      } else {
        deleted = true
        await route.fulfill({ status: 204 })
      }
      return
    } else if (path === `/models/${model.id}/test`) {
      expect(request.method()).toBe('POST')
      expect(request.headers()['x-csrf-token']).toBeTruthy()
      expect(request.postDataJSON()).toEqual({
        expected_provider_revision: 3,
        expected_model_revision: 1,
      })
      probes++
      body = {
        test: {
          model_id: model.id,
          provider_revision: 3,
          model_revision: 1,
          tested_at: new Date().toISOString(),
          connectivity: { status: 'succeeded', message: '连通性测试通过', duration_ms: 120 },
          tools: {
            status: probes === 1 ? 'succeeded' : 'failed',
            message:
              probes === 1
                ? '工具调用测试通过'
                : '工具调用测试失败，请检查供应商配置、凭据和模型能力',
            duration_ms: 240,
          },
        },
      }
    } else {
      await route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '测试未配置' } })
      return
    }
    await route.fulfill({ status: 200, json: body })
  })
  await page.goto('/admin/')
  await page.getByRole('link', { name: '模型供应商' }).click()
  await page.getByRole('button', { name: '编辑', exact: true }).click()
  await expect(page.getByLabel('名称')).toHaveValue('测试供应商')
  await page.getByLabel('名称').fill('更新后的供应商')
  await chooseOption(page, '协议', 'Responses 协议')
  await page.getByLabel('服务地址').fill('https://models.example.test')
  await page.getByRole('button', { name: '确认编辑供应商' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await expect(page.getByText('更新后的供应商')).toBeVisible()
  await expect(page.getByRole('button', { name: '编辑', exact: true })).toBeVisible()
  await page.getByRole('link', { name: '模型管理' }).click()
  await page.getByRole('button', { name: '测试模型' }).click()
  await expect(page.getByRole('button', { name: '关闭弹窗' })).toBeFocused()
  await page.keyboard.press('Tab')
  await expect(page.getByRole('button', { name: '取消', exact: true })).toBeFocused()
  await page.keyboard.press('Tab')
  await expect(page.getByRole('button', { name: '开始测试' })).toBeFocused()
  expect(probes).toBe(0)
  await page.getByRole('button', { name: '开始测试' }).click()
  await expect(page.getByRole('status')).toContainText('连通性：通过')
  await expect(page.getByRole('status')).toContainText('工具调用：通过')
  await expect(page.getByRole('button', { name: '开始测试' })).toBeDisabled()
  await page.screenshot({
    path: testInfo.outputPath('model-test-desktop.png'),
    fullPage: true,
    animations: 'disabled',
  })
  await page.getByRole('button', { name: '关闭', exact: true }).last().click()
  await page.getByRole('button', { name: '测试模型' }).click()
  await page.getByRole('button', { name: '开始测试' }).click()
  await expect(page.getByRole('status')).toContainText('工具调用：失败')
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true,
  )
  await page.screenshot({
    path: testInfo.outputPath('model-test-mobile.png'),
    fullPage: true,
    animations: 'disabled',
  })
  expect(edits).toBe(1)
  expect(probes).toBe(2)
  await page.getByRole('button', { name: '关闭', exact: true }).last().click()
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.getByRole('button', { name: '编辑', exact: true }).click()
  await expect(page.getByLabel('显示名称')).toHaveValue('团队模型')
  await page.getByLabel('显示名称').fill('修改后的模型')
  await page.getByLabel('上下文窗口').fill('32768')
  await page.getByLabel('输出上限').fill('2048')
  await page.getByLabel('支持的思考级别').fill('low, high')
  await chooseOption(page, '使用的思考级别', 'high')
  await page.screenshot({ path: testInfo.outputPath('model-edit-desktop.png'), fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: testInfo.outputPath('model-edit-mobile.png'), fullPage: true })
  await page.getByRole('button', { name: '保存修改' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await expect(page.getByText('修改后的模型')).toBeVisible()
  await page.getByRole('button', { name: '启用', exact: true }).click()
  await page.getByRole('button', { name: '确认启用模型' }).click()
  await expect(page.getByRole('button', { name: '停用', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '编辑', exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: '删除', exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: '停用', exact: true }).click()
  await page.getByRole('button', { name: '确认停用模型' }).click()
  await page.getByRole('button', { name: '删除', exact: true }).click()
  await page.getByRole('button', { name: '确认删除模型' }).click()
  await expect(page.getByRole('alert')).toContainText('仍被策略版本引用')
  await expect(page.getByRole('button', { name: '确认删除模型' })).toBeDisabled()
  await page.getByRole('button', { name: '关闭并重新读取' }).click()
  await page.getByRole('button', { name: '删除', exact: true }).click()
  await page.getByRole('button', { name: '确认删除模型' }).click()
  await expect(page.getByRole('heading', { name: '暂无模型', exact: true })).toBeVisible()
  await expect(page.getByRole('table')).toHaveCount(0)
  await expect(page.locator('.pagination')).toHaveCount(0)
  await expect(page.locator('.empty-state')).toHaveCSS('border-top-style', 'dashed')
  expect(modelEdits).toBe(1)
  expect(deletionAttempts).toBe(2)
  expect(problems).toEqual([])
})

test('供应商模型规格独立预填并完整登记', async ({ page }, testInfo) => {
  const problems: string[] = []
  page.on('pageerror', (error) => problems.push(error.message))
  const provider = {
    id: id(4),
    name: '元数据供应商',
    protocol: 'responses',
    base_url: 'https://models.example.test',
    is_enabled: true,
    revision: 1,
    created_at: new Date().toISOString(),
  }
  const offered = [
    {
      model_key: 'vision-model',
      display_name: '多模态模型',
      context_limit: 1048576,
      output_limit: 393216,
      supports_vision: true,
      metadata: {
        input_modalities: ['text', 'image'],
        output_modalities: ['text'],
        effort: { supported_levels: ['low', 'high', 'max'], default_level: 'high' },
      },
    },
    {
      model_key: 'text-model',
      display_name: '文本模型',
      context_limit: 32768,
      output_limit: 4096,
      supports_tools: false,
      supports_vision: false,
      metadata: {
        input_modalities: ['text'],
        output_modalities: ['text'],
        effort: { supported_levels: ['low', 'high'], default_level: 'low' },
      },
    },
  ]
  const imported: Record<string, unknown>[] = []
  let discoveries = 0
  await page.route('**/api/v2/admin/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname.replace('/api/v2/admin', '')
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
    else if (path === '/users') body = { users: [] }
    else if (path === `/model-providers/${provider.id}`) body = { provider }
    else if (path === '/model-providers') body = { providers: [provider] }
    else if (path === `/model-providers/${provider.id}/remote-models`) {
      discoveries++
      body = { models: offered }
    } else if (path === `/model-providers/${provider.id}/models`) {
      if (request.method() === 'POST') {
        expect(request.headers()['x-csrf-token']).toBeTruthy()
        const model = {
          ...request.postDataJSON(),
          id: id(imported.length + 5),
          provider_id: provider.id,
          revision: 1,
          is_enabled: true,
        }
        imported.push(model)
        body = { model }
      } else body = { models: imported }
    } else {
      await route.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: '测试未配置' } })
      return
    }
    await route.fulfill({ status: request.method() === 'POST' ? 201 : 200, json: body })
  })
  await page.goto('/admin/')
  await page.getByRole('link', { name: '模型供应商', exact: true }).click()
  await page.getByRole('link', { name: '模型管理', exact: true }).click()
  await page.getByRole('button', { name: '从供应商拉取', exact: true }).click()
  await expect(page.getByRole('button', { name: '展开 多模态模型 详细信息' })).toHaveAttribute(
    'aria-expanded',
    'false',
  )
  await expect(page.getByLabel('多模态模型 · 上下文窗口')).toBeHidden()
  await page.screenshot({
    path: testInfo.outputPath('model-import-collapsed.png'),
    fullPage: true,
    animations: 'disabled',
  })
  await page.getByRole('button', { name: '展开 多模态模型 详细信息' }).click()
  await page.getByRole('button', { name: '展开 文本模型 详细信息' }).click()
  await expect(page.getByLabel('多模态模型 · 上下文窗口')).toHaveValue('1048576')
  await expect(page.getByLabel('多模态模型 · 输出上限')).toHaveValue('393216')
  await expect(page.getByRole('combobox', { name: '多模态模型 · 图像输入' })).toHaveText('支持')
  await expect(page.getByRole('combobox', { name: '多模态模型 · 工具调用' })).toHaveText('请选择')
  await expect(page.getByRole('combobox', { name: '多模态模型 · 思考级别' })).toHaveText(
    'high（默认）',
  )
  await expect(page.getByLabel('文本模型 · 上下文窗口')).toHaveValue('32768')
  await expect(page.getByLabel('文本模型 · 输出上限')).toHaveValue('4096')
  await expect(page.getByRole('combobox', { name: '文本模型 · 图像输入' })).toHaveText('不支持')
  await expect(page.getByRole('combobox', { name: '文本模型 · 工具调用' })).toHaveText('不支持')
  await expect(page.getByRole('combobox', { name: '文本模型 · 思考级别' })).toHaveText(
    'low（默认）',
  )
  await expect(page.getByText('文本、图像', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: '收起 多模态模型 详细信息' }).click()
  await page.getByRole('button', { name: '收起 文本模型 详细信息' }).click()
  await page.getByRole('button', { name: '登记所选（2）' }).click()
  expect(imported).toHaveLength(0)
  await expect(page.getByRole('button', { name: '收起 多模态模型 详细信息' })).toHaveAttribute(
    'aria-expanded',
    'true',
  )
  await expect(page.getByRole('button', { name: '展开 文本模型 详细信息' })).toHaveAttribute(
    'aria-expanded',
    'false',
  )
  await expect(page.getByRole('combobox', { name: '多模态模型 · 工具调用' })).toBeFocused()
  await chooseOption(page, '多模态模型 · 工具调用', '支持')
  await chooseOption(page, '多模态模型 · 思考级别', 'max')
  await page.getByRole('combobox', { name: '多模态模型 · 思考级别' }).click()
  await expect(page.getByRole('listbox')).toBeVisible()
  await page.screenshot({
    path: testInfo.outputPath('model-import-desktop.png'),
    fullPage: true,
    animations: 'disabled',
  })
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog')).toBeVisible()
  await expect(page.getByRole('listbox')).toHaveCount(0)
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true,
  )
  await page.getByRole('combobox', { name: '多模态模型 · 思考级别' }).click()
  const bounds = await page.getByRole('listbox').boundingBox()
  expect(bounds).not.toBeNull()
  expect(bounds!.x).toBeGreaterThanOrEqual(0)
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(390)
  expect(bounds!.y).toBeGreaterThanOrEqual(0)
  expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(844)
  await page.screenshot({
    path: testInfo.outputPath('model-import-mobile.png'),
    fullPage: true,
    animations: 'disabled',
  })
  await page.keyboard.press('Escape')
  await page.emulateMedia({ colorScheme: 'dark' })
  await page.getByRole('combobox', { name: '多模态模型 · 思考级别' }).click()
  await page.screenshot({
    path: testInfo.outputPath('model-import-dark.png'),
    fullPage: true,
    animations: 'disabled',
  })
  await page.keyboard.press('Escape')
  expect(discoveries).toBe(1)
  await expect(page.locator('select')).toHaveCount(0)
  await page.getByRole('button', { name: '登记所选（2）' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(imported).toHaveLength(2)
  expect(imported[0]).toMatchObject({
    ...offered[0],
    supports_tools: true,
    reasoning_effort: 'max',
  })
  expect(imported[1]).toMatchObject({ ...offered[1], reasoning_effort: 'low' })
  await expect(page.getByRole('button', { name: '从供应商拉取', exact: true })).toBeFocused()
  await expect(page.getByText('思考级别：max')).toBeVisible()
  await expect(page.getByText('思考级别：low')).toBeVisible()
  expect(problems).toEqual([])
})
