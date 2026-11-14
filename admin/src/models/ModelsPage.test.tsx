import { chooseOption } from '../../tests/select'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { App } from '../App'
import { failure, id, json, makeClient, provider } from '../../tests/fixtures'

describe('供应商模型目录', () => {
  it('部分登记失败保留成功记录并要求重新读取，不重复提交', async () => {
    const registered: unknown[] = []
    let writes = 0
    const fixture = makeClient((path, options) => {
      if (path.endsWith('/remote-models'))
        return json({
          models: [
            {
              model_key: 'model-a',
              display_name: '模型甲',
              context_limit: 65536,
              output_limit: 4096,
              supports_tools: true,
              supports_vision: true,
              metadata: {
                input_modalities: ['text', 'image'],
                output_modalities: ['text'],
                effort: { supported_levels: ['low', 'high', 'max'], default_level: 'high' },
              },
            },
            {
              model_key: 'model-b',
              display_name: '模型乙',
              context_limit: 32768,
              output_limit: 2048,
              supports_tools: false,
              supports_vision: false,
              metadata: {
                input_modalities: ['text'],
                output_modalities: ['text'],
                effort: { supported_levels: ['low', 'high'], default_level: 'low' },
              },
            },
          ],
        })
      if (path.includes('/models')) {
        if (options.method === 'POST') {
          writes++
          if (writes === 2) return failure('MODEL_MANAGEMENT_UNAVAILABLE', '登记暂不可用', 503)
          const model = {
            ...JSON.parse(String(options.body)),
            id: id(5),
            provider_id: provider.id,
            is_enabled: true,
            revision: 1,
          }
          registered.push(model)
          return json({ model }, 201)
        }
        return json({ models: registered })
      }
      if (path === `/model-providers/${provider.id}`) return json({ provider: provider })
      if (path.startsWith('/model-providers')) return json({ providers: [provider] })
      return json({ users: [] })
    })
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('link', { name: '模型供应商' }))
    await user.click(await screen.findByRole('link', { name: '模型管理' }))
    await user.click(screen.getByRole('button', { name: '从供应商拉取' }))
    await user.click(await screen.findByRole('button', { name: '展开 模型甲 详细信息' }))
    await user.click(screen.getByRole('button', { name: '展开 模型乙 详细信息' }))
    expect(screen.getByLabelText('模型甲 · 上下文窗口')).toHaveValue(65536)
    expect(screen.getByLabelText('模型乙 · 上下文窗口')).toHaveValue(32768)
    expect(screen.getByLabelText('模型甲 · 图像输入', { selector: 'button' })).toHaveTextContent(
      '支持',
    )
    expect(screen.getByLabelText('模型乙 · 图像输入', { selector: 'button' })).toHaveTextContent(
      '不支持',
    )
    expect(screen.getByLabelText('模型甲 · 思考级别', { selector: 'button' })).toHaveTextContent(
      'high',
    )
    expect(screen.getByLabelText('模型乙 · 思考级别', { selector: 'button' })).toHaveTextContent(
      'low',
    )
    await user.click(await screen.findByRole('button', { name: '登记所选（2）' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('登记暂不可用')
    expect(screen.getByText('已登记 1 个，其余未完成')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '登记所选（2）' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '关闭并重新读取' }))
    expect(await screen.findByText('模型甲')).toBeInTheDocument()
    expect(screen.queryByText('模型乙')).not.toBeInTheDocument()
    expect(writes).toBe(2)
  })

  it('Responses 供应商拉取后按单模型契约登记，不发送路径归属字段', async () => {
    const registered: unknown[] = []
    const fixture = makeClient((path, options) => {
      if (path.endsWith('/remote-models'))
        return json({
          models: [
            {
              model_key: 'model-a',
              display_name: '模型甲',
              context_limit: 65536,
              output_limit: 4096,
              supports_tools: true,
              supports_vision: true,
              metadata: {
                input_modalities: ['text', 'image'],
                output_modalities: ['text'],
                effort: { supported_levels: ['low', 'high', 'max'], default_level: 'high' },
              },
            },
          ],
        })
      if (path.includes('/models')) {
        if (options.method === 'POST') {
          const body = JSON.parse(String(options.body))
          expect(body).toEqual({
            model_key: 'model-a',
            display_name: '模型甲',
            context_limit: 65536,
            output_limit: 4096,
            supports_tools: true,
            supports_vision: true,
            reasoning_effort: 'high',
            metadata: {
              input_modalities: ['text', 'image'],
              output_modalities: ['text'],
              effort: { supported_levels: ['low', 'high', 'max'], default_level: 'high' },
            },
          })
          const model = {
            ...body,
            id: id(5),
            provider_id: provider.id,
            is_enabled: true,
            revision: 1,
          }
          registered.push(model)
          return json({ model }, 201)
        }
        return json({ models: registered })
      }
      if (path === `/model-providers/${provider.id}`)
        return json({ provider: { ...provider, protocol: 'responses' } })
      if (path.startsWith('/model-providers'))
        return json({ providers: [{ ...provider, protocol: 'responses' }] })
      return json({ users: [] })
    })
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('link', { name: '模型供应商' }))
    expect(await screen.findByText('Responses')).toBeInTheDocument()
    await user.click(screen.getByRole('link', { name: '模型管理' }))
    await screen.findByRole('heading', { name: '暂无模型' })
    await user.click(screen.getByRole('button', { name: '从供应商拉取' }))
    await user.click(await screen.findByRole('button', { name: '登记所选（1）' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(await screen.findByText('模型甲')).toBeInTheDocument()
    expect(registered).toHaveLength(1)
  })

  it('展示真实读取失败和请求编号，供应商认证失败不会退出管理员会话', async () => {
    const fixture = makeClient((path) => {
      if (path.endsWith('/remote-models'))
        return failure('MODEL_DISCOVERY_AUTHENTICATION', '供应商拒绝了访问凭据', 422)
      if (path.includes('/models')) return json({ models: [] })
      if (path === `/model-providers/${provider.id}`) return json({ provider: provider })
      if (path.startsWith('/model-providers')) return json({ providers: [provider] })
      return json({ users: [] })
    })
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('link', { name: '模型供应商' }))
    await user.click(await screen.findByRole('link', { name: '模型管理' }))
    await user.click(screen.getByRole('button', { name: '从供应商拉取' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('供应商拒绝了访问凭据')
    expect(screen.getByRole('alert')).toHaveTextContent(id(9))
    expect(screen.queryByRole('button', { name: '登录管理平台' })).not.toBeInTheDocument()
  })

  it('停用供应商编辑保留身份，预填信息并携带修订号', async () => {
    let writes = 0
    const disabled = { ...provider, is_enabled: false }
    const fixture = makeClient((path, options) => {
      if (path.endsWith('/edit')) {
        writes++
        expect(JSON.parse(String(options.body))).toEqual({
          expected_revision: 7,
          name: '修改后名称',
          protocol: 'responses',
          base_url: provider.base_url,
          credential: '',
        })
        return json({
          provider: { ...disabled, name: '修改后名称', protocol: 'responses', revision: 8 },
        })
      }
      if (path === `/model-providers/${provider.id}`) return json({ provider: disabled })
      if (path.startsWith('/model-providers')) return json({ providers: [disabled] })
      return json({ users: [] })
    })
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('link', { name: '模型供应商' }))
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    expect(screen.getByLabelText('名称')).toHaveValue(provider.name)
    expect(screen.getByLabelText('服务地址')).toHaveValue(provider.base_url)
    expect(screen.getByLabelText('新供应商凭据')).toHaveValue('')
    await user.clear(screen.getByLabelText('名称'))
    await user.type(screen.getByLabelText('名称'), '修改后名称')
    await chooseOption(user, '协议', 'Responses 协议')
    await user.click(screen.getByRole('button', { name: '确认编辑供应商' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(writes).toBe(1)
  })

  it('启用供应商没有编辑入口', async () => {
    const fixture = makeClient((path) =>
      path.startsWith('/model-providers') ? json({ providers: [provider] }) : json({ users: [] }),
    )
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('link', { name: '模型供应商' }))
    await screen.findByText(provider.name)
    expect(screen.queryByRole('button', { name: '编辑' })).not.toBeInTheDocument()
  })

  it('编辑失败清空输入凭据，禁止自动或重复提交', async () => {
    let writes = 0
    const fixture = makeClient((path) => {
      if (path.endsWith('/edit')) {
        writes++
        return failure('MODEL_CONFIGURATION_CONFLICT', '配置已变更', 409)
      }
      return path.startsWith('/model-providers')
        ? json({ providers: [{ ...provider, is_enabled: false }] })
        : json({ users: [] })
    })
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('link', { name: '模型供应商' }))
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.type(screen.getByLabelText('新供应商凭据'), crypto.randomUUID())
    await user.click(screen.getByRole('button', { name: '确认编辑供应商' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('配置已变更')
    expect(screen.getByLabelText('新供应商凭据')).toHaveValue('')
    expect(screen.getByRole('button', { name: '确认编辑供应商' })).toBeDisabled()
    expect(writes).toBe(1)
  })

  it.each(['succeeded', 'failed', 'skipped', 'request_failed'])(
    '模型目录显示独立测试结果：%s',
    async (status) => {
      const model = {
        id: id(5),
        provider_id: provider.id,
        model_key: 'test-model',
        display_name: '目录模型',
        context_limit: 8192,
        output_limit: 1024,
        supports_tools: false,
        supports_vision: false,
        metadata: {},
        is_enabled: false,
        revision: 3,
      }
      let writes = 0
      const fixture = makeClient((path, options) => {
        if (path.endsWith('/test')) {
          writes++
          expect(JSON.parse(String(options.body))).toEqual({
            expected_provider_revision: 7,
            expected_model_revision: 3,
          })
          if (status === 'request_failed')
            return failure('MODEL_CONFIGURATION_CONFLICT', '测试期间配置已变更', 409)
          return json({
            test: {
              model_id: model.id,
              provider_revision: 7,
              model_revision: 3,
              tested_at: new Date().toISOString(),
              connectivity: {
                status: status === 'skipped' ? 'failed' : 'succeeded',
                message: '连通性结果',
                duration_ms: 18,
              },
              tools: { status, message: '工具调用结果', duration_ms: 22 },
            },
          })
        }
        if (path.includes('/models')) return json({ models: [model] })
        if (path === `/model-providers/${provider.id}`)
          return json({ provider: { ...provider, is_enabled: false } })
        if (path.startsWith('/model-providers'))
          return json({ providers: [{ ...provider, is_enabled: false }] })
        return json({ users: [] })
      })
      const user = userEvent.setup()
      render(<App client={fixture.client} />)
      await user.click(await screen.findByRole('link', { name: '模型供应商' }))
      await user.click(await screen.findByRole('link', { name: '模型管理' }))
      await user.click(await screen.findByRole('button', { name: '测试模型' }))
      expect(screen.getByText(/可能产生供应商费用/)).toBeInTheDocument()
      expect(writes).toBe(0)
      await user.click(screen.getByRole('button', { name: '开始测试' }))
      if (status === 'request_failed')
        expect(await screen.findByRole('alert')).toHaveTextContent('测试期间配置已变更')
      else
        expect(await screen.findByRole('status')).toHaveTextContent(
          '工具调用：' + { succeeded: '通过', failed: '失败', skipped: '未执行' }[status],
        )
      expect(screen.getByRole('button', { name: '开始测试' })).toBeDisabled()
      expect(writes).toBe(1)
    },
  )
  it('未提供元数据时留空并要求确认，不套用默认规格', async () => {
    let writes = 0
    const registered: unknown[] = []
    const fixture = makeClient((path, options) => {
      if (path.endsWith('/remote-models'))
        return json({ models: [{ model_key: 'unknown', display_name: '未知模型', metadata: {} }] })
      if (path.includes('/models')) {
        if (options.method === 'POST') {
          writes++
          const body = JSON.parse(String(options.body))
          expect(body).toEqual({
            model_key: 'unknown',
            display_name: '未知模型',
            context_limit: 4096,
            output_limit: 1024,
            supports_tools: true,
            supports_vision: false,
            reasoning_effort: '',
            metadata: {},
          })
          const model = {
            ...body,
            id: id(5),
            provider_id: provider.id,
            is_enabled: true,
            revision: 1,
          }
          registered.push(model)
          return json({ model }, 201)
        }
        return json({ models: registered })
      }
      if (path === `/model-providers/${provider.id}`) return json({ provider: provider })
      if (path.startsWith('/model-providers')) return json({ providers: [provider] })
      return json({ users: [] })
    })
    const user = userEvent.setup()
    render(<App client={fixture.client} />)
    await user.click(await screen.findByRole('link', { name: '模型供应商' }))
    await user.click(await screen.findByRole('link', { name: '模型管理' }))
    await user.click(screen.getByRole('button', { name: '从供应商拉取' }))
    expect(await screen.findByLabelText('未知模型 · 上下文窗口')).toHaveValue(null)
    expect(screen.getByLabelText('未知模型 · 输出上限')).toHaveValue(null)
    expect(screen.getByLabelText('未知模型 · 工具调用', { selector: 'button' })).toHaveTextContent(
      '请选择',
    )
    expect(screen.getByLabelText('未知模型 · 图像输入', { selector: 'button' })).toHaveTextContent(
      '请选择',
    )
    expect(screen.getByLabelText('未知模型 · 思考级别', { selector: 'button' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '登记所选（1）' }))
    expect(writes).toBe(0)
    await user.type(screen.getByLabelText('未知模型 · 上下文窗口'), '4096')
    await user.type(screen.getByLabelText('未知模型 · 输出上限'), '8192')
    await chooseOption(user, '未知模型 · 工具调用', '支持')
    await chooseOption(user, '未知模型 · 图像输入', '不支持')
    await user.click(screen.getByRole('button', { name: '登记所选（1）' }))
    expect(writes).toBe(0)
    await user.clear(screen.getByLabelText('未知模型 · 输出上限'))
    await user.type(screen.getByLabelText('未知模型 · 输出上限'), '1024')
    await user.click(screen.getByRole('button', { name: '登记所选（1）' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(writes).toBe(1)
  })
})

it('一次点击直接请求，加载期间去重，离开供应商取消在途请求', async () => {
  let finish: ((value: Response) => void) | undefined
  let signal: AbortSignal | null | undefined
  const fixture = makeClient((path, options) => {
    if (path.endsWith('/remote-models')) {
      signal = options.signal
      return new Promise<Response>((resolve) => {
        finish = resolve
      })
    }
    if (path.includes('/models')) return json({ models: [] })
    if (path === `/model-providers/${provider.id}`) return json({ provider: provider })
    if (path.startsWith('/model-providers')) return json({ providers: [provider] })
    return json({ users: [] })
  })
  const user = userEvent.setup()
  render(<App client={fixture.client} />)
  await user.click(await screen.findByRole('link', { name: '模型供应商' }))
  await user.click(await screen.findByRole('link', { name: '模型管理' }))
  const trigger = screen.getByRole('button', { name: '从供应商拉取' })
  await user.click(trigger)
  expect(trigger).toBeDisabled()
  expect(trigger).toHaveTextContent('正在拉取')
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  await user.click(trigger)
  expect(fixture.calls.filter((call) => call.path.endsWith('/remote-models'))).toHaveLength(1)
  await user.click(screen.getByRole('link', { name: '全部供应商' }))
  expect(signal?.aborted).toBe(true)
  finish!(json({ models: [{ model_key: 'late', display_name: '过期响应', metadata: {} }] }))
  await screen.findByText(provider.name)
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
})

it('拉取失败可显式重试，空列表与已登记模型可以查看但不能重复提交', async () => {
  let reads = 0
  const model = {
    id: id(5),
    provider_id: provider.id,
    model_key: 'existing',
    display_name: '已登记模型',
    context_limit: 4096,
    output_limit: 1024,
    supports_tools: true,
    supports_vision: false,
    metadata: {},
    is_enabled: true,
    revision: 1,
  }
  const fixture = makeClient((path) => {
    if (path.endsWith('/remote-models')) {
      reads++
      if (reads === 1) return failure('MODEL_DISCOVERY_RATE_LIMITED', '供应商限流', 429)
      return json({ models: reads === 2 ? [] : [model] })
    }
    if (path.includes('/models')) return json({ models: [model] })
    if (path === `/model-providers/${provider.id}`) return json({ provider: provider })
    if (path.startsWith('/model-providers')) return json({ providers: [provider] })
    return json({ users: [] })
  })
  const user = userEvent.setup()
  render(<App client={fixture.client} />)
  await user.click(await screen.findByRole('link', { name: '模型供应商' }))
  await user.click(await screen.findByRole('link', { name: '模型管理' }))
  await user.click(screen.getByRole('button', { name: '从供应商拉取' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('供应商限流')
  expect(reads).toBe(1)
  await user.click(screen.getByRole('button', { name: '从供应商拉取' }))
  expect(await screen.findByText('供应商没有返回任何模型。')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: '登记所选（0）' })).toBeDisabled()
  await user.click(screen.getByRole('button', { name: '取消' }))
  await user.click(screen.getByRole('button', { name: '从供应商拉取' }))
  await user.click(await screen.findByRole('button', { name: '展开 已登记模型 详细信息' }))
  expect(screen.getByRole('checkbox', { name: '选择 已登记模型' })).toBeDisabled()
  expect(screen.getByRole('button', { name: '登记所选（0）' })).toBeDisabled()
  expect(screen.getByLabelText('已登记模型 · 上下文窗口')).toBeVisible()
  expect(screen.getByLabelText('已登记模型 · 上下文窗口')).toBeDisabled()
  expect(reads).toBe(3)
})
