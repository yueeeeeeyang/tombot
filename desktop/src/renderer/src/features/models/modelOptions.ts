import type { SelectOption } from '../../components/Select'
import type { SettingsDraft } from '../settings/settings'

/** 仅将模型设置中填写的名称列为招聘选项，不补充内置模型或发起供应商请求。 */
export function getModelOptions(draft: SettingsDraft): readonly SelectOption[] {
  const modelName = draft.modelName.trim()
  if (!modelName) return []
  return [
    {
      value: `configured:${draft.provider}:${modelName}`,
      label: modelName,
      description: draft.provider === 'custom' ? '自定义供应商' : draft.provider,
    },
  ]
}
