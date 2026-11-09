import { z } from 'zod'
import { MAX_AGENTS } from './model'
import type { WorkspaceStorage } from './storage'

export const SIDEBAR_STORAGE_KEY = 'tombot.sidebar.v1'
const idsSchema = z
  .array(z.string().min(1).max(128))
  .max(MAX_AGENTS)
  .refine((ids) => new Set(ids).size === ids.length)
const preferencesSchema = z.object({
  version: z.literal(1),
  pinnedIds: idsSchema.refine((ids) => !ids.includes('assistant')),
  unreadIds: idsSchema,
  hiddenIds: idsSchema,
})
export type SidebarPreferences = z.infer<typeof preferencesSchema>
export type SidebarFlag = 'pinnedIds' | 'unreadIds' | 'hiddenIds'

/** 初始化独立的展示偏好，不改变工作空间格式或默认猫咪的固定位置。 */
export function createSidebarPreferences(): SidebarPreferences {
  return { version: 1, pinnedIds: [], unreadIds: [], hiddenIds: [] }
}

/** 读取并完整校验侧栏偏好；不可读或损坏时抛错，保留原文供恢复。 */
export function loadSidebarPreferences(storage: WorkspaceStorage): SidebarPreferences {
  let serialized: string | null
  try {
    serialized = storage.getItem(SIDEBAR_STORAGE_KEY)
  } catch {
    throw new Error('无法读取侧栏偏好，请检查存储权限后重启。')
  }
  if (serialized === null) return createSidebarPreferences()
  try {
    return preferencesSchema.parse(JSON.parse(serialized))
  } catch {
    throw new Error('侧栏偏好格式异常，原始内容已保留；暂时显示全部猫咪。')
  }
}

/** 保存成功后才能应用展示变更；失败时抛错，本次操作不生效。 */
export function persistSidebarPreferences(
  storage: WorkspaceStorage,
  preferences: SidebarPreferences,
): void {
  const serialized = JSON.stringify(preferencesSchema.parse(preferences))
  try {
    storage.setItem(SIDEBAR_STORAGE_KEY, serialized)
  } catch {
    throw new Error('侧栏偏好保存失败，本次操作未生效。请检查存储空间后重试。')
  }
}

/** 更新指定猫咪的展示状态，并清理已删除猫咪的引用；未知标识不能写入。 */
export function setSidebarFlag(
  preferences: SidebarPreferences,
  agentIds: string[],
  id: string,
  flag: SidebarFlag,
  enabled: boolean,
): SidebarPreferences {
  if (!agentIds.includes(id)) throw new Error('猫咪不存在')
  if (id === 'assistant' && flag === 'pinnedIds') throw new Error('默认猫咪固定置顶')
  if (id === 'assistant' && flag === 'hiddenIds') throw new Error('默认猫咪不能隐藏')
  const next = {
    ...preferences,
    pinnedIds: preferences.pinnedIds.filter((candidate) => agentIds.includes(candidate)),
    unreadIds: preferences.unreadIds.filter((candidate) => agentIds.includes(candidate)),
    hiddenIds: preferences.hiddenIds.filter((candidate) => agentIds.includes(candidate)),
  }
  next[flag] = next[flag].filter((candidate) => candidate !== id)
  if (enabled) next[flag].push(id)
  return preferencesSchema.parse(next)
}

/** 删除猫咪后清理全部展示引用，避免隐藏或未读记录残留。 */
export function removeSidebarAgent(
  preferences: SidebarPreferences,
  id: string,
): SidebarPreferences {
  return preferencesSchema.parse({
    ...preferences,
    pinnedIds: preferences.pinnedIds.filter((candidate) => candidate !== id),
    unreadIds: preferences.unreadIds.filter((candidate) => candidate !== id),
    hiddenIds: preferences.hiddenIds.filter((candidate) => candidate !== id),
  })
}
