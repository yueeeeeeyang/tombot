import { useRef, useState } from 'react'
import { createWorkspace, type CurrentWorkspace } from './model'
import { loadWorkspace, persistWorkspace } from './storage'

/** 管理单窗口工作空间；写入失败保留内存内容并显示错误，读取失败时禁止覆盖原始存储。 */
export function useWorkspace() {
  const [initial] = useState(() => {
    try {
      return { workspace: loadWorkspace(localStorage), error: '', isBlocked: false }
    } catch (error) {
      return { workspace: createWorkspace(), error: (error as Error).message, isBlocked: true }
    }
  })
  const [workspace, setWorkspace] = useState(initial.workspace)
  const [error, setError] = useState(initial.error)
  const current = useRef(workspace)

  /** 同步应用领域变更；返回是否已持久化，非法操作抛错且不改变状态。 */
  function commit(update: (previous: CurrentWorkspace) => CurrentWorkspace): boolean {
    if (initial.isBlocked) throw new Error(initial.error)
    const next = update(current.current)
    current.current = next
    setWorkspace(next)
    try {
      persistWorkspace(localStorage, next)
      setError('')
      return true
    } catch (failure) {
      setError((failure as Error).message)
      return false
    }
  }

  return { workspace, error, isBlocked: initial.isBlocked, commit }
}
