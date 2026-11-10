import { createSettingsDraft, type SettingsDraft } from '../settings/settings'
import { createWorkspace, type AppWorkspace, type ChatMessage } from '../workspace/model'
import type {
  DeliveryResult,
  DesktopBackend,
  SessionPhase,
  SettingsView,
  WorkspaceSnapshot,
} from './backend'

const modelNotReady = '尚未配置可用模型，请先在设置中填写 API 地址和模型名称。'

/** 测试里代替服务端的内存实现。它遵守同一套会话、保存和投递规则，不写 localStorage。 */
export class MemoryBackend implements DesktopBackend {
  readonly savesSynchronously = true
  phase: SessionPhase = 'ready'
  setupCode = 'memory-setup-code'
  password = ''
  failSave: Error | null = null
  private workspace = createWorkspace()
  private pristine = true
  private settings: SettingsDraft = createSettingsDraft()
  private apiKey = ''

  trySessionSync(): SessionPhase {
    return this.phase
  }

  session(): Promise<{ phase: SessionPhase }> {
    return Promise.resolve({ phase: this.phase })
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
    this.password = ''
    this.failSave = null
    this.workspace = createWorkspace()
    this.pristine = true
    this.settings = createSettingsDraft()
    this.apiKey = ''
  }
}

export const memoryBackend = new MemoryBackend()
