import { describe, expect, it } from 'vitest'
import { createSettingsDraft } from '../settings/settings'
import { getModelOptions } from './modelOptions'

describe('招聘模型预览选项', () => {
  it('没有配置时返回空列表，不加入内置模型或默认模型', () => {
    expect(getModelOptions(createSettingsDraft())).toEqual([])
  })

  it('可选择设置中填写的模型，忽略空白名称，不修改设置原文', () => {
    const draft = { ...createSettingsDraft(), modelName: '  my-model  ' }
    expect(getModelOptions(draft)).toEqual([
      {
        value: 'configured:deepseek:my-model',
        label: 'my-model',
        description: 'deepseek',
      },
    ])
    expect(draft.modelName).toBe('  my-model  ')
    expect(getModelOptions({ ...draft, modelName: '  ' })).toEqual(
      getModelOptions(createSettingsDraft()),
    )
  })
})
