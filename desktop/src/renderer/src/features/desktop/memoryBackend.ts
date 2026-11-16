import { replyExcerpt } from '../workspace/model'
import {
  addAgent,
  createWorkspace,
  toggleReaction,
  type Agent,
  type NewAgent,
  type AppWorkspace,
  type ChatMessage,
} from '../workspace/model'
import { ulid } from '../../../../shared/ulid'
import { normalizeServerAddress } from '../../../../shared/serverAddress'
import type {
  DeliveryResult,
  ServerBubble,
  DesktopBackend,
  LoginCredentials,
  SessionPhase,
  SessionSnapshot,
  UserProfile,
  UserEvent,
  WorkspaceSnapshot,
} from './backend'

const modelNotReady = '当前模型策略不可用，请联系管理员检查服务端模型配置。'

/** 测试里代替服务端的内存实现。它遵守同一套会话、保存和投递规则，不写 localStorage。 */
export class MemoryBackend implements DesktopBackend {
  private reactionRequests = new Map<string, string>()
  readonly savesSynchronously = true
  phase: SessionPhase = 'ready'
  serverUrl = ''
  connectError: string | null = null
  /** 测试用账号：用户名与密码都匹配才进入工作空间。 */
  username = 'owner'
  userProfile: UserProfile = { username: 'owner', displayName: '测试用户', avatar: '' }
  password = ''
  failSave: Error | null = null
  sessionError: Error | null = null
  private workspace = createWorkspace()
  private pristine = true
  private isModelPolicyAvailable = false
  private creations = new Map<string, { profile: string; agent: Agent }>()

  /** 地址与密码优先；其余阶段直接反映已登录状态。 */
  private gatePhase(): SessionPhase {
    if (
      this.phase === 'address' ||
      this.phase === 'offline' ||
      this.phase === 'login' ||
      this.phase === 'loading'
    ) {
      return this.phase
    }
    return 'ready'
  }

  trySessionSync(): SessionSnapshot {
    const phase = this.gatePhase()
    return {
      phase,
      serverUrl: this.serverUrl,
      ...(phase === 'offline' && this.connectError ? { message: this.connectError } : {}),
    }
  }

  session(): Promise<SessionSnapshot> {
    if (this.sessionError) return Promise.reject(this.sessionError)
    return Promise.resolve(this.trySessionSync())
  }

  loadUserProfile(): Promise<UserProfile> {
    return Promise.resolve({ ...this.userProfile })
  }

  watchWindowFocus(onFocus: () => void): () => void {
    window.addEventListener('focus', onFocus)
    return () => window.removeEventListener('focus', onFocus)
  }

  async connect(input: string): Promise<SessionSnapshot> {
    const serverUrl = normalizeServerAddress(input)
    this.serverUrl = serverUrl
    if (this.connectError) {
      this.phase = 'offline'
      return { phase: 'offline', message: this.connectError, serverUrl }
    }
    if (this.phase === 'address' || this.phase === 'offline' || this.phase === 'loading') {
      this.phase = 'login'
    }
    return this.trySessionSync()
  }

  async login(credentials: LoginCredentials): Promise<void> {
    if (credentials.username !== this.username || credentials.password !== this.password) {
      throw new Error('用户名或密码不正确。')
    }
    this.phase = 'ready'
  }

  async browserLogin(): Promise<SessionSnapshot> {
    // 内存实现没有系统浏览器；直接视为用户已在网页完成登录。
    this.phase = 'ready'
    return this.trySessionSync()
  }

  async trustServerCertificate(): Promise<void> {
    throw new Error('内存服务不支持证书验证。')
  }

  async logout(): Promise<void> {
    this.phase = 'login'
  }

  watchEvents(onEvent: (event: UserEvent) => void): () => void {
    this.eventListeners.add(onEvent)
    return () => this.eventListeners.delete(onEvent)
  }

  publishEvent(event: UserEvent): void {
    for (const listener of this.eventListeners) listener(event)
  }
  private eventListeners = new Set<(event: UserEvent) => void>()

  tryWorkspaceSync(): WorkspaceSnapshot {
    return { workspace: this.workspace, pristine: this.pristine }
  }

  loadWorkspace(): Promise<WorkspaceSnapshot> {
    return Promise.resolve(this.tryWorkspaceSync())
  }

  async createAgent(profile: NewAgent, requestId: string): Promise<Agent> {
    if (this.failSave) throw this.failSave
    const encoded = JSON.stringify(profile)
    const previous = this.creations.get(requestId)
    if (previous) {
      if (previous.profile !== encoded) throw new Error('创建请求的资料发生冲突。')
      return previous.agent
    }
    const workspace = addAgent(this.workspace, profile, ulid())
    const agent = workspace.agents.at(-1)!
    this.creations.set(requestId, { profile: encoded, agent })
    this.workspace = workspace
    this.pristine = false
    return agent
  }

  saveWorkspace(workspace: AppWorkspace): void {
    if (this.failSave) throw this.failSave
    this.workspace = workspace
    this.pristine = false
  }

  listGroupArtifacts(): ReturnType<DesktopBackend['listGroupArtifacts']> {
    return Promise.resolve({ artifacts: [], next: 0 })
  }

