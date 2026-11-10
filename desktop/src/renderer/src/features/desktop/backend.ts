import type { SettingsDraft } from '../settings/settings'
import type { AppWorkspace } from '../workspace/model'

export type SessionPhase =
  'loading' | 'address' | 'offline' | 'setup' | 'login' | 'bootstrap' | 'ready'

export interface BootstrapStep {
  id: string
  title: string
  status: string
  detail?: string
  duration_ms: number
}

export interface BootstrapStatus {
  phase: string
  steps: BootstrapStep[]
  error?: string
  cursor: number
}

export interface SessionSnapshot {
  phase: SessionPhase
  message?: string
  serverUrl?: string
  bootstrap?: BootstrapStatus
}

export interface WorkspaceSnapshot {
  workspace: AppWorkspace
  pristine: boolean
}

export interface SettingsView extends SettingsDraft {
  hasApiKey: boolean
}

export type ServerBubble = {
  id: string
  role: 'user' | 'assistant'
  speaker: string
  content: string
  createdAt: string
  replyToId?: string
  reactions: Array<{ emoji: string; count: number }>
  attachments: Array<{
    name: string
    path: string
    sha256: string
    mediaType: string
    kind: 'image' | 'file'
  }>
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
  trySessionSync?: () => SessionSnapshot
  session: () => Promise<SessionSnapshot>
  /** 保存用户确认的服务地址并检查是否已初始化。地址不合法时抛出。 */
  connect: (serverUrl: string) => Promise<SessionSnapshot>
  watchBootstrap?: (
    onStatus: (status: BootstrapStatus) => void,
    onError: (error: Error) => void,
  ) => () => void
  setup: (input: { setupCode: string; password: string }) => Promise<void>
  login: (password: string) => Promise<void>
  tryWorkspaceSync?: () => WorkspaceSnapshot
  loadWorkspace: () => Promise<WorkspaceSnapshot>
  saveWorkspace: (workspace: AppWorkspace) => void | Promise<void>
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
  listGroups?: () => Promise<
    Array<{ id: string; name: string; revision: number; conversation_id: string }>
  >
  createGroup?: (
    name: string,
    memberIds: string[],
  ) => Promise<{ id: string; name: string; revision: number; conversation_id: string }>
  groupDetail?: (id: string) => Promise<{
    id: string
    name: string
    revision: number
    conversation_id: string
    members: Array<{ bot_id: string; name: string }>
  }>
  addGroupMembers?: (id: string, revision: number, botIds: string[]) => Promise<void>
  removeGroupMember?: (id: string, revision: number, botId: string) => Promise<void>
  listConversationMessages?: (conversationId: string) => Promise<ServerBubble[]>
  sendConversation?: (
    conversationId: string,
    content: string,
    attachments: Array<{ path: string; sha256: string }>,
    replyToId: string | undefined,
    onEvent?: (event: {
      type: string
      text: string
      messageId?: string
      peerId?: string
      peerName?: string
      conversationId?: string
      direction?: 'sent' | 'received'
    }) => void,
  ) => Promise<void>
  uploadAttachment?: (
    file: { name: string; type: string; bytes: Uint8Array },
    destination: { botId?: string; shared?: boolean },
  ) => Promise<{ path: string; sha256: string }>
  previewAttachment?: (path: string, botId?: string) => Promise<string>
  toggleReaction?: (messageId: string, emoji: string) => Promise<void>
  ensureAgent?: (agentId: string) => Promise<void>
  startBootstrap?: () => Promise<void>
  trySettingsSync?: () => SettingsView
  loadSettings: () => Promise<SettingsView>
  saveSettings: (draft: SettingsDraft, apiKey: string) => SettingsView | Promise<SettingsView>
  listProviderModels?: (
    providerId: string,
    apiKey: string,
  ) => Promise<Array<{ id: string; name: string }>>
}

/** 读取当前页面的服务通道。没有桥接时直接失败，不用本地数据假装已连接。 */
export function getBackend(): DesktopBackend {
  const backend = window.desktop?.backend
  if (!backend) throw new Error('桌面服务桥不可用。')
  return backend
}
