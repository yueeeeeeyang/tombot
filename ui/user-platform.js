// @ts-check

// 主题偏好不含身份信息；读取失败仍可切换，但明确提示不能持久保存。
const themeKey = 'tombot-user-theme'
const preference = matchMedia('(prefers-color-scheme: dark)')
let theme = 'system'
let storageFailed = false
try {
  const saved = localStorage.getItem(themeKey)
  if (saved === 'light' || saved === 'dark') theme = saved
} catch {
  storageFailed = true
}
function applyTheme() {
  document.documentElement.dataset.theme =
    theme === 'system' ? (preference.matches ? 'dark' : 'light') : theme
}
applyTheme()
preference.addEventListener('change', applyTheme)

document.addEventListener('DOMContentLoaded', () => {
  const select = document.querySelector('#theme-select')
  const notice = document.querySelector('#theme-error')
  const account = document.querySelector('.account-control')
  if (
    !(select instanceof HTMLSelectElement) ||
    !(notice instanceof HTMLElement) ||
    !(account instanceof HTMLDetailsElement)
  )
    return
  const showStorageError = () => {
    notice.hidden = !storageFailed
    notice.textContent = storageFailed ? '浏览器无法保存主题偏好，本次切换仍然有效。' : ''
  }
  select.value = theme
  showStorageError()
  select.addEventListener('change', () => {
    theme = select.value
    applyTheme()
    try {
      localStorage.setItem(themeKey, theme)
      storageFailed = false
    } catch {
      storageFailed = true
    }
    showStorageError()
  })
  document.addEventListener('click', (event) => {
    if (event.target instanceof Node && !account.contains(event.target)) account.open = false
  })
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && account.open) {
      account.open = false
      const trigger = account.querySelector('summary')
      if (trigger instanceof HTMLElement) trigger.focus()
    }
  })
})
