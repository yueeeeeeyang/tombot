import { createWorkspace, workspaceSchema, type Workspace } from './model'
import { migrateCatAvatars, PREVIOUS_STORAGE_KEY } from './migrations/v2-cat-avatars'

export const STORAGE_KEY = 'tombot.workspace.v2'
const MAX_STORAGE_BYTES = 4 * 1024 * 1024
export type WorkspaceStorage = Pick<Storage, 'getItem' | 'setItem'>

/** 从指定存储读取并校验工作空间；损坏或不可读时抛错，保留原始内容。 */
export function loadWorkspace(storage: WorkspaceStorage): Workspace {
  let serialized: string | null
  let previous: string | null = null
  try {
    serialized = storage.getItem(STORAGE_KEY)
    if (serialized === null) previous = storage.getItem(PREVIOUS_STORAGE_KEY)
  } catch {
    throw new Error('无法读取本地草稿，请检查应用的存储权限后重试。')
  }
  if (serialized === null && previous === null) return createWorkspace()
  let workspace: Workspace
  try {
    workspace =
      serialized === null
        ? migrateCatAvatars(JSON.parse(previous!))
        : workspaceSchema.parse(JSON.parse(serialized))
  } catch {
    throw new Error('本地草稿格式异常，原始内容已保留。请修复存储后重新打开客户端。')
  }
  if (serialized === null) {
    // 先成功写入新版本，旧版本留作回滚备份，避免升级中断导致草稿丢失。
    try {
      persistWorkspace(storage, workspace)
    } catch {
      throw new Error('猫咪头像升级保存失败，原始草稿已保留。请检查存储空间后重新打开客户端。')
    }
  }
  return workspace
}

/** 完整校验后原子替换本地内容；容量或写入失败时抛错，调用方不得标记保存成功。 */
export function persistWorkspace(storage: WorkspaceStorage, workspace: Workspace): void {
  const serialized = JSON.stringify(workspaceSchema.parse(workspace))
  if (new Blob([serialized]).size > MAX_STORAGE_BYTES) {
    throw new Error('本地草稿空间已满，请先导出并删除不需要的草稿。')
  }
  try {
    storage.setItem(STORAGE_KEY, serialized)
  } catch {
    throw new Error('本地保存失败，当前内容仅保留在内存中。请释放磁盘空间后重试保存。')
  }
}