  loadIntroduction(): Promise<{ text: string }> {
    return Promise.resolve({ text: '' })
  }

  listRoutines(): Promise<
    Array<{
      id: string
      name: string
      cron: string
      prompt: string
      is_enabled: boolean
      revision: number
    }>
  > {
    return Promise.resolve([])
  }

  setRoutineEnabled(): Promise<void> {
    return Promise.resolve()
  }

  deleteRoutine(): Promise<void> {
    return Promise.resolve()
  }

  openComputer(): Promise<string> {
    return Promise.resolve('')
  }

  async setReaction(
    messageId: string,
    emoji: string,
    isActive: boolean,
    clientRequestId: string,
  ): Promise<ServerBubble> {
    const agent = this.workspace.agents.find((candidate) =>
      candidate.messages.some((message) => message.id === messageId),
    )
    const target = agent?.messages.find((message) => message.id === messageId)
    if (!agent || !target || target.status !== 'sent') throw new Error('消息尚未发送或不存在')
    const digest = JSON.stringify([messageId, emoji, isActive])
    const previous = this.reactionRequests.get(clientRequestId)
    if (previous && previous !== digest) throw new Error('表情请求内容冲突')
    if (!previous && target.reactions.some((reaction) => reaction.emoji === emoji) !== isActive) {
      this.workspace = toggleReaction(this.workspace, agent.id, target.id, emoji)
      const updated = this.workspace.agents
        .find((candidate) => candidate.id === agent.id)!
        .messages.find((message) => message.id === target.id)!
      updated.reactionRevision = (target.reactionRevision ?? 0) + 1
    }
    this.reactionRequests.set(clientRequestId, digest)
    const message = this.workspace.agents
      .find((candidate) => candidate.id === agent.id)!
      .messages.find((message) => message.id === target.id)!
    return {
      ...message,
      sequence: message.sequence ?? 1,
      speaker: message.role === 'assistant' ? agent.id : '',
      reactions: message.reactions,
      attachments: message.attachments ?? [],
    }
  }

  async deliverMessage(
    agentId: string,
    content: string,
    clientRequestId: string,
    onEvent?: (event: { type: string; text: string }) => void,
    replyToId?: string,
  ): Promise<DeliveryResult> {
    const agent = this.workspace.agents.find((item) => item.id === agentId)
    if (!agent) return { code: 'AGENT_NOT_FOUND', message: '助手不存在' }
    if (!content.trim()) return { code: 'INPUT_INVALID', message: '消息不能为空' }
    if (!this.isModelPolicyAvailable) {
      return { code: 'MODEL_NOT_READY', message: modelNotReady }
    }
    const replyTarget = agent.messages.find((message) => message.id === replyToId)
    if (replyToId && !replyTarget) return { code: 'INPUT_INVALID', message: '回复的消息不存在' }
    onEvent?.({ type: 'process', text: '正在思考' })
    const sent: ChatMessage = {
      id: `sent-${Date.now()}`,
      requestId: clientRequestId,
      ...(replyTarget
        ? { replyTo: { messageId: replyTarget.id, excerpt: replyExcerpt(replyTarget.content) } }
        : {}),
      role: 'user',
      content,
      createdAt: new Date().toISOString(),
      status: 'sent',
      reactions: [],
    }
    const reply: ChatMessage = {
      id: `reply-${Date.now()}`,
      role: 'assistant',
      content: '内存测试回复',
      createdAt: new Date().toISOString(),
      status: 'sent',
      reactions: [],
    }
    // 内存实现同时返回持久化消息，由调用方按消息和请求标识增量合并。
    return {
      acceptedMessageId: sent.id,
      messages: [
        ...agent.messages.filter((message) => message.id !== clientRequestId),
        sent,
        reply,
      ].map((message, index) => ({
        id: message.id,
        requestId: message.requestId,
        replyTo: message.replyTo,
        sequence: index + 1,
        role: message.role,
        speaker: message.role === 'assistant' ? agentId : '',
        content: message.content,
        createdAt: message.createdAt,
        reactions: message.reactions.map((reaction) => ({ ...reaction })),
        attachments: [],
      })),
    }
  }

  /** 测试模拟服务端策略可用性或空工作空间，客户端不参与模型配置。 */
  loadFixture(input: { workspace?: AppWorkspace; isModelPolicyAvailable?: boolean }): void {
    this.reactionRequests.clear()
    if (input.workspace) this.workspace = input.workspace
    if (input.isModelPolicyAvailable !== undefined)
      this.isModelPolicyAvailable = input.isModelPolicyAvailable
  }

  reset(): void {
    this.creations.clear()
    this.phase = 'ready'
    this.serverUrl = ''
    this.connectError = null
    this.password = ''
    this.failSave = null
    this.sessionError = null
    this.userProfile = { username: 'owner', displayName: '测试用户', avatar: '' }
    this.eventListeners.clear()
    this.workspace = createWorkspace()
    this.pristine = true
    this.isModelPolicyAvailable = false
  }
}

export const memoryBackend = new MemoryBackend()
