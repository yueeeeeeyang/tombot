import { contextBridge, ipcRenderer, type IpcRendererEvent } from 'electron'

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
      connect: (serverUrl: string) => invoke('desktop:connect', serverUrl),
      setup: (input: { setupCode: string; password: string }) => invoke('desktop:setup', input),
      login: (password: string) => invoke('desktop:login', password),
      loadWorkspace: () => invoke('desktop:load-workspace'),
      saveWorkspace: (workspace: unknown) => invoke('desktop:save-workspace', workspace),
      deliverMessage: (
        agentId: string,
        messageId: string,
        onEvent?: (event: {
          type: string
          text: string
          messageId?: string
          peerId?: string
          peerName?: string
          conversationId?: string
          direction?: 'sent' | 'received'
        }) => void,
      ) => {
        const listener = (
          _event: IpcRendererEvent,
          payload: {
            type: string
            text: string
            messageId?: string
            peerId?: string
            peerName?: string
            conversationId?: string
            direction?: 'sent' | 'received'
          },
        ) => onEvent?.(payload)
        ipcRenderer.on('desktop:stream-event', listener)
        return invoke('desktop:deliver', { agentId, messageId }).finally(() => {
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
      listMemories: (agentId: string) => invoke('desktop:memories', agentId),
      listRoutines: (agentId: string) => invoke('desktop:routines', agentId),
      setRoutineEnabled: (id: string, revision: number, enabled: boolean) =>
        invoke('desktop:routine-enabled', { id, revision, enabled }),
      deleteRoutine: (id: string, revision: number) =>
        invoke('desktop:routine-delete', { id, revision }),
      listGroups: () => invoke('desktop:groups'),
      createGroup: (name: string, memberIds: string[]) =>
        invoke('desktop:group-create', { name, memberIds }),
      groupDetail: (id: string) => invoke('desktop:group', id),
      addGroupMembers: (id: string, revision: number, botIds: string[]) =>
        invoke('desktop:group-members', { id, revision, botIds }),
      removeGroupMember: (id: string, revision: number, botId: string) =>
        invoke('desktop:group-member-remove', { id, revision, botId }),
      listConversationMessages: (conversationId: string) =>
        invoke('desktop:conversation-messages', conversationId),
      sendConversation: (
        conversationId: string,
        content: string,
        attachments: Array<{ path: string; sha256: string }>,
        replyToId: string | undefined,
        onEvent?: (event: { type: string; text: string }) => void,
      ) => {
        const listener = (_event: IpcRendererEvent, payload: { type: string; text: string }) =>
          onEvent?.(payload)
        ipcRenderer.on('desktop:stream-event', listener)
        return invoke('desktop:conversation-send', {
          conversationId,
          content,
          attachments,
          replyToId,
        }).finally(() => ipcRenderer.removeListener('desktop:stream-event', listener))
      },
      uploadAttachment: (
        file: { name: string; type: string; bytes: Uint8Array },
        destination: { botId?: string; shared?: boolean },
      ) => invoke('desktop:upload', { ...file, ...destination }),
      previewAttachment: (path: string, botId?: string) =>
        invoke('desktop:preview', { path, botId }),
      toggleReaction: (messageId: string, emoji: string) =>
        invoke('desktop:reaction', { messageId, emoji }),
      ensureAgent: (agentId: string) => invoke('desktop:ensure-agent', agentId),
      loadSettings: () => invoke('desktop:load-settings'),
      saveSettings: (draft: unknown, apiKey: string) =>
        invoke('desktop:save-settings', { draft, apiKey }),
    }),
  }),
)
