import { contextBridge, ipcRenderer, type IpcRendererEvent } from 'electron'

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
      setup: (input: { setupCode: string; password: string }) => invoke('desktop:setup', input),
      login: (password: string) => invoke('desktop:login', password),
      loadWorkspace: () => invoke('desktop:load-workspace'),
      saveWorkspace: (workspace: unknown) => invoke('desktop:save-workspace', workspace),
      importWorkspace: (workspace: unknown) => invoke('desktop:import-workspace', workspace),
      deliverMessage: (
        agentId: string,
        messageId: string,
        onEvent?: (event: { type: string; text: string }) => void,
      ) => {
        const listener = (_event: IpcRendererEvent, payload: { type: string; text: string }) =>
          onEvent?.(payload)
        ipcRenderer.on('desktop:stream-event', listener)
        return invoke('desktop:deliver', { agentId, messageId }).finally(() => {
          ipcRenderer.removeListener('desktop:stream-event', listener)
        })
      },
      openComputer: (agentId: string) => invoke<string>('desktop:computer', agentId),
      startBootstrap: () => invoke('desktop:bootstrap'),
      listMemories: (agentId: string) => invoke('desktop:memories', agentId),
      createMemory: (agentId: string, title: string, content: string) =>
        invoke('desktop:memory-create', { agentId, title, content }),
      deleteMemory: (agentId: string, path: string, sha256: string) =>
        invoke('desktop:memory-delete', { agentId, path, sha256 }),
      listRoutines: (agentId: string) => invoke('desktop:routines', agentId),
      createRoutine: (agentId: string, name: string, prompt: string) =>
        invoke('desktop:routine-create', { agentId, name, prompt }),
      setRoutineEnabled: (id: string, revision: number, enabled: boolean) =>
        invoke('desktop:routine-enabled', { id, revision, enabled }),
      deleteRoutine: (id: string, revision: number) =>
        invoke('desktop:routine-delete', { id, revision }),
      loadSettings: () => invoke('desktop:load-settings'),
      saveSettings: (draft: unknown, apiKey: string) =>
        invoke('desktop:save-settings', { draft, apiKey }),
    }),
  }),
)
