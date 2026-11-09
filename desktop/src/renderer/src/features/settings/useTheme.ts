import { useLayoutEffect, useState } from 'react'
import type { ThemePreference } from './settings'

export const THEME_STORAGE_KEY = 'tombot.theme.v1'

/** 读取独立的外观偏好；损坏或不可访问时明确提示，用户可重新选择以恢复。 */
function readTheme(): { theme: ThemePreference; error: string } {
  try {
    const theme = localStorage.getItem(THEME_STORAGE_KEY)
    if (theme === null) return { theme: 'system', error: '' }
    if (theme === 'system' || theme === 'light' || theme === 'dark') return { theme, error: '' }
    return { theme: 'system', error: '主题偏好无效，暂时跟随系统。请重新选择主题以保存。' }
  } catch {
    return { theme: 'system', error: '无法读取主题偏好，暂时跟随系统。请重新选择主题以重试。' }
  }
}

/** 即时应用主题并监听系统变化；保存失败仍保留当前外观并向用户报告。 */
export function useTheme() {
  const [preference, setPreference] = useState(readTheme)
  useLayoutEffect(() => {
    const systemTheme = window.matchMedia('(prefers-color-scheme: dark)')
    function applyTheme(): void {
      document.documentElement.dataset.theme =
        preference.theme === 'system' ? (systemTheme.matches ? 'dark' : 'light') : preference.theme
    }
    applyTheme()
    systemTheme.addEventListener('change', applyTheme)
    return () => systemTheme.removeEventListener('change', applyTheme)
  }, [preference.theme])

  function setTheme(theme: ThemePreference): void {
    try {
      localStorage.setItem(THEME_STORAGE_KEY, theme)
      setPreference({ theme, error: '' })
    } catch {
      setPreference({ theme, error: '主题已应用，但未能保存。请重新选择主题以重试保存。' })
    }
  }
  return { ...preference, setTheme }
}
