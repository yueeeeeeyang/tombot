import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { ApiError } from '../api/client'
import { PolicyEditor } from './PolicyEditor'
import { failure, id, json, makeClient } from '../../tests/fixtures'
import { chooseOption } from '../../tests/select'

const provider = {
  id: id(4),
  name: '测试供应商',
  protocol: 'responses',
  base_url: 'https://models.example.test',
  is_enabled: true,
  revision: 1,
  created_at: new Date().toISOString(),
}
const model = {
  id: id(5),
  provider_id: provider.id,
  model_key: 'fast',
  display_name: '快速模型',
  context_limit: 8192,
  output_limit: 1024,
  supports_tools: true,
  supports_vision: false,
  is_enabled: true,
  revision: 1,
  reasoning_effort: 'high',
  metadata: { effort: { supported_levels: ['low', 'high'], default_level: 'high' } },
}
const second = {
  ...model,
  id: id(7),
  model_key: 'vision',
  display_name: '视觉模型',
  supports_vision: true,
}
const policy = {
  id: id(6),
  name: '多模型策略',
  is_default: false,
  is_enabled: true,
  revision: 1,
  current_version: null,
}
function setup(save: (body: Record<string, unknown>) => Response) {
  return makeClient((path, options) => {
    if (path.startsWith('/model-providers?')) return json({ providers: [provider] })
    if (path.includes('/credentials')) return json({ credentials: [] })
    if (path.includes('/models?'))
      return json({
        models: [
          model,
          second,
          { ...model, id: id(8), display_name: '停用模型', is_enabled: false },
        ],
      })
    return save(JSON.parse(String(options.body)))
  })
}
async function add(user: ReturnType<typeof userEvent.setup>, name: string) {
  await chooseOption(user, '候选模型', name)
  await user.click(screen.getByRole('button', { name: '添加' }))
}

