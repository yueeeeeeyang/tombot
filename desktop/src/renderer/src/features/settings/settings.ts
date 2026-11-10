export type SettingsSection = 'general' | 'models'
export type ThemePreference = 'system' | 'light' | 'dark'
export type LanguagePreference = 'system' | 'zh-CN' | 'en'
export interface SettingsDraft {
  language: LanguagePreference
  provider: string
  providerName: string
  baseUrl: string
  modelName: string
}

/** 创建仅用于界面预览的设置草稿，不读取凭据或写入持久化存储。 */
export function createSettingsDraft(): SettingsDraft {
  return {
    language: 'system',
    provider: 'deepseek',
    providerName: 'DeepSeek',
    baseUrl: '',
    modelName: '',
  }
}

/** 校验预览表单中的地址，返回字段错误；不发起网络请求。 */
export function validateSettingsDraft(draft: SettingsDraft): { baseUrl?: string } {
  const errors: { baseUrl?: string } = {}
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
  return errors
}
