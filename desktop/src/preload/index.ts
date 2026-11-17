import type {
  SessionSnapshot,
  StreamEvent,
  UserEvent,
} from '../renderer/src/features/desktop/backend'
import { contextBridge, ipcRenderer, type IpcRendererEvent } from 'electron'
import type { NewAgent } from '../renderer/src/features/workspace/model'

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

// 只暴露平台标识和桌面服务通道。页面拿不到令牌，也不能发任意请求。
contextBridge.exposeInMainWorld(
  'desktop',
  Object.freeze({
    platform: process.platform,
    backend: Object.freeze({
      session: () => invoke('desktop:session'),
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
      loadWorkspace: () => invoke('desktop:load-workspace'),
      loadAgentConversation: (agentId: string, before?: number) =>
        invoke('desktop:agent-conversation', agentId, before),
      createAgent: (profile: NewAgent, requestId: string) =>
        invoke('desktop:create-agent', profile, requestId),
      readClipboardFiles: () => invoke('desktop:clipboard-files'),
      saveWorkspace: (workspace: unknown) => invoke('desktop:save-workspace', workspace),
      watchEvents: (onEvent: (event: UserEvent) => void, onError: (error: Error) => void) => {
        const listener = (_event: IpcRendererEvent, payload: UserEvent) => onEvent(payload)
        ipcRenderer.on('desktop:user-event', listener)
        let stopped = false
        const watchId = crypto.randomUUID()
        void invoke('desktop:watch-events', watchId).catch((error: Error) => {
          // 订阅被动结束后不再回调；主动停止不算错误。
          if (!stopped) onError(error)
        })
        return () => {
          stopped = true
          ipcRenderer.send('desktop:unwatch-events', watchId)
          ipcRenderer.removeListener('desktop:user-event', listener)
        }
      },
      deliverMessage: (
        agentId: string,
        content: string,
        clientRequestId: string,
        onEvent?: (event: StreamEvent) => void,
        replyToId?: string,
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
        }).finally(() => {
          ipcRenderer.removeListener('desktop:stream-event', listener)
        })
      },
      openComputer: (agentId: string) => invoke<string>('desktop:computer', agentId),
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
      listTasks: (agentId: string) => invoke('desktop:tasks', agentId),
      setTaskEnabled: (agentId: string, id: string, revision: number, enabled: boolean) =>
        invoke('desktop:task-enabled', { agentId, id, revision, enabled }),
      deleteTask: (agentId: string, id: string, revision: number) =>
        invoke('desktop:task-delete', { agentId, id, revision }),
      listGroups: () => invoke('desktop:groups'),
      createGroup: (name: string, memberIds: string[], requestId: string) =>
        invoke('desktop:group-create', { name, memberIds, requestId }),
      listGroupFiles: (id: string, before?: string, query?: string) =>
        invoke('desktop:group-files', { id, before, query }),
      uploadGroupFile: (id: string, file: { name: string; bytes: Uint8Array }, requestId: string) =>
        invoke('desktop:group-file-upload', { id, file, requestId }),
      previewGroupFile: (id: string, fileId: string) =>
        invoke('desktop:group-file-preview', { id, fileId }),
      downloadGroupFile: (id: string, fileId: string) =>
        invoke('desktop:group-file-download', { id, fileId }),
      renameGroupFile: (id: string, fileId: string, revision: number, name: string) =>
        invoke('desktop:group-file-rename', { id, fileId, revision, name }),
      deleteGroupFile: (id: string, fileId: string, revision: number) =>
        invoke('desktop:group-file-delete', { id, fileId, revision }),
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
