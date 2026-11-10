import type { SettingsDraft } from '../settings/settings'
import type { AppWorkspace } from '../workspace/model'

export type SessionPhase = 'loading' | 'offline' | 'setup' | 'login' | 'ready'

export interface SessionSnapshot {
  phase: SessionPhase
  message?: string
}

export interface WorkspaceSnapshot {
  workspace: AppWorkspace
  pristine: boolean
}

export interface SettingsView extends SettingsDraft {
  hasApiKey: boolean
}

/** 投递结果。模型未配置时不改消息，只带回说明。 */
export interface DeliveryResult {
  workspace?: AppWorkspace
  code?: string
  message?: string
}

/**
 * 桌面与服务之间的唯一通道。
 * 测试用的内存实现会同步保存；真正的服务调用返回 Promise。
 */
export interface DesktopBackend {
  savesSynchronously?: boolean
  trySessionSync?: () => SessionPhase
  session: () => Promise<SessionSnapshot>
  setup: (input: { setupCode: string; password: string }) => Promise<void>
  login: (password: string) => Promise<void>
  tryWorkspaceSync?: () => WorkspaceSnapshot
  loadWorkspace: () => Promise<WorkspaceSnapshot>
  saveWorkspace: (workspace: AppWorkspace) => void | Promise<void>
  importWorkspace: (workspace: AppWorkspace) => AppWorkspace | Promise<AppWorkspace>
  deliverMessage: (
    agentId: string,
    messageId: string,
    onEvent?: (event: { type: string; text: string }) => void,
  ) => Promise<DeliveryResult>
  openComputer?: (agentId: string) => Promise<string>
  listMemories?: (
    agentId: string,
  ) => Promise<Array<{ path: string; title: string; summary: string; sha256: string }>>
  listRoutines?: (agentId: string) => Promise<
    Array<{
      id: string
      name: string
      cron: string
      prompt: string
      is_enabled: boolean
      revision: number
    }>
  >
  setRoutineEnabled?: (id: string, revision: number, enabled: boolean) => Promise<void>
  deleteRoutine?: (id: string, revision: number) => Promise<void>
  startBootstrap?: () => Promise<void>
  trySettingsSync?: () => SettingsView
  loadSettings: () => Promise<SettingsView>
  saveSettings: (draft: SettingsDraft, apiKey: string) => SettingsView | Promise<SettingsView>
}

/** 读取当前页面的服务通道。没有桥接时直接失败，不用本地数据假装已连接。 */
export function getBackend(): DesktopBackend {
  const backend = window.desktop?.backend
  if (!backend) throw new Error('桌面服务桥不可用。')
  return backend
}
