import { useState } from 'react'
import type { LanguagePreference } from './settings'

export const LANGUAGE_STORAGE_KEY = 'tombot.language.v1'

/** 语言属于本机外观偏好，不再依赖服务端模型配置接口。 */
function readLanguage(): { language: LanguagePreference; error: string } {
  try {
    const language = localStorage.getItem(LANGUAGE_STORAGE_KEY)
    if (language === null) return { language: 'system', error: '' }
    if (language === 'system' || language === 'zh-CN' || language === 'en')
      return { language, error: '' }
    return { language: 'system', error: '语言偏好无效，请重新选择语言以保存。' }
  } catch {
    return { language: 'system', error: '无法读取语言偏好，请重新选择语言以重试。' }
  }
}

/** 保存失败保留本次选择并提示重试，不假装已经持久化。 */
export function useLanguage() {
  const [preference, setPreference] = useState(readLanguage)
  function setLanguage(language: LanguagePreference): void {
    try {
      localStorage.setItem(LANGUAGE_STORAGE_KEY, language)
      setPreference({ language, error: '' })
    } catch {
      setPreference({ language, error: '语言偏好未能保存，请重新选择语言以重试。' })
    }
  }
  return { ...preference, setLanguage }
}
