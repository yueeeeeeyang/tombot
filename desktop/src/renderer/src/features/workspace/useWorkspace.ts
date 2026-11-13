import { useEffect, useRef, useState } from 'react'
import {
  getBackend,
  type DeliveryResult,
  type DesktopBackend,
  type IncomingMessages,
  type ServerBubble,
  type StreamEvent,
} from '../desktop/backend'
import { emptyWorkspace, type AppWorkspace, type ChatMessage } from './model'
import { isReactionEmoji } from './reactions'

interface WorkspaceState {
  workspace: AppWorkspace
  error: string
  isBlocked: boolean
  ready: boolean
}

/** 把服务端消息转换成气泡记录；服务端副本一律视为已发送，未知表情直接忽略。 */
function toChatMessage(bubble: ServerBubble): ChatMessage {
  return {
    id: bubble.id,
    role: bubble.role,
    content: bubble.content,
    createdAt: bubble.createdAt,
    status: 'sent',
    reactions: bubble.reactions.flatMap((reaction) =>
      isReactionEmoji(reaction.emoji) ? [{ emoji: reaction.emoji, count: reaction.count }] : [],
    ),
    ...(bubble.attachments.length > 0
      ? {
          attachments: bubble.attachments.map((attachment) => ({
            name: attachment.name,
            path: attachment.path,
            sha256: attachment.sha256,
            mediaType: attachment.mediaType,
            kind: attachment.kind,
          })),
        }
      : {}),
  }
}

/** 只合并已落库的通知和直接回复，不回写服务，也不覆盖正在编辑的草稿与资料。 */
export function mergeIncoming(local: AppWorkspace, incoming: IncomingMessages): AppWorkspace {
  return {
    ...local,
    agents: local.agents.map((agent) => {
      const received = incoming.find((item) => item.id === agent.id)
      if (!received) return agent
      const messages = [...agent.messages]
      for (const message of received.messages) {
        const index = messages.findIndex((item) => item.id === message.id)
        if (index < 0) messages.push(message)
        else messages[index] = message
      }
      return { ...agent, messages }
    }),
  }
}

function readInitial(backend: DesktopBackend): WorkspaceState {
  const sync = backend.tryWorkspaceSync?.()
  // 没有同步快照时先占位，真正的列表等服务返回后再换上，不用本机旧档填充。
  if (!sync) return { workspace: emptyWorkspace(), error: '', isBlocked: false, ready: false }
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
      try {
        const snapshot = await backend.loadWorkspace()
        if (cancelled) return
        current.current = snapshot.workspace
        setWorkspace(snapshot.workspace)
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

  function receive(incoming: IncomingMessages): void {
    current.current = mergeIncoming(current.current, incoming)
    setWorkspace(current.current)
  }

  /**
   * 把一条用户消息交给服务端执行。
   * 服务端返回该会话的最新消息列表后整体替换本地记录：服务端才是权威副本，
   * 本地未发送状态不能覆盖已经持久化的回合。
   */
  function deliver(
    agentId: string,
    content: string,
    onEvent?: (event: StreamEvent) => void,
  ): Promise<DeliveryResult> {
    return enqueue(async () => {
      try {
        const pending = backend.saveWorkspace(current.current)
        if (pending) await pending
        const receiveEvent = (event: StreamEvent) => {
          if (event.type === 'incoming' && event.incoming) receive(event.incoming)
          else onEvent?.(event)
        }
        const result = await backend.deliverMessage(agentId, content, receiveEvent)
        if (result.messages) {
          const messages = result.messages.map(toChatMessage)
          const merged: AppWorkspace = {
            ...current.current,
            agents: current.current.agents.map((agent) =>
              agent.id === agentId ? { ...agent, messages } : agent,
            ),
          }
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

  return { workspace, error, isBlocked, ready, commit, deliver, receive }
}
