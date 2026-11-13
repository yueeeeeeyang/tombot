import { createSettingsDraft, type SettingsDraft } from '../settings/settings'
import { createWorkspace, type AppWorkspace, type ChatMessage } from '../workspace/model'
import { normalizeServerAddress } from '../../../../shared/serverAddress'
import type {
  DeliveryResult,
  DesktopBackend,
  LoginCredentials,
  SessionPhase,
  SessionSnapshot,
  SettingsView,
  UserEvent,
  WorkspaceSnapshot,
} from './backend'

const modelNotReady = '尚未配置可用模型，请先在设置中填写 API 地址和模型名称。'

/** 测试里代替服务端的内存实现。它遵守同一套会话、保存和投递规则，不写 localStorage。 */
export class MemoryBackend implements DesktopBackend {
  readonly savesSynchronously = true
  phase: SessionPhase = 'ready'
  serverUrl = ''
  connectError: string | null = null
  /** 测试用账号：用户名与密码都匹配才进入工作空间。 */
  username = 'owner'
  password = ''
  failSave: Error | null = null
  sessionError: Error | null = null
  private workspace = createWorkspace()
  private pristine = true
  private settings: SettingsDraft = {
    ...createSettingsDraft(),
    provider: 'custom',
    providerName: '测试供应商',
    baseUrl: '',
    modelName: '内存模型',
  }
  private apiKey = 'memory-key'

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

  async deliverMessage(
    agentId: string,
    content: string,
    onEvent?: (event: { type: string; text: string }) => void,
  ): Promise<DeliveryResult> {
    const agent = this.workspace.agents.find((item) => item.id === agentId)
    if (!agent) return { code: 'AGENT_NOT_FOUND', message: '助手不存在' }
    if (!content.trim()) return { code: 'INPUT_INVALID', message: '消息不能为空' }
    if (!this.settings.baseUrl.trim() || !this.settings.modelName.trim()) {
      return { code: 'MODEL_NOT_READY', message: modelNotReady }
    }
    onEvent?.({ type: 'process', text: '正在思考' })
    const sent: ChatMessage = {
      id: `sent-${Date.now()}`,
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
    // 内存实现返回服务端视角的完整列表，与真实服务端一致，由调用方整体替换。
    return {
      messages: [...agent.messages, sent, reply].map((message, index) => ({
        id: message.id,
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

  trySettingsSync(): SettingsView {
    return { ...this.settings, hasApiKey: this.apiKey.length > 0 }
  }

  loadSettings(): Promise<SettingsView> {
    return Promise.resolve(this.trySettingsSync())
  }

  listBuiltinProviders(): Promise<Array<{ id: string; name: string }>> {
    return Promise.resolve([{ id: 'deepseek', name: 'DeepSeek' }])
  }

  listProviderModels(
    _providerId: string,
    apiKey: string,
  ): Promise<Array<{ id: string; name: string }>> {
    if (!apiKey.trim() && !this.apiKey) {
      return Promise.reject(new Error('请填写 API Key。'))
    }
    if (apiKey === 'bad-key') {
      return Promise.reject(new Error('API Key 验证失败，请检查后重试。'))
    }
    return Promise.resolve([{ id: 'deepseek-chat', name: 'DeepSeek Chat' }])
  }

  saveSettings(draft: SettingsDraft, apiKey: string): SettingsView {
    this.settings = { ...draft }
    if (apiKey) this.apiKey = apiKey
    return this.trySettingsSync()
  }

  /** 测试切换到尚未配置模型或空工作空间。调用方结束时要 reset。 */
  loadFixture(input: {
    workspace?: AppWorkspace
    settings?: Partial<SettingsDraft>
    apiKey?: string
  }): void {
    if (input.workspace) this.workspace = input.workspace
    if (input.settings) this.settings = { ...createSettingsDraft(), ...input.settings }
    if (input.apiKey !== undefined) this.apiKey = input.apiKey
  }

  reset(): void {
    this.phase = 'ready'
    this.serverUrl = ''
    this.connectError = null
    this.password = ''
    this.failSave = null
    this.sessionError = null
    this.eventListeners.clear()
    this.workspace = createWorkspace()
    this.pristine = true
    this.settings = {
      ...createSettingsDraft(),
      provider: 'custom',
      providerName: '测试供应商',
      baseUrl: '',
      modelName: '内存模型',
    }
    this.apiKey = 'memory-key'
  }
}

export const memoryBackend = new MemoryBackend()
