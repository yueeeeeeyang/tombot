export type SettingsSection = 'general' | 'models'
export type ThemePreference = 'system' | 'light' | 'dark'
export type LanguagePreference = 'system' | 'zh-CN' | 'en'
export type ModelProvider = 'openai' | 'anthropic' | 'deepseek' | 'google' | 'custom'

export interface SettingsDraft {
  language: LanguagePreference
  provider: ModelProvider
  baseUrl: string
  modelName: string
  temperature: number
  topP: number
  maxOutputTokens: string
  isStreaming: boolean
}

export const MODEL_PARAMETER_LIMITS = {
  temperature: { min: 0, max: 2, step: 0.1 },
  topP: { min: 0, max: 1, step: 0.05 },
  maxOutputTokens: { min: 1, max: 131072 },
} as const

/** 创建仅用于界面预览的设置草稿，不读取凭据或写入持久化存储。 */
export function createSettingsDraft(): SettingsDraft {
  return {
    language: 'system',
    provider: 'openai',
    baseUrl: '',
    modelName: '',
    temperature: 0.7,
    topP: 1,
    maxOutputTokens: '4096',
    isStreaming: true,
  }
}

/** 校验预览表单中的地址和长度，返回字段错误；不发起网络请求。 */
export function validateSettingsDraft(draft: SettingsDraft): {
  baseUrl?: string
  maxOutputTokens?: string
} {
  const errors: { baseUrl?: string; maxOutputTokens?: string } = {}
  if (draft.baseUrl.trim()) {
    try {
      const address = new URL(draft.baseUrl.trim())
      if (!['https:', 'http:'].includes(address.protocol) || address.username || address.password) {
        errors.baseUrl = '请输入不含登录凭据的 HTTP 或 HTTPS 地址。'
      }
    } catch {
      errors.baseUrl = '请输入完整的 HTTP 或 HTTPS 地址。'
    }
  }
  const { min, max } = MODEL_PARAMETER_LIMITS.maxOutputTokens
  if (
    !/^\d+$/.test(draft.maxOutputTokens) ||
    Number(draft.maxOutputTokens) < min ||
    Number(draft.maxOutputTokens) > max
  ) {
    errors.maxOutputTokens = `请输入 ${min}～${max} 之间的整数。`
  }
  return errors
}
