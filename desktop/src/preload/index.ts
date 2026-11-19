import type {
  SessionSnapshot,
  StreamEvent,
  UserEvent,
  WorkspaceLoadFailure,
  WorkspaceSnapshot,
} from '../renderer/src/features/desktop/backend'
import { contextBridge, ipcRenderer, type IpcRendererEvent } from 'electron'
import type { NewAgent } from '../renderer/src/features/workspace/model'
import type { DesktopConsole, ConsoleStreamEvent } from '../shared/console'

interface BootstrapStatus {
  phase: string
  steps: Array<{ id: string; title: string; status: string; detail?: string; duration_ms: number }>
  error?: string
  cursor: number
}

type CallResult<T> = { ok: true; value: T } | { ok: false; code: string; message: string }

async function invoke<T>(channel: string, ...args: unknown[]): Promise<T> {
  const result = (await ipcRenderer.invoke(channel, ...args)) as CallResult<T>
  if (!result.ok) {
    const error = new Error(result.message)
    ;(error as Error & { code: string }).code = result.code
    throw error
  }
  return result.value
}

type EventSubscriber = { onEvent: (event: UserEvent) => void; onError: (error: Error) => void }

const eventSubscribers = new Set<EventSubscriber>()
let activeEventWatchId: string | undefined
let wasSessionOffline = false

function receiveUserEvent(_event: IpcRendererEvent, payload: UserEvent, watchId: string): void {
  if (watchId !== activeEventWatchId) return
  for (const subscriber of [...eventSubscribers]) subscriber.onEvent(payload)
}

function clearEventWatch(): void {
  activeEventWatchId = undefined
  ipcRenderer.removeListener('desktop:user-event', receiveUserEvent)
}

function cancelEventWatch(): void {
  const watchId = activeEventWatchId
  if (!watchId) return
  clearEventWatch()
  ipcRenderer.send('desktop:unwatch-events', watchId)
}

/** 同一窗口共用事件流，避免并发连接各自推进同一个事件游标。 */
function startEventWatch(needsRefresh = false): void {
  if (activeEventWatchId || !eventSubscribers.size) return
  const watchId = crypto.randomUUID()
  activeEventWatchId = watchId
  ipcRenderer.on('desktop:user-event', receiveUserEvent)
  void invoke('desktop:watch-events', watchId, needsRefresh).then(
    () => {
      if (activeEventWatchId === watchId) clearEventWatch()
    },
    (error: Error) => {
      if (activeEventWatchId !== watchId) return
      clearEventWatch()
      // 断线保留业务监听，等会话探测确认服务可达再恢复事件流。
      for (const subscriber of [...eventSubscribers]) subscriber.onError(error)
    },
  )
}

/** 返回本地监听的取消函数，最后一个监听取消时才终止主进程订阅。 */
function watchEvents(onEvent: EventSubscriber['onEvent'], onError: EventSubscriber['onError']) {
  const subscriber = { onEvent, onError }
  eventSubscribers.add(subscriber)
  if (eventSubscribers.size === 1) startEventWatch()
  return () => {
    if (eventSubscribers.delete(subscriber) && eventSubscribers.size === 0) cancelEventWatch()
  }
}

/** 会话恢复后替换可能尚未退出的旧连接，不等旧连接超时。 */
async function inspectSession(): Promise<SessionSnapshot> {
  const session = await invoke<SessionSnapshot>('desktop:session')
  if (session.phase === 'offline') wasSessionOffline = true
  if (session.phase === 'ready') {
    const needsRefresh = wasSessionOffline
    wasSessionOffline = false
    if (needsRefresh) cancelEventWatch()
    startEventWatch(needsRefresh)
  }
  return session
}

