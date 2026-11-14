import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { ApiError } from '../api/client'
import { ModelEditor } from './ModelEditor'
import { failure, id, json, makeClient } from '../../tests/fixtures'
import { chooseOption } from '../../tests/select'

const model = {
  id: id(5),
  provider_id: id(4),
  model_key: 'team-model',
  display_name: '测试模型',
  context_limit: 8192,
  output_limit: 1024,
  supports_tools: true,
  supports_vision: false,
  is_enabled: false,
  revision: 2,
  reasoning_effort: 'high',
  metadata: {
    input_modalities: ['text'],
    output_modalities: ['text'],
    effort: { supported_levels: ['low', 'high'], default_level: 'high' },
  },
}

describe('停用模型编辑', () => {
  it('取消授权后保留表单并恢复编辑，不显示结果不确定或错误提示', async () => {
    let calls = 0
    const fixture = makeClient(() => {
      calls++
      return failure('VERIFICATION_REQUIRED', '需要验证', 403)
    })
    await fixture.client.restore()
    fixture.client.setVerifier(async () => {
      throw new ApiError('VERIFICATION_CANCELED', '已取消授权，操作已终止')
    })
    const user = userEvent.setup()
    render(<ModelEditor client={fixture.client} model={model} onClose={() => {}} />)
    const save = screen.getByRole('button', { name: '保存修改' })
    await user.click(save)
    expect(save).toBeEnabled()
    expect(screen.getByLabelText('显示名称')).toBeEnabled()
    expect(screen.getByLabelText('显示名称')).toHaveValue(model.display_name)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText('关闭并重新读取')).not.toBeInTheDocument()
    expect(calls).toBe(1)
    await user.click(save)
    expect(calls).toBe(2)
  })
  it('预填参数，校验上限并携带修订号提交元数据，不修改归属和启用状态', async () => {
    let submitted: Record<string, unknown> | undefined
    let closed = false
    const fixture = makeClient((_path, options) => {
      submitted = JSON.parse(String(options.body))
      return json({ model: { ...model, ...submitted, revision: 3 } })
    })
    await fixture.client.restore()
    const user = userEvent.setup()
    render(
      <ModelEditor
        client={fixture.client}
        model={model}
        onClose={() => {
          closed = true
        }}
      />,
    )
    expect(screen.getByLabelText('显示名称')).toHaveValue('测试模型')
    expect(screen.getByLabelText('支持的思考级别')).toHaveValue('low, high')
    await user.clear(screen.getByLabelText('输出上限'))
    await user.type(screen.getByLabelText('输出上限'), '9000')
    await user.click(screen.getByRole('button', { name: '保存修改' }))
    expect(screen.getByRole('alert')).toHaveTextContent('输出上限不能超过上下文窗口')
    expect(submitted).toBeUndefined()
    await user.clear(screen.getByLabelText('输出上限'))
    await user.type(screen.getByLabelText('输出上限'), '2048')
    await chooseOption(user, '使用的思考级别', 'low')
    await user.click(screen.getByRole('button', { name: '保存修改' }))
    expect(submitted).toMatchObject({
      expected_revision: 2,
      model_key: 'team-model',
      output_limit: 2048,
      reasoning_effort: 'low',
      metadata: model.metadata,
    })
    expect(submitted).not.toHaveProperty('provider_id')
    expect(submitted).not.toHaveProperty('is_enabled')
    expect(closed).toBe(true)
  })
  it('编辑冲突保留错误并停止重发', async () => {
    let calls = 0
    const fixture = makeClient(() => {
      calls++
      return failure('MODEL_CONFIGURATION_CONFLICT', '模型配置已变更', 409)
    })
    await fixture.client.restore()
    const user = userEvent.setup()
    render(<ModelEditor client={fixture.client} model={model} onClose={() => {}} />)
    await user.click(screen.getByRole('button', { name: '保存修改' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('模型配置已变更')
    expect(screen.getByRole('button', { name: '保存修改' })).toBeDisabled()
    expect(calls).toBe(1)
  })
})
