import type { SelectOption } from '../../components/Select'

/** 每只猫咪可选的模型。只在这里维护，界面和存储校验都引用这份名单。 */
export const CAT_MODELS = [
  { id: 'gpt-4o', label: 'GPT-4o', provider: 'OpenAI' },
  { id: 'claude-sonnet', label: 'Claude Sonnet', provider: 'Anthropic' },
  { id: 'deepseek-v3', label: 'DeepSeek V3', provider: 'DeepSeek' },
  { id: 'gemini-flash', label: 'Gemini Flash', provider: 'Google' },
] as const

export type CatModelId = (typeof CAT_MODELS)[number]['id']
export const CAT_MODEL_IDS = CAT_MODELS.map((model) => model.id) as unknown as [
  CatModelId,
  ...CatModelId[],
]
export const DEFAULT_CAT_MODEL_ID: CatModelId = CAT_MODELS[0].id

/** 把目录转成下拉选项；不读取设置草稿，也不访问供应商。 */
export function catModelOptions(): readonly SelectOption<CatModelId>[] {
  return CAT_MODELS.map((model) => ({
    value: model.id,
    label: model.label,
    description: model.provider,
  }))
}

export function isCatModelId(modelId: string): modelId is CatModelId {
  return CAT_MODELS.some((model) => model.id === modelId)
}
