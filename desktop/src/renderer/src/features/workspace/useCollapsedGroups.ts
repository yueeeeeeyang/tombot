import { useState } from 'react'
import { z } from 'zod'

const STORAGE_KEY = 'tombot.collapsed-groups.v1'
const groupsSchema = z.array(z.string().min(1).max(160)).max(128)

/** 分组折叠只保存在当前设备；存储失败不应用操作并保留原始内容。 */
export function useCollapsedGroups(onError: (message: string) => void) {
  const [initial] = useState(() => {
    try {
      const raw = localStorage.getItem(STORAGE_KEY)
      return { groups: raw ? groupsSchema.parse(JSON.parse(raw)) : [], error: '' }
    } catch {
      return { groups: [] as string[], error: '无法读取分组折叠状态，原始内容已保留。' }
    }
  })
  const [collapsed, setCollapsed] = useState(initial.groups)
  function toggle(group: string) {
    if (initial.error) {
      onError(initial.error)
      return
    }
    const next = collapsed.includes(group)
      ? collapsed.filter((item) => item !== group)
      : [...collapsed, group]
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(groupsSchema.parse(next)))
      setCollapsed(next)
    } catch {
      onError('无法保存分组折叠状态，本次操作未生效。')
    }
  }
  return { collapsed, toggle, error: initial.error }
}
