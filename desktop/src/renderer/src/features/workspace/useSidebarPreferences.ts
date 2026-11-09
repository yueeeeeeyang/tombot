import { useRef, useState } from 'react'
import {
  createSidebarPreferences,
  loadSidebarPreferences,
  persistSidebarPreferences,
  type SidebarPreferences,
} from './sidebarPreferences'

/** 单窗口侧栏偏好；校验和写入失败显式上报，不覆盖损坏数据或伪装保存成功。 */
export function useSidebarPreferences() {
  const [initial] = useState(() => {
    try {
      return { preferences: loadSidebarPreferences(localStorage), error: '' }
    } catch (failure) {
      return { preferences: createSidebarPreferences(), error: (failure as Error).message }
    }
  })
  const [preferences, setPreferences] = useState(initial.preferences)
  const current = useRef(preferences)
  function update(updatePreferences: (previous: SidebarPreferences) => SidebarPreferences): void {
    if (initial.error) throw new Error(initial.error)
    const next = updatePreferences(current.current)
    if (JSON.stringify(next) === JSON.stringify(current.current)) return
    persistSidebarPreferences(localStorage, next)
    current.current = next
    setPreferences(next)
  }
  return { preferences, update, error: initial.error }
}