describe('多模型策略编辑', () => {
  it('取消授权后保留候选并允许重新保存', async () => {
    let writes = 0
    const fixture = setup(() => {
      writes++
      return failure('VERIFICATION_REQUIRED', '需要验证', 403)
    })
    await fixture.client.restore()
    fixture.client.setVerifier(async () => {
      throw new ApiError('VERIFICATION_CANCELED', '已取消授权，操作已终止')
    })
    const user = userEvent.setup()
    render(
      <PolicyEditor
        client={fixture.client}
        policy={policy}
        onClose={() => {}}
        onSaved={() => {}}
      />,
    )
    await screen.findByRole('combobox', { name: '候选供应商' })
    await chooseOption(user, '候选供应商', provider.name)
    await add(user, model.display_name)
    const save = screen.getByRole('button', { name: '保存版本' })
    await user.click(save)
    expect(save).toBeEnabled()
    expect(screen.getByLabelText('快速模型 优先级')).toBeEnabled()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText('关闭并重新读取')).not.toBeInTheDocument()
    expect(writes).toBe(1)
    await user.click(save)
    expect(writes).toBe(2)
  })
  it('添加不同能力候选，默认继承且保存权重，不允许重复选择', async () => {
    let submitted: Record<string, unknown> | undefined
    const fixture = setup((body) => {
      submitted = body
      return failure('MODEL_CONFIGURATION_CONFLICT', '配置已变更', 409)
    })
    await fixture.client.restore()
    const user = userEvent.setup()
    render(
      <PolicyEditor
        client={fixture.client}
        policy={policy}
        onClose={() => {}}
        onSaved={() => {}}
      />,
    )
    expect(screen.getByRole('button', { name: '保存版本' })).toBeDisabled()
    await screen.findByRole('combobox', { name: '候选供应商' })
    await chooseOption(user, '候选供应商', provider.name)
    await add(user, model.display_name)
    await add(user, second.display_name)
    expect(screen.getAllByText('继承模型配置', { exact: true })).toHaveLength(2)
    await user.click(screen.getByRole('combobox', { name: '候选模型' }))
    expect(screen.queryByRole('option', { name: model.display_name })).not.toBeInTheDocument()
    expect(screen.queryByRole('option', { name: '停用模型' })).not.toBeInTheDocument()
    await user.keyboard('{Escape}')
    await chooseOption(user, '路由方式', '加权分配')
    await user.clear(screen.getByLabelText('快速模型 分配权重'))
    await user.type(screen.getByLabelText('快速模型 分配权重'), '8')
    await user.click(screen.getByRole('button', { name: '保存版本' }))
    expect(submitted).toEqual({
      routing_mode: 'weighted',
      candidates: [
        { model_id: model.id, priority: 1, weight: 8 },
        { model_id: second.id, priority: 2, weight: 1 },
      ],
    })
    expect(await screen.findByRole('alert')).toHaveTextContent('配置已变更')
    expect(screen.getByRole('button', { name: '保存版本' })).toBeDisabled()
  })
  it('可单独覆盖参数、移除候选并选择轮询', async () => {
    let submitted: Record<string, unknown> | undefined
    const fixture = setup((body) => {
      submitted = body
      return failure('MODEL_CONFIGURATION_CONFLICT', '需要重新读取', 409)
    })
    await fixture.client.restore()
    const user = userEvent.setup()
    render(
      <PolicyEditor
        client={fixture.client}
        policy={policy}
        onClose={() => {}}
        onSaved={() => {}}
      />,
    )
    await screen.findByRole('combobox', { name: '候选供应商' })
    await chooseOption(user, '候选供应商', provider.name)
    await add(user, model.display_name)
    await add(user, second.display_name)
    await user.click(screen.getByRole('button', { name: '移除 视觉模型' }))
    const candidate = screen.getByRole('region', { name: '候选 快速模型' })
    await user.click(within(candidate).getByText(/参数设置/))
    expect(screen.getByLabelText('快速模型 输出上限')).toHaveValue(null)
    await user.type(screen.getByLabelText('快速模型 输出上限'), '512')
    await chooseOption(user, '快速模型 思考级别', 'low')
    await chooseOption(user, '路由方式', '轮询')
    await user.click(screen.getByRole('button', { name: '保存版本' }))
    expect(submitted).toEqual({
      routing_mode: 'round_robin',
      candidates: [
        { model_id: model.id, priority: 1, weight: 1, output_limit: 512, reasoning_effort: 'low' },
      ],
    })
  })
  it('添加时设置思考级别，切换模型或添加后重置，不支持的级别不可选择', async () => {
    let submitted: Record<string, unknown> | undefined
    const fixture = setup((body) => {
      submitted = body
      return failure('MODEL_CONFIGURATION_CONFLICT', '配置已变更', 409)
    })
    await fixture.client.restore()
    const user = userEvent.setup()
    render(
      <PolicyEditor
        client={fixture.client}
        policy={policy}
        onClose={() => {}}
        onSaved={() => {}}
      />,
    )
    await screen.findByRole('combobox', { name: '候选供应商' })
    await chooseOption(user, '候选供应商', provider.name)
    expect(await screen.findByRole('combobox', { name: '候选思考级别' })).toBeDisabled()
    await chooseOption(user, '候选模型', model.display_name)
    await chooseOption(user, '候选思考级别', 'low')
    await chooseOption(user, '候选模型', second.display_name)
    expect(screen.getByRole('combobox', { name: '候选思考级别' })).toHaveTextContent(
      '继承模型配置（high）',
    )
    await chooseOption(user, '候选模型', model.display_name)
    await chooseOption(user, '候选思考级别', 'low')
    await user.click(screen.getByRole('button', { name: '添加' }))
    expect(screen.getByRole('region', { name: '候选 快速模型' })).toHaveTextContent('思考级别：low')
    expect(screen.getByRole('combobox', { name: '候选思考级别' })).toBeDisabled()
    await chooseOption(user, '候选模型', second.display_name)
    expect(screen.getByRole('combobox', { name: '候选思考级别' })).toHaveTextContent(
      '继承模型配置（high）',
    )
    await user.click(screen.getByRole('combobox', { name: '候选思考级别' }))
    expect(screen.queryByRole('option', { name: 'xhigh' })).not.toBeInTheDocument()
    await user.keyboard('{Escape}')
    await chooseOption(user, '候选思考级别', '不发送思考级别')
    await user.click(screen.getByRole('button', { name: '添加' }))
    await user.click(screen.getByRole('button', { name: '保存版本' }))
    expect(submitted).toEqual({
      routing_mode: 'priority',
      candidates: [
        { model_id: model.id, priority: 1, weight: 1, reasoning_effort: 'low' },
        { model_id: second.id, priority: 2, weight: 1, reasoning_effort: '' },
      ],
    })
  })
  it('选择器不显示表格页脚，多页数据可翻页并在读取失败后重试', async () => {
    const providers = [
      provider,
      ...Array.from({ length: 49 }, (_, index) => ({
        ...provider,
        id: String(100 + index).padStart(26, '0'),
        name: `供应商 ${index}`,
      })),
    ]
    const models = [
      model,
      ...Array.from({ length: 49 }, (_, index) => ({
        ...model,
        id: String(1000 + index).padStart(26, '0'),
        display_name: `模型 ${index}`,
      })),
    ]
    let failNext = true
    const fixture = makeClient((path) => {
      const url = new URL(path, 'https://admin.example.test')
      expect(url.searchParams.get('limit')).toBe('50')
      if (url.pathname === '/model-providers') {
        if (url.searchParams.has('before')) {
          expect(url.searchParams.get('before')).toBe(providers.at(-1)!.id)
          if (failNext) {
            failNext = false
            return failure('SERVICE_UNAVAILABLE', '供应商暂时无法读取', 503)
          }
          return json({ providers: [{ ...provider, id: id(9), name: '下一页供应商' }] })
        }
        return json({ providers })
      }
      if (url.pathname.endsWith('/credentials')) return json({ credentials: [] })
      if (url.pathname.endsWith('/models')) {
        if (url.searchParams.has('before')) {
          expect(url.searchParams.get('before')).toBe(models.at(-1)!.id)
          return json({ models: [second] })
        }
        return json({ models })
      }
      throw new Error(`未配置的测试接口：${url.pathname}`)
    })
    await fixture.client.restore()
    const user = userEvent.setup()
    render(
      <PolicyEditor
        client={fixture.client}
        policy={policy}
        onClose={() => {}}
        onSaved={() => {}}
      />,
    )
    await user.click(await screen.findByRole('button', { name: '更多供应商' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('供应商暂时无法读取')
    await user.click(screen.getByRole('button', { name: '重新读取' }))
    await chooseOption(user, '候选供应商', '下一页供应商')
    expect(screen.queryByRole('button', { name: '更多供应商' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '返回供应商首页' }))
    await chooseOption(user, '候选供应商', provider.name)
    await user.click(await screen.findByRole('button', { name: '更多模型' }))
    await add(user, second.display_name)
    expect(screen.queryByRole('button', { name: '更多模型' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '返回模型首页' }))
    await screen.findByRole('button', { name: '更多模型' })
    expect(screen.getByRole('region', { name: '候选 视觉模型' })).toBeInTheDocument()
    expect(screen.queryByText(/本页\s*\d+\s*条/)).not.toBeInTheDocument()
    expect(screen.getByRole('dialog').querySelector('.pagination')).toBeNull()
  })
})