// 只暴露平台标识和桌面服务通道。页面拿不到令牌，也不能发任意请求。
contextBridge.exposeInMainWorld(
  'desktop',
  Object.freeze({
    platform: process.platform,
    backend: Object.freeze({
      session: inspectSession,
      loadUserProfile: () => invoke('desktop:user-profile'),
      watchWindowFocus: (onFocus: () => void) => {
        const listener = () => onFocus()
        ipcRenderer.on('desktop:window-focus', listener)
        return () => ipcRenderer.removeListener('desktop:window-focus', listener)
      },
      connect: (serverUrl: string) => invoke('desktop:connect', serverUrl),
      browserLogin: async (): Promise<SessionSnapshot> => {
        // contextBridge 传递异常时不保留自定义 code；状态必须用普通对象跨越隔离边界。
        const result = (await ipcRenderer.invoke(
          'desktop:browser-login',
        )) as CallResult<SessionSnapshot>
        if (result.ok) return result.value
        return {
          phase: result.code === 'SERVER_URL_MISSING' ? 'address' : 'login',
          errorCode: result.code,
          message: result.message,
        }
      },
      trustServerCertificate: (address: string, fingerprint: string) =>
        invoke('desktop:trust-server-certificate', address, fingerprint),
      logout: () => invoke('desktop:logout'),
      loadWorkspace: async (): Promise<WorkspaceSnapshot | WorkspaceLoadFailure> => {
        // 保留错误分类，以便工作空间只在暂时断线后重试。
        const result = (await ipcRenderer.invoke(
          'desktop:load-workspace',
        )) as CallResult<WorkspaceSnapshot>
        return result.ok ? result.value : { errorCode: result.code, message: result.message }
      },
      checkClientUpdate: () => invoke('desktop:check-update'),
      installClientUpdate: async (id: string, onProgress: (message: string) => void) => {
        const progress = (_event: Electron.IpcRendererEvent, message: string) => onProgress(message)
        ipcRenderer.on('desktop:update-progress', progress)
        try {
          return await invoke('desktop:install-update', id)
        } finally {
          ipcRenderer.removeListener('desktop:update-progress', progress)
        }
      },
      downloadClientUpdate: (id: string) => invoke('desktop:download-update', id),
      watchUpdateRequest: (onRequest: () => void) => {
        const listener = () => onRequest()
        ipcRenderer.on('desktop:update-request', listener)
        return () => ipcRenderer.removeListener('desktop:update-request', listener)
      },
      loadAbout: () => invoke('desktop:about'),
      loadConversationPreviews: (ids: string[]) => invoke('desktop:conversation-previews', ids),
      loadAgentConversation: (agentId: string, before?: number) =>
        invoke('desktop:agent-conversation', agentId, before),
      createAgent: (profile: NewAgent, requestId: string) =>
        invoke('desktop:create-agent', profile, requestId),
      readClipboardFiles: () => invoke('desktop:clipboard-files'),
      saveWorkspace: (workspace: unknown) => invoke('desktop:save-workspace', workspace),
      watchEvents,
      deliverMessage: (
        agentId: string,
        content: string,
        clientRequestId: string,
        onEvent?: (event: StreamEvent) => void,
        replyToId?: string,
        attachments?: Array<{ path: string; sha256: string }>,
      ) => {
        const requestId = crypto.randomUUID()
        const listener = (
          _event: IpcRendererEvent,
          payload: StreamEvent & { requestId: string },
        ) => {
          if (payload.requestId === requestId) onEvent?.(payload)
        }
        ipcRenderer.on('desktop:stream-event', listener)
        return invoke('desktop:deliver', {
          agentId,
          content,
          requestId,
          clientRequestId,
          replyToId,
          attachments,
        }).finally(() => {
          ipcRenderer.removeListener('desktop:stream-event', listener)
        })
      },
      openConsole: (botId: string, requestId: string) =>
        invoke<DesktopConsole>('desktop:console-open', botId, requestId),
      consoleStatus: (id: string) => invoke<DesktopConsole>('desktop:console-status', id),
      closeConsole: (id: string) => invoke<DesktopConsole>('desktop:console-close', id),
      controlConsole: (id: string, takeover: boolean) =>
        invoke<DesktopConsole>('desktop:console-control', id, takeover),
      connectConsole: (
        id: string,
        streamId: string,
        receive: (event: ConsoleStreamEvent) => void,
      ) => {
        const listener = (
          _event: IpcRendererEvent,
          incoming: string,
          message: ConsoleStreamEvent,
        ) => {
          if (incoming !== streamId) return
          receive(message)
          if (message.kind === 'bytes')
            ipcRenderer.send('desktop:console-stream-ack', streamId, message.bytes.byteLength)
        }
        ipcRenderer.on('desktop:console-stream', listener)
        ipcRenderer.send('desktop:console-stream-open', id, streamId)
        return {
          send: (bytes: Uint8Array) =>
            ipcRenderer.send('desktop:console-stream-write', streamId, bytes),
          close: () => {
            ipcRenderer.removeListener('desktop:console-stream', listener)
            ipcRenderer.send('desktop:console-stream-close', streamId)
          },
        }
      },
      startBootstrap: () => invoke('desktop:bootstrap'),
      watchBootstrap: (
        onStatus: (status: BootstrapStatus) => void,
        onError: (error: Error) => void,
      ) => {
        const watchID = crypto.randomUUID()
        const listener = (_event: IpcRendererEvent, payload: BootstrapStatus) => onStatus(payload)
        ipcRenderer.on('desktop:bootstrap-status', listener)
        void invoke('desktop:bootstrap-watch', watchID).catch((error: Error) => onError(error))
        return () => {
          ipcRenderer.send('desktop:bootstrap-unwatch', watchID)
          ipcRenderer.removeListener('desktop:bootstrap-status', listener)
        }
      },
      computerStatus: () =>
        invoke<import('../shared/computer').ComputerStatus>('desktop:computer-status'),
      restartComputer: (version: string) =>
        invoke<import('../shared/computer').ComputerStatus>('desktop:computer-restart', version),
      listTasks: (agentId: string) => invoke('desktop:tasks', agentId),
      setTaskEnabled: (agentId: string, id: string, revision: number, enabled: boolean) =>
        invoke('desktop:task-enabled', { agentId, id, revision, enabled }),
      deleteTask: (agentId: string, id: string, revision: number) =>
        invoke('desktop:task-delete', { agentId, id, revision }),
      listGroups: () => invoke('desktop:groups'),
      createGroup: (name: string, memberIds: string[], requestId: string) =>
        invoke('desktop:group-create', { name, memberIds, requestId }),
      listChatFiles: (id: string, before?: string, query?: string) =>
        invoke('desktop:chat-files', { id, before, query }),
      uploadChatFile: (id: string, file: { name: string; bytes: Uint8Array }, requestId: string) =>
        invoke('desktop:chat-file-upload', { id, file, requestId }),
      previewChatFile: (id: string, fileId: string) =>
        invoke('desktop:chat-file-preview', { id, fileId }),
      downloadChatFile: (id: string, fileId: string) =>
        invoke('desktop:chat-file-download', { id, fileId }),
      renameChatFile: (id: string, fileId: string, revision: number, name: string) =>
        invoke('desktop:chat-file-rename', { id, fileId, revision, name }),
      deleteChatFile: (id: string, fileId: string, revision: number) =>
        invoke('desktop:chat-file-delete', { id, fileId, revision }),
      groupDetail: (id: string) => invoke('desktop:group', id),
      deleteGroup: (id: string, revision: number) =>
        invoke('desktop:group-delete', { id, revision }),
      loadBotActivities: () => invoke('desktop:bot-activities'),
      conversationActivity: (id: string) => invoke('desktop:conversation-activity', id),
      renameGroup: (id: string, revision: number, name: string) =>
        invoke('desktop:group-rename', { id, revision, name }),
      addGroupMembers: (id: string, revision: number, botIds: string[]) =>
        invoke('desktop:group-members', { id, revision, botIds }),
      removeGroupMember: (id: string, revision: number, botId: string) =>
        invoke('desktop:group-member-remove', { id, revision, botId }),
      listConversationMessages: (conversationId: string, before?: number) =>
        invoke('desktop:conversation-messages', { conversationId, before }),
      sendConversation: (
        conversationId: string,
        content: string,
        attachments: Array<{ path: string; sha256: string }>,
        replyToId: string | undefined,
        mentions: string[],
        requestId: string,
      ) =>
        invoke('desktop:conversation-send', {
          requestId,
          conversationId,
          content,
          attachments,
          replyToId,
          mentions,
        }),
      uploadAttachment: (
        file: { name: string; type: string; bytes: Uint8Array; requestId?: string },
        destination: { botId?: string; shared?: boolean; groupId?: string },
      ) => invoke('desktop:upload', { ...file, ...destination }),
      previewAttachment: (path: string, botId?: string) =>
        invoke('desktop:preview', { path, botId }),
      setReaction: (messageId: string, emoji: string, isActive: boolean, clientRequestId: string) =>
        invoke('desktop:reaction', { messageId, emoji, isActive, clientRequestId }),
      ensureAgent: (agentId: string) => invoke('desktop:ensure-agent', agentId),
    }),
  }),
)
