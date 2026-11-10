import { useEffect, useRef, useState } from 'react'
import { getBackend, type DeliveryResult, type DesktopBackend } from '../desktop/backend'
import { readLegacyWorkspace } from '../desktop/legacyWorkspace'
import { createWorkspace, type AppWorkspace } from './model'

interface WorkspaceState {
  workspace: AppWorkspace
  error: string
  isBlocked: boolean
  ready: boolean
}

/** 把服务返回的消息叠回本地草稿，避免投递期间输入被盖掉。 */
function mergeDelivery(local: AppWorkspace, remote: AppWorkspace): AppWorkspace {
  const remoteIds = new Set(remote.agents.map((agent) => agent.id))
  const agents = remote.agents.map((agent) => {
    const current = local.agents.find((item) => item.id === agent.id)
    return current ? { ...agent, draft: current.draft } : agent
  })
  for (const agent of local.agents) {
    if (!remoteIds.has(agent.id)) agents.push(agent)
  }
  const activeAgentId = local.agents.some((agent) => agent.id === local.activeAgentId)
    ? local.activeAgentId
    : remote.activeAgentId
  return { ...remote, activeAgentId, agents }
}

function readInitial(backend: DesktopBackend): WorkspaceState {
  const legacy = readLegacyWorkspace(localStorage)
  if (legacy.status === 'blocked') {
    return { workspace: createWorkspace(), error: legacy.message, isBlocked: true, ready: true }
  }
  const sync = backend.tryWorkspaceSync?.()
  if (!sync) return { workspace: createWorkspace(), error: '', isBlocked: false, ready: false }
  if (legacy.status === 'ready' && sync.pristine) {
    try {
      const imported = backend.importWorkspace(legacy.workspace)
      if (!(imported instanceof Promise)) {
        return { workspace: imported, error: '', isBlocked: false, ready: true }
      }
    } catch (error) {
      return {
        workspace: sync.workspace,
        error: (error as Error).message,
        isBlocked: false,
        ready: true,
      }
    }
  }
  return { workspace: sync.workspace, error: '', isBlocked: false, ready: true }
}

/** 管理工作空间。服务是权威副本；写入失败时保留内存内容并提示。 */
export function useWorkspace() {
  const backend = getBackend()
  const [initial] = useState(() => readInitial(backend))
  const [workspace, setWorkspace] = useState(initial.workspace)
  const [error, setError] = useState(initial.error)
  const [isBlocked, setBlocked] = useState(initial.isBlocked)
  const [ready, setReady] = useState(initial.ready)
  const [saveState, setSaveState] = useState<'saved' | 'saving' | 'error'>('saved')
  const current = useRef(workspace)
  const blocked = useRef(initial.isBlocked)
  const chain = useRef(Promise.resolve())
  const pendingTasks = useRef(0)

  useEffect(() => {
    document.documentElement.dataset.workspaceSave = saveState
  }, [saveState])

  useEffect(() => {
    if (initial.ready) return
    let cancelled = false
    void (async () => {
      const legacy = readLegacyWorkspace(localStorage)
      if (legacy.status === 'blocked') {
        if (cancelled) return
        blocked.current = true
        setBlocked(true)
        setError(legacy.message)
        setReady(true)
        return
      }
      try {
        const snapshot = await backend.loadWorkspace()
        let next = snapshot.workspace
        if (legacy.status === 'ready' && snapshot.pristine) {
          next = await backend.importWorkspace(legacy.workspace)
        }
        if (cancelled) return
        current.current = next
        setWorkspace(next)
        setReady(true)
      } catch (failure) {
        if (cancelled) return
        blocked.current = true
        setBlocked(true)
        setError((failure as Error).message)
        setReady(true)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [backend, initial.ready])

  function enqueue<T>(task: () => Promise<T>): Promise<T> {
    pendingTasks.current += 1
    setSaveState('saving')
    const run = chain.current.then(task, task)
    chain.current = run.then(
      () => {
        pendingTasks.current -= 1
        if (pendingTasks.current === 0) {
          setError('')
          setSaveState('saved')
        }
      },
      () => {
        pendingTasks.current -= 1
        setSaveState('error')
      },
    )
    return run
  }

  async function flushLatest(): Promise<void> {
    const snapshot = current.current
    const pending = backend.saveWorkspace(snapshot)
    if (pending) await pending
    if (current.current !== snapshot) await flushLatest()
  }

  function scheduleFlush(): void {
    void enqueue(async () => {
      try {
        await flushLatest()
      } catch (failure) {
        setError((failure as Error).message)
        throw failure
      }
    })
  }

  /** 同步应用领域变更。同步通道会立即返回是否写成功；服务通道先更新界面再排队保存。 */
  function commit(update: (previous: AppWorkspace) => AppWorkspace): boolean {
    if (blocked.current) throw new Error(error || initial.error)
    const next = update(current.current)
    current.current = next
    setWorkspace(next)
    if (backend.savesSynchronously) {
      try {
        backend.saveWorkspace(next)
        setError('')
        setSaveState('saved')
        return true
      } catch (failure) {
        setError((failure as Error).message)
        setSaveState('error')
        return false
      }
    }
    scheduleFlush()
    return true
  }

  /** 把一条用户消息交给服务。模型未配置时服务不会改写未发送状态。 */
  function deliver(agentId: string, messageId: string): Promise<DeliveryResult> {
    return enqueue(async () => {
      try {
        const pending = backend.saveWorkspace(current.current)
        if (pending) await pending
        const result = await backend.deliverMessage(agentId, messageId)
        if (result.workspace) {
          const merged = mergeDelivery(current.current, result.workspace)
          current.current = merged
          setWorkspace(merged)
          const again = backend.saveWorkspace(merged)
          if (again) await again
        }
        return result
      } catch (failure) {
        return { code: 'DELIVER_FAILED', message: (failure as Error).message }
      }
    })
  }

  return { workspace, error, isBlocked, ready, commit, deliver }
}
