import { createSettingsDraft, type SettingsDraft } from '../settings/settings'
import { createWorkspace, type AppWorkspace, type ChatMessage } from '../workspace/model'
import { normalizeServerAddress } from '../../../../shared/serverAddress'
import type {
  BootstrapStatus,
  DeliveryResult,
  DesktopBackend,
  SessionPhase,
  SessionSnapshot,
  SettingsView,
  WorkspaceSnapshot,
} from './backend'

const modelNotReady = '尚未配置可用模型，请先在设置中填写 API 地址和模型名称。'

/** 测试里代替服务端的内存实现。它遵守同一套会话、保存和投递规则，不写 localStorage。 */
export class MemoryBackend implements DesktopBackend {
  readonly savesSynchronously = true
  phase: SessionPhase = 'ready'
  serverUrl = ''
  connectError: string | null = null
  setupCode = 'memory-setup-code'
  password = ''
  failSave: Error | null = null
  sessionError: Error | null = null
  /** 为 true 时，下一次重试仍失败，用来验证失败不会进入工作空间。 */
  failNextBootstrap = false
  bootstrap: BootstrapStatus = { phase: 'ready', steps: [], cursor: 0 }
  private listeners = new Set<(status: BootstrapStatus) => void>()
  private workspace = createWorkspace()
  private pristine = true
  private settings: SettingsDraft = createSettingsDraft()
  private apiKey = ''

  /** 地址、初始化和密码优先于引导。已验证且引导未完成时才停在进度页。 */
  private gatePhase(): SessionPhase {
    if (
      this.phase === 'address' ||
      this.phase === 'offline' ||
      this.phase === 'setup' ||
      this.phase === 'login' ||
      this.phase === 'loading'
    ) {
      return this.phase
    }
    if (this.bootstrap.phase !== 'ready') return 'bootstrap'
    return 'ready'
  }

  trySessionSync(): SessionSnapshot {
    const phase = this.gatePhase()
    return {
      phase,
      serverUrl: this.serverUrl,
      ...(phase === 'offline' && this.connectError ? { message: this.connectError } : {}),
      ...(phase === 'bootstrap' ? { bootstrap: this.bootstrap } : {}),
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
      this.phase = this.password ? 'login' : 'setup'
    }
    return this.trySessionSync()
  }

  watchBootstrap(onStatus: (status: BootstrapStatus) => void): () => void {
    this.listeners.add(onStatus)
    return () => this.listeners.delete(onStatus)
  }

  publishBootstrap(status: BootstrapStatus): void {
    this.bootstrap = status
    if (status.phase === 'ready') this.phase = 'ready'
    for (const listener of this.listeners) listener(status)
  }

  async setup(input: { setupCode: string; password: string }): Promise<void> {
    if ([...input.password].length < 12) {
      throw new Error('密码至少 12 个字符，且不能超过 1024 字节。')
    }
    if (input.setupCode.trim() !== this.setupCode) throw new Error('初始化码不正确。')
    this.password = input.password
    this.phase = 'ready'
  }

  async login(password: string): Promise<void> {
    if (this.phase === 'setup') throw new Error('请先完成初始化。')
    if (password !== this.password) throw new Error('密码不正确。')
    this.phase = 'ready'
  }

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

  importWorkspace(workspace: AppWorkspace): AppWorkspace {
    if (!this.pristine) throw new Error('服务端已有工作空间，不能再用本机旧档覆盖。')
    this.workspace = workspace
    this.pristine = false
    return workspace
  }

  listMemories(): Promise<Array<{ path: string; title: string; summary: string; sha256: string }>> {
    return Promise.resolve([])
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

  startBootstrap(): Promise<void> {
    if (this.bootstrap.phase !== 'failed') return Promise.resolve()
    if (this.failNextBootstrap) {
      this.failNextBootstrap = false
      this.publishBootstrap({
        ...this.bootstrap,
        cursor: this.bootstrap.cursor + 1,
        error: this.bootstrap.error ?? '准备失败',
      })
      return Promise.resolve()
    }
    this.publishBootstrap({
      phase: 'ready',
      cursor: this.bootstrap.cursor + 1,
      steps: this.bootstrap.steps.map((step) => ({ ...step, status: 'ok', detail: undefined })),
    })
    return Promise.resolve()
  }

  openComputer(): Promise<string> {
    return Promise.resolve('http://127.0.0.1:9/api/v1/computer/view?agent=memory&ticket=memory')
  }

  async deliverMessage(
    agentId: string,
    messageId: string,
    onEvent?: (event: { type: string; text: string }) => void,
  ): Promise<DeliveryResult> {
    const agent = this.workspace.agents.find((item) => item.id === agentId)
    const message = agent?.messages.find((item) => item.id === messageId)
    if (!agent || !message || message.role !== 'user') {
      return { code: 'MESSAGE_NOT_FOUND', message: '消息不存在' }
    }
    if (message.status === 'sent') return { workspace: this.workspace }
    if (!this.settings.baseUrl.trim() || !this.settings.modelName.trim()) {
      return { code: 'MODEL_NOT_READY', message: modelNotReady }
    }
    onEvent?.({ type: 'process', text: '正在思考' })
    onEvent?.({ type: 'delta', text: '内存测试回复' })
    const sent: ChatMessage = {
      ...message,
      status: 'sent',
      requestId: message.requestId ?? 'memory-request',
    }
    const reply: ChatMessage = {
      id: `reply-${message.id}`,
      role: 'assistant',
      content: '内存测试回复',
      createdAt: new Date().toISOString(),
      status: 'sent',
      requestId: sent.requestId,
      reactions: [],
    }
    this.workspace = {
      ...this.workspace,
      agents: this.workspace.agents.map((item) =>
        item.id === agentId
          ? {
              ...item,
              messages: item.messages
                .map((entry) => (entry.id === messageId ? sent : entry))
                .concat(reply),
            }
          : item,
      ),
    }
    this.pristine = false
    return { workspace: this.workspace }
  }

  trySettingsSync(): SettingsView {
    return { ...this.settings, hasApiKey: this.apiKey.length > 0 }
  }

  loadSettings(): Promise<SettingsView> {
    return Promise.resolve(this.trySettingsSync())
  }

  saveSettings(draft: SettingsDraft, apiKey: string): SettingsView {
    this.settings = { ...draft }
    if (apiKey) this.apiKey = apiKey
    return this.trySettingsSync()
  }

  reset(): void {
    this.phase = 'ready'
    this.serverUrl = ''
    this.connectError = null
    this.password = ''
    this.failSave = null
    this.sessionError = null
    this.failNextBootstrap = false
    this.bootstrap = { phase: 'ready', steps: [], cursor: 0 }
    this.listeners.clear()
    this.workspace = createWorkspace()
    this.pristine = true
    this.settings = createSettingsDraft()
    this.apiKey = ''
  }
}

export const memoryBackend = new MemoryBackend()
