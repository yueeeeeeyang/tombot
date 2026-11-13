import type { Artifact } from '../workspace/model'
import type { SettingsDraft } from '../settings/settings'
import type { Agent, AppWorkspace } from '../workspace/model'

export interface StreamEvent {
  type: string
  text: string
  incoming?: IncomingMessages
  botId?: string
  runId?: string
  isWorking?: boolean
  isFinished?: boolean
  messageId?: string
}

export type IncomingMessages = Array<Pick<Agent, 'id' | 'messages'>>

/**
 * 会话阶段：没有地址、连不上、需要登录、可以进入工作空间。
 * 单个 Bot 的电脑未就绪只影响它的电脑能力，不再作为整页门禁。
 */
export type SessionPhase = 'loading' | 'address' | 'offline' | 'login' | 'ready'

export interface SessionSnapshot {
  phase: SessionPhase
  message?: string
  serverUrl?: string
  username?: string
}

/** 桌面登录凭据只在本进程提交；动态验证码只在管理员账户要求时填写。 */
export interface LoginCredentials {
  username: string
  password: string
  code?: string
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
  /** 会话内单调递增序号；向前翻页时作为 before 游标。 */
  sequence: number
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

/** 会话消息的一页；hasMore 为真时还能继续向前加载。 */
export interface MessagePage {
  messages: ServerBubble[]
  hasMore: boolean
}

/** 投递结果。服务端整段返回助手消息，客户端按身份替换本地未发送状态。 */
export interface DeliveryResult {
  /** 服务端已持久化的会话消息；缺省时不改动本地记录。 */
  messages?: ServerBubble[]
  code?: string
  message?: string
}

/**
 * 桌面与服务之间的唯一通道。
 * 测试用的内存实现会同步保存；真正的服务调用返回 Promise。
 */
export interface DesktopBackend {
  readClipboardFiles?: () => Promise<Array<{ name: string; type: string; bytes: Uint8Array }>>
  savesSynchronously?: boolean
  trySessionSync?: () => SessionSnapshot
  session: () => Promise<SessionSnapshot>
  /** 保存用户确认的服务地址并检查登录状态。地址不合法时抛出。 */
  connect: (serverUrl: string) => Promise<SessionSnapshot>
  /** 用系统浏览器完成登录：桌面端只接收一次性授权码，口令不经过本进程。 */
  browserLogin: () => Promise<SessionSnapshot>
  logout?: () => Promise<void>
  /** 订阅当前用户事件；服务端在有界窗口后收尾，调用方负责续订。 */
  watchEvents?: (onEvent: (event: UserEvent) => void, onError: (error: Error) => void) => () => void
  tryWorkspaceSync?: () => WorkspaceSnapshot
  loadWorkspace: () => Promise<WorkspaceSnapshot>
  saveWorkspace: (workspace: AppWorkspace) => void | Promise<void>
  /** 把一条用户消息交给服务端执行；返回该会话的最新消息列表。 */
  deliverMessage: (
    agentId: string,
    content: string,
    onEvent?: (event: StreamEvent) => void,
  ) => Promise<DeliveryResult>
  openComputer?: (agentId: string) => Promise<string>
  loadIntroduction: (agentId: string) => Promise<{ text: string }>
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
  listGroups?: () => Promise<GroupSummary[]>
  createGroup?: (name: string, memberIds: string[]) => Promise<GroupSummary>
  listGroupArtifacts: (
    id: string,
    before?: number,
  ) => Promise<{ artifacts: Artifact[]; next: number }>
  groupDetail?: (id: string) => Promise<GroupDetail>
  renameGroup?: (id: string, revision: number, name: string) => Promise<GroupDetail>
  addGroupMembers?: (id: string, revision: number, botIds: string[]) => Promise<void>
  removeGroupMember?: (id: string, revision: number, botId: string) => Promise<void>
  /**
   * 读取会话消息的一页，按时间正序返回。
   * before 为上一页最早消息的序号；hasMore 表示还有更早的记录可继续加载。
   */
  listConversationMessages?: (conversationId: string, before?: number) => Promise<MessagePage>
  sendConversation?: (
    conversationId: string,
    content: string,
    attachments: Array<{ path: string; sha256: string }>,
    replyToId: string | undefined,
    mentions: string[],
    onEvent?: (event: StreamEvent) => void,
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
  listBuiltinProviders?: () => Promise<Array<{ id: string; name: string }>>
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

export type GroupSummary = {
  avatar_presets: string[]
  id: string
  name: string
  revision: number
  conversation_id: string
}

export type GroupDetail = {
  avatar_presets: string[]
  id: string
  name: string
  revision: number
  conversation_id: string
  members: Array<{ bot_id: string; name: string }>
}

/**
 * 服务端已提交事件。界面只把它当作"该重新读取了"的提示，
 * 不用它承载正文，避免本地状态与权威记录分叉。
 */
export type UserEvent = {
  sequence: number
  kind: string
  resource_id: string
  payload: Record<string, unknown>
  created_at: string
}
