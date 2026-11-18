import { useCallback, useEffect, useRef, useState } from 'react'
import {
  getBackend,
  type DeliveryResult,
  type DesktopBackend,
  type IncomingMessages,
  type ServerBubble,
  type StreamEvent,
} from '../desktop/backend'
import { isTransientSessionError } from '../session/sessionProbe'
import {
  addAgent,
  emptyWorkspace,
  type Agent,
  type NewAgent,
  type AppWorkspace,
  type ChatMessage,
} from './model'
import { isReactionEmoji } from './reactions'
import { mergeMessages } from './messageTimeline'
import { ulid } from '../../../../shared/ulid'

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
    ...(bubble.requestId ? { requestId: bubble.requestId } : {}),
    sequence: bubble.sequence,
    role: bubble.role,
    content: bubble.content,
    createdAt: bubble.createdAt,
    status: 'sent',
    replyTo: bubble.replyTo,
    dmNotice: bubble.dmNotice,
    reactionRevision: bubble.reactionRevision,
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
  const agents = local.agents.map((agent) => {
    const received = incoming.find((item) => item.id === agent.id)
    if (!received) return agent
    const messages = mergeMessages(agent.messages, received.messages)
    return messages === agent.messages ? agent : { ...agent, messages }
  })
  return agents.every((agent, index) => agent === local.agents[index])
    ? local
    : { ...local, agents }
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
  const retryInitialLoad = useRef<(() => Promise<void>) | undefined>(undefined)
  const chain = useRef(Promise.resolve())
  const pendingTasks = useRef(0)
  const reactionChain = useRef(Promise.resolve())
  const reactionRequests = useRef(new Map<string, { clientRequestId: string; isActive: boolean }>())

  useEffect(() => {
    document.documentElement.dataset.workspaceSave = saveState
  }, [saveState])

  useEffect(() => {
    if (initial.ready) return
    let cancelled = false
    let isLoading = false

    function fail(message: string, errorCode?: string) {
      if (cancelled) return
      retryInitialLoad.current = isTransientSessionError(errorCode) ? load : undefined
      blocked.current = true
      setBlocked(true)
      setError(message)
      setReady(true)
    }

    async function load(): Promise<void> {
      if (cancelled || isLoading) return
      isLoading = true
      try {
        const snapshot = await backend.loadWorkspace()
        if (cancelled) return
        if ('errorCode' in snapshot) {
          fail(snapshot.message, snapshot.errorCode)
          return
        }
        retryInitialLoad.current = undefined
        current.current = snapshot.workspace
        blocked.current = false
        setWorkspace(snapshot.workspace)
        setBlocked(false)
        setError('')
        setReady(true)
      } catch (failure) {
        fail(failure instanceof Error ? failure.message : '无法读取工作空间。')
      } finally {
        isLoading = false
      }
    }

    void load()
    return () => {
      cancelled = true
      retryInitialLoad.current = undefined
    }
  }, [backend, initial.ready])

  const [historyPages, setHistoryPages] = useState<
    Record<string, { before: number; hasMore: boolean; loading: boolean }>
  >({})
  const paging = useRef(new Set<string>())
  const refreshing = useRef(new Map<string, { dirty: boolean }>())
  const activityRevisions = useRef(new Map<string, number>())
  const activitySequences = useRef(new Map<string, number>())
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
          const activityRevision = activityRevisions.current.get(agentId)
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
          // 请求期间到达的运行事件更新，旧快照不能把刚开始的动画关闭或把已结束的任务重新点亮。
          if (activityRevision !== activityRevisions.current.get(agentId)) continue
          activityRevisions.current.set(agentId, (activityRevision ?? 0) + 1)
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
        if (botId && /^(?:private\.|group\.|task\.)?run\./.test(event.kind)) {
          // 断流重播或乱序事件不能复活已经结束的运行。
          if (event.sequence <= (activitySequences.current.get(botId) ?? -1)) return
          activitySequences.current.set(botId, event.sequence)
          activityRevisions.current.set(botId, (activityRevisions.current.get(botId) ?? 0) + 1)
          const source = `server:${botId}`
          receiveActivity?.(source, {
            type: 'activity',
            text: '',
            botId,
            runId: event.resource_id,
            isWorking: !event.kind.endsWith('run.finished'),
            isFinished: event.kind.endsWith('run.finished'),
          })
          if (event.kind.endsWith('run.progress') && typeof event.payload.text === 'string') {
            receiveActivity?.(source, {
              type: 'process',
              text: event.payload.text,
              botId,
              runId: event.resource_id,
            })
          }
          if (event.kind.endsWith('run.finished') && event.payload.status !== 'succeeded') {
            setError('消息已接收，但本次任务未完成。请重试或联系管理员查看运行日志。')
          }
        }
        if (
          event.kind.endsWith('run.finished') ||
          event.kind === 'message.created' ||
          event.kind === 'message.reactions_changed' ||
          event.kind === 'workspace.refresh'
        ) {
          void refreshConversation(botId || current.current.activeAgentId)
        }
      },
      (failure) => setError(failure.message),
    )
  }, [backend, ready, isBlocked, receiveActivity, refreshConversation])

  useEffect(() => {
    if (!ready || isBlocked || !backend.loadBotActivities) return
    let cancelled = false
    let refreshing = false
    async function refreshActivities() {
      if (refreshing) return
      refreshing = true
      const revisions = new Map(activityRevisions.current)
      try {
        const { runs } = await backend.loadBotActivities!()
        if (cancelled) return
        for (const agent of current.current.agents) {
          if (revisions.get(agent.id) !== activityRevisions.current.get(agent.id)) continue
          activityRevisions.current.set(agent.id, (revisions.get(agent.id) ?? 0) + 1)
          const source = `server:${agent.id}`
          clearActivity?.(source)
          for (const run of runs.filter((run) => run.bot_id === agent.id)) {
            receiveActivity?.(source, {
              type: 'activity',
              text: '',
              botId: agent.id,
              runId: run.id,
              isWorking: true,
            })
            if (run.progress_summary)
              receiveActivity?.(source, {
                type: 'process',
                text: run.progress_summary,
                botId: agent.id,
                runId: run.id,
              })
          }
        }
      } catch (failure) {
        if (!cancelled) setError((failure as Error).message)
      } finally {
        refreshing = false
      }
    }
    void refreshActivities()
    const timer = setInterval(() => void refreshActivities(), 10_000)
    const unwatch = backend.watchEvents?.(
      (event) => {
        if (event.kind === 'workspace.refresh' || event.kind.endsWith('run.finished'))
          void refreshActivities()
      },
      (failure) => setError(failure.message),
    )
    const focus = () => void refreshActivities()
    window.addEventListener('focus', focus)
    return () => {
      cancelled = true
      clearInterval(timer)
      unwatch?.()
      window.removeEventListener('focus', focus)
    }
  }, [backend, ready, isBlocked, clearActivity, receiveActivity])

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

  /** 表情只在服务确认后更新；失败保留请求标识供显式重试，不能将网络失败当成保存失败。 */
  function react(agentId: string, messageId: string, emoji: string): Promise<void> {
    const task = async () => {
      if (!isReactionEmoji(emoji)) throw new Error('不支持这个表情')
      const target = current.current.agents
        .find((agent) => agent.id === agentId)
        ?.messages.find((message) => message.id === messageId || message.requestId === messageId)
      if (!target) throw new Error('消息不存在')
      if (target.status !== 'sent') throw new Error('请等待消息发送成功后再添加表情。')
      const key = JSON.stringify([target.id, emoji])
      const isActive = !target.reactions.some((reaction) => reaction.emoji === emoji)
      const previous = reactionRequests.current.get(key)
      if (previous && previous.isActive !== isActive) {
        // 推送已确认旧操作但回执丢失时，先核实旧请求，再执行用户当前看到的取消或添加。
        const confirmed = await backend.setReaction(
          target.id,
          emoji,
          previous.isActive,
          previous.clientRequestId,
        )
        reactionRequests.current.delete(key)
        receive([{ id: agentId, messages: [toChatMessage(confirmed)] }])
      }
      const request = reactionRequests.current.get(key) ?? {
        clientRequestId: ulid(),
        isActive,
      }
      reactionRequests.current.set(key, request)
      const message = await backend.setReaction(
        target.id,
        emoji,
        request.isActive,
        request.clientRequestId,
      )
      reactionRequests.current.delete(key)
      receive([{ id: agentId, messages: [toChatMessage(message)] }])
    }
    const pending = reactionChain.current.then(task, task)
    // 队列只负责恢复后续操作；当前错误由返回的 Promise 交给界面提示。
    reactionChain.current = pending.then(
      () => undefined,
      () => undefined,
    )
    return pending
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
        const messages =
          current.current.agents.find((agent) => agent.id === agentId)?.messages ?? []
        const outgoing = messages.find(
          (message) => (message.requestId ?? message.id) === clientRequestId,
        )
        const replyToId = outgoing?.replyTo?.messageId
        const target = messages.find((message) => message.id === replyToId)
        if (target && target.status !== 'sent') throw new Error('请等待原消息发送成功后再回复。')
        const result = await backend.deliverMessage(
          agentId,
          content,
          clientRequestId,
          receiveEvent,
          replyToId,
          outgoing?.attachments,
        )
        if (result.acceptedMessageId || result.messages) {
          const acceptedId = result.acceptedMessageId
          const local = current.current.agents.find((agent) => agent.id === agentId)?.messages
          const pending = local?.find(
            (message) => (message.requestId ?? message.id) === clientRequestId,
          )
          const confirmed = local?.find((message) => message.id === acceptedId)
          const accepted: ChatMessage[] =
            acceptedId && pending
              ? [
                  {
                    ...pending,
                    ...confirmed,
                    id: acceptedId,
                    requestId: clientRequestId,
                    status: 'sent',
                  },
                ]
              : []
          current.current = mergeIncoming(current.current, [
            { id: agentId, messages: [...accepted, ...(result.messages ?? []).map(toChatMessage)] },
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
    retryLoadingWorkspace: () => void retryInitialLoad.current?.(),
    commit,
    create,
    deliver,
    receive,
    react,
    historyPages,
    loadEarlier,
    hasUnsavedChanges: saveState === 'error',
    dismissError: () => {
      if (saveState !== 'error') setError('')
    },
  }
}
