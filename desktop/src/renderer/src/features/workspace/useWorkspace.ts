import { useCallback, useEffect, useRef, useState } from 'react'
import {
  getBackend,
  type DeliveryResult,
  type DesktopBackend,
  type IncomingMessages,
  type ServerBubble,
  type StreamEvent,
} from '../desktop/backend'
import {
  addAgent,
  emptyWorkspace,
  type Agent,
  type NewAgent,
  type AppWorkspace,
  type ChatMessage,
} from './model'
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
    sequence: bubble.sequence,
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
      messages.sort((left, right) =>
        left.sequence && right.sequence
          ? left.sequence - right.sequence
          : left.createdAt.localeCompare(right.createdAt),
      )
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
export function useWorkspace(
  receiveActivity?: (source: string, event: StreamEvent) => void,
  clearActivity?: (source: string) => void,
) {
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

  const [historyPages, setHistoryPages] = useState<
    Record<string, { before: number; hasMore: boolean; loading: boolean }>
  >({})
  const paging = useRef(new Set<string>())
  const refreshing = useRef(new Map<string, { dirty: boolean }>())
  const refreshConversation = useCallback(
    async (agentId: string) => {
      if (!backend.loadAgentConversation || !agentId) return
      const active = refreshing.current.get(agentId)
      if (active) {
        active.dirty = true
        return
      }
      const request = { dirty: false }
      refreshing.current.set(agentId, request)
      try {
        do {
          request.dirty = false
          const page = await backend.loadAgentConversation(agentId)
          const firstSequence = page.messages[0]?.sequence ?? 0
          setHistoryPages((previous) => {
            const old = previous[agentId]
            if (old && old.before > 0 && (!firstSequence || firstSequence > old.before))
              return previous
            return {
              ...previous,
              [agentId]: {
                before: firstSequence,
                hasMore: page.hasMore,
                loading: old?.loading ?? false,
              },
            }
          })
          current.current = mergeIncoming(current.current, [
            { id: agentId, messages: page.messages.map(toChatMessage) },
          ])
          setWorkspace(current.current)
          const source = `server:${agentId}`
          clearActivity?.(source)
          for (const run of page.runs) {
            receiveActivity?.(source, {
              type: 'activity',
              text: '',
              botId: agentId,
              runId: run.id,
              isWorking: true,
            })
            const summary = run.progress_summary || (run.status === 'queued' ? '等待处理' : '')
            if (summary)
              receiveActivity?.(source, {
                type: 'process',
                text: summary,
                botId: agentId,
                runId: run.id,
              })
          }
        } while (request.dirty)
      } catch (failure) {
        setError((failure as Error).message)
      } finally {
        refreshing.current.delete(agentId)
      }
    },
    [backend, clearActivity, receiveActivity],
  )

  useEffect(() => {
    if (!ready || isBlocked || !backend.loadAgentConversation) return
    // 快速切换会话时取消尚未发出的首屏请求。
    const initialLoad = setTimeout(() => {
      void refreshConversation(workspace.activeAgentId)
    }, 0)
    // 断线与服务重启后仍定期核对权威快照，事件用于缩短正常更新延迟。
    const timer = setInterval(() => {
      void refreshConversation(current.current.activeAgentId)
    }, 10_000)
    return () => {
      clearTimeout(initialLoad)
      clearInterval(timer)
    }
  }, [backend, ready, isBlocked, workspace.activeAgentId, refreshConversation])

  useEffect(() => {
    if (!ready || isBlocked || !backend.loadAgentConversation) return
    return backend.watchEvents?.(
      (event) => {
        const botId = typeof event.payload.bot_id === 'string' ? event.payload.bot_id : ''
        if (botId && event.kind.startsWith('run.')) {
          const source = `server:${botId}`
          receiveActivity?.(source, {
            type: 'activity',
            text: '',
            botId,
            runId: event.resource_id,
            isWorking: event.kind !== 'run.finished',
            isFinished: event.kind === 'run.finished',
          })
          if (event.kind === 'run.progress' && typeof event.payload.text === 'string') {
            receiveActivity?.(source, {
              type: 'process',
              text: event.payload.text,
              botId,
              runId: event.resource_id,
            })
          }
          if (event.kind === 'run.finished' && event.payload.status !== 'succeeded') {
            setError('消息已接收，但本次任务未完成。请重试或联系管理员查看运行日志。')
          }
        }
        if (
          event.kind.startsWith('run.') ||
          event.kind === 'message.created' ||
          event.kind === 'workspace.refresh'
        ) {
          void refreshConversation(current.current.activeAgentId)
        }
      },
      (failure) => setError(failure.message),
    )
  }, [backend, ready, isBlocked, receiveActivity, refreshConversation])

  async function loadEarlier(agentId: string): Promise<void> {
    const pageState = historyPages[agentId]
    if (
      !backend.loadAgentConversation ||
      !pageState?.hasMore ||
      !pageState.before ||
      paging.current.has(agentId)
    )
      return
    paging.current.add(agentId)
    setHistoryPages((previous) => ({ ...previous, [agentId]: { ...pageState, loading: true } }))
    try {
      const page = await backend.loadAgentConversation(agentId, pageState.before)
      current.current = mergeIncoming(current.current, [
        { id: agentId, messages: page.messages.map(toChatMessage) },
      ])
      setWorkspace(current.current)
      setHistoryPages((previous) => ({
        ...previous,
        [agentId]: {
          before: page.messages[0]?.sequence ?? pageState.before,
          hasMore: page.hasMore,
          loading: false,
        },
      }))
    } catch (failure) {
      setError((failure as Error).message)
      setHistoryPages((previous) => ({ ...previous, [agentId]: { ...pageState, loading: false } }))
    } finally {
      paging.current.delete(agentId)
    }
  }

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
    void enqueue(flushLatest).catch((failure: Error) => setError(failure.message))
  }

  /** 等待服务端创建成功，之后才加入侧栏；本地校验不能代替服务端创建。 */
  function create(profile: NewAgent, requestId: string): Promise<string> {
    return enqueue(async () => {
      if (blocked.current) throw new Error(error || initial.error)
      // 只校验输入与本机容量，绝不把请求标识当成机器人标识写入工作空间。
      addAgent(current.current, profile, requestId)
      const agent = await backend.createAgent(profile, requestId)
      const select = (previous: AppWorkspace, created: Agent): AppWorkspace => ({
        ...previous,
        activeAgentId: created.id,
        leadAgentId: previous.leadAgentId || created.id,
        agents: [...previous.agents.filter((item) => item.id !== created.id), created],
      })
      await backend.saveWorkspace(select(current.current, agent))
      // 等待期间其他会话仍可能收到消息或编辑草稿，因此在最新快照上合并。
      current.current = select(current.current, agent)
      setWorkspace(current.current)
      return agent.id
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
   * 受理后按消息标识合并，保留同时新增的草稿和待发消息。
   */
  function deliver(
    agentId: string,
    content: string,
    clientRequestId: string,
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
        const result = await backend.deliverMessage(agentId, content, clientRequestId, receiveEvent)
        if (result.acceptedMessageId || result.messages) {
          const acceptedId = result.acceptedMessageId
          current.current = {
            ...current.current,
            agents: current.current.agents.map((agent) => {
              if (agent.id !== agentId) return agent
              const alreadyReceived =
                acceptedId && agent.messages.some((message) => message.id === acceptedId)
              return {
                ...agent,
                messages: agent.messages.flatMap((message) => {
                  if (message.id !== clientRequestId) return [message]
                  if (alreadyReceived || result.messages) return []
                  return [{ ...message, id: acceptedId!, status: 'sent' as const }]
                }),
              }
            }),
          }
          current.current = mergeIncoming(current.current, [
            { id: agentId, messages: (result.messages ?? []).map(toChatMessage) },
          ])
          setWorkspace(current.current)
          if (backend.savesSynchronously) backend.saveWorkspace(current.current)
          void refreshConversation(agentId)
        }
        return result
      } catch (failure) {
        return { code: 'DELIVER_FAILED', message: (failure as Error).message }
      }
    })
  }

  return {
    workspace,
    error,
    isBlocked,
    ready,
    commit,
    create,
    deliver,
    receive,
    historyPages,
    loadEarlier,
    hasUnsavedChanges: saveState === 'error',
    dismissError: () => {
      if (saveState !== 'error') setError('')
    },
  }
}
