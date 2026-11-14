import { useEffect, useState } from 'react'
import { SunMoon } from 'lucide-react'
import { Select } from './Select'

export type Theme = 'system' | 'light' | 'dark'
export const themeOptions = [
  { value: 'system', label: '跟随系统' },
  { value: 'light', label: '浅色' },
  { value: 'dark', label: '深色' },
] as const

function readTheme(): Theme {
  try {
    const value = localStorage.getItem('tombot-admin-theme')
    return value === 'light' || value === 'dark' ? value : 'system'
  } catch {
    return 'system'
  }
}

/** 登录页与账户菜单共用主题应用和保存规则，系统主题变化时即时更新。 */
export function useTheme() {
  const [theme, updateTheme] = useState<Theme>(readTheme)
  const [storageError, setStorageError] = useState(false)
  useEffect(() => {
    const preference = matchMedia('(prefers-color-scheme: dark)')
    const apply = () => {
      document.documentElement.dataset.theme =
        theme === 'system' ? (preference.matches ? 'dark' : 'light') : theme
    }
    apply()
    preference.addEventListener('change', apply)
    return () => preference.removeEventListener('change', apply)
  }, [theme])
  function setTheme(value: Theme) {
    updateTheme(value)
    try {
      localStorage.setItem('tombot-admin-theme', value)
      setStorageError(false)
    } catch {
      setStorageError(true)
    }
  }
  return { theme, setTheme, storageError }
}

export function ThemePicker() {
  const { theme, setTheme, storageError } = useTheme()
  return (
    <div className="theme-picker">
      <SunMoon size={16} />
      <Select
        label="外观"
        value={theme}
        onChange={(value) => setTheme(value as Theme)}
        options={themeOptions}
      />
      {storageError && <small role="status">外观仅在本次页面生效</small>}
    </div>
  )
}
