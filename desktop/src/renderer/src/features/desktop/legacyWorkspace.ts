import { V4_STORAGE_KEY } from '../workspace/migrations/v5-cat-surfaces'
import { V3_STORAGE_KEY } from '../workspace/migrations/v4-cat-model'
import { V2_STORAGE_KEY } from '../workspace/migrations/v3-message-interactions'
import { PREVIOUS_STORAGE_KEY } from '../workspace/migrations/v2-cat-avatars'
import { loadWorkspace, STORAGE_KEY, type WorkspaceStorage } from '../workspace/storage'
import type { AppWorkspace } from '../workspace/model'

const legacyKeys = [
  STORAGE_KEY,
  V4_STORAGE_KEY,
  V3_STORAGE_KEY,
  V2_STORAGE_KEY,
  PREVIOUS_STORAGE_KEY,
]

export type LegacyWorkspace =
  | { status: 'absent' }
  | { status: 'ready'; workspace: AppWorkspace }
  | { status: 'blocked'; message: string }

/** 只在确实存在旧档时读取。没有旧档不能当成一份新的预设去导入。 */
export function readLegacyWorkspace(storage: WorkspaceStorage): LegacyWorkspace {
  try {
    if (!legacyKeys.some((key) => storage.getItem(key) !== null)) return { status: 'absent' }
  } catch {
    return { status: 'blocked', message: '无法读取本地草稿，请检查应用的存储权限后重试。' }
  }
  try {
    return { status: 'ready', workspace: loadWorkspace(storage) }
  } catch (error) {
    return { status: 'blocked', message: (error as Error).message }
  }
}
