import { createHash, randomUUID } from 'node:crypto'
import { createServer, type Server } from 'node:http'
import type { ElectronApplication } from '@playwright/test'
import { attachmentKind, attachmentMediaType } from '../../src/shared/attachmentMedia'
import {
  CHAT_FILE_MAX_BYTES,
  chatFilePath,
  type ChatFile,
  type ChatFilePreview,
} from '../../src/shared/chatFiles'
import type { CatTask } from '../../src/shared/tasks'
import { ulid } from '../../src/shared/ulid'
import type {
  AgentConversation,
  DeliveryResult,
  GroupDetail,
  ServerBubble,
  UserEvent,
} from '../../src/renderer/src/features/desktop/backend'
import { MemoryBackend } from '../../src/renderer/src/features/desktop/memoryBackend'
import {
  replyExcerpt,
  type AppWorkspace,
  type ChatMessage,
  type NewAgent,
} from '../../src/renderer/src/features/workspace/model'

export type FixtureDelivery = {
  conversationId: string
  botId?: string
  content: string
  requestId: string
  mentions?: string[]
  replyToId?: string
  attachments?: Array<{ path: string; sha256: string }>
}

type DeliveryInput = Omit<FixtureDelivery, 'conversationId' | 'botId' | 'requestId'>
type Route = (args: unknown[]) => unknown

const fileTypes: Record<string, string> = {
  pdf: 'application/pdf',
  txt: 'text/plain',
}
const workspaceFileLimit = 256 * 1024 * 1024
const requestLimit = Math.ceil(CHAT_FILE_MAX_BYTES * (4 / 3)) + 256 * 1024
const ipcTimeout = 10_000
const messagePageSize = 80

/** 布局用例走实际 Electron 窗口及预加载。真实认证、服务契约和执行交给独立的 V2 联合测试。 */
export class Companion {
  readonly url = 'https://tombot.example.test'
  private readonly backend = new MemoryBackend()
  private readonly messages = new Map<string, ServerBubble[]>()
  private readonly groups = new Map<string, GroupDetail>()
  private readonly tasks = new Map<string, CatTask[]>()
  private readonly files = new Map<string, ChatFile[]>()
  private readonly filePageSizes = new Map<string, number>()
  private readonly previews = new Map<string, ChatFilePreview>()
  private readonly handlers = new Map<string, Route>()
  private runs: AgentConversation['runs'] = []
  private server?: Server
  private endpoint = ''
  private application?: ElectronApplication
  private sequence = 0
  private isDeliveryEnabled = true
  private onDelivery?: (delivery: FixtureDelivery) => void | Promise<void>

  constructor(workspace: AppWorkspace) {
    this.backend.serverUrl = this.url
    this.backend.loadFixture({ workspace: structuredClone(workspace) })
    this.updateWorkspace(() => undefined)
    this.registerRoutes()
  }

  workspace(): AppWorkspace {
    return structuredClone(this.backend.tryWorkspaceSync().workspace)
  }

  updateWorkspace(mutate: (workspace: AppWorkspace) => void): void {
    const workspace = this.workspace()
    mutate(workspace)
    for (const agent of workspace.agents) {
      agent.conversationId ??= 'chat-' + agent.id
      const conversationId = agent.conversationId
      const saved = this.messages.get(conversationId) ?? []
      const history = new Map(saved.map((message) => [message.id, message]))
      for (const message of agent.messages.filter((candidate) => candidate.status === 'sent')) {
        const sequence = history.get(message.id)?.sequence ?? history.size + 1
        history.set(message.id, this.toServerBubble(message, agent.id, sequence))
      }
      this.messages.set(
        conversationId,
        [...history.values()].sort((first, second) => first.sequence - second.sequence),
      )
    }
    this.backend.saveWorkspace(workspace)
  }

  setTasks(agentId: string, tasks: CatTask[]): void {
    this.tasks.set(agentId, structuredClone(tasks))
  }

  setRuns(runs: AgentConversation['runs']): void {
    this.runs = structuredClone(runs)
  }

  setChatFiles(conversationId: string, files: ChatFile[], pageSize = 100): void {
    this.files.set(conversationId, structuredClone(files))
    this.filePageSizes.set(conversationId, pageSize)
  }

  setChatFilePreview(fileId: string, preview: ChatFilePreview): void {
    this.previews.set(fileId, structuredClone(preview))
  }

  setOffline(isOffline: boolean): void {
    this.backend.phase = isOffline ? 'offline' : 'ready'
    this.backend.connectError = isOffline ? '无法连接服务。请确认地址和网络后重试。' : null
  }

  setDeliveryEnabled(isEnabled: boolean): void {
    this.isDeliveryEnabled = isEnabled
  }

  setDelivery(handler: (delivery: FixtureDelivery) => void | Promise<void>): void {
    this.isDeliveryEnabled = true
    this.onDelivery = handler
  }

  conversationMessages(id: string): ServerBubble[] {
    return structuredClone(this.messages.get(id) ?? [])
  }

  async appendMessage(
    conversationId: string,
    message: ChatMessage,
    speaker?: string,
  ): Promise<ServerBubble> {
    const history = this.messages.get(conversationId) ?? []
    const agent = this.workspace().agents.find(
      (candidate) => candidate.conversationId === conversationId,
    )
    const bubble = this.toServerBubble(
      message,
      speaker ?? agent?.id ?? '',
      (history.at(-1)?.sequence ?? 0) + 1,
    )
    this.messages.set(conversationId, [...history, bubble])
    return structuredClone(bubble)
  }

  /** 启动夹具时偶发的真实首屏请求仍可完成。用例随后重载页面读取测试工作空间。 */
  async attach(application: ElectronApplication): Promise<void> {
    if (!this.server) await this.listen()
    this.application = application
    await application.evaluate(
      ({ ipcMain }, { endpoint, channels, timeout }) => {
        for (const channel of channels) {
          ipcMain.removeHandler(channel)
          ipcMain.handle(channel, async (_event, ...args: unknown[]) => {
            const response = await fetch(endpoint, {
              method: 'POST',
              body: JSON.stringify({ channel, args }, (_, value: unknown) =>
                value instanceof Uint8Array
                  ? { fixtureBytes: Buffer.from(value).toString('base64') }
                  : value,
              ),
              signal: AbortSignal.timeout(timeout),
            })
            if (!response.ok) throw new Error('桌面测试夹具请求失败。')
            return response.json()
          })
        }

        const watchers = new Map<string, { publish: (event: unknown) => void; stop: () => void }>()
        ipcMain.removeHandler('desktop:watch-events')
        ipcMain.handle(
          'desktop:watch-events',
          (event, watchId: string) =>
            new Promise((resolve) => {
              const { sender } = event
              const stop = () => {
                sender.removeListener('destroyed', stop)
                sender.removeListener('did-start-navigation', stop)
                watchers.delete(watchId)
                resolve({ ok: true, value: null })
              }
              sender.once('destroyed', stop)
              sender.once('did-start-navigation', stop)
              watchers.set(watchId, {
                publish: (message) => {
                  if (!sender.isDestroyed()) sender.send('desktop:user-event', message, watchId)
                },
                stop,
              })
            }),
        )
        ipcMain.removeAllListeners('desktop:unwatch-events')
        ipcMain.on('desktop:unwatch-events', (_event, watchId: string) =>
          watchers.get(watchId)?.stop(),
        )
        Reflect.set(globalThis, 'publishDesktopFixtureEvent', (event: unknown) => {
          for (const watcher of watchers.values()) watcher.publish(event)
        })
      },
      { endpoint: this.endpoint, channels: [...this.handlers.keys()], timeout: ipcTimeout },
    )
  }

  /** 通过预加载当前持有的 watchId 发布测试事件，重载后不会通知过期订阅。 */
  async publish(
    kind: string,
    resourceId: string,
    payload: Record<string, unknown> = {},
  ): Promise<void> {
    const event: UserEvent = {
      sequence: ++this.sequence,
      kind,
      resource_id: resourceId,
      payload,
      created_at: new Date().toISOString(),
    }
    if (!this.application) throw new Error('尚未绑定桌面测试窗口。')
    await this.application.evaluate((_, message) => {
      const publish = Reflect.get(globalThis, 'publishDesktopFixtureEvent') as (
        event: UserEvent,
      ) => void
      publish(message)
    }, event)
  }

  async stop(): Promise<void> {
    const server = this.server
    this.application = undefined
    if (!server) return
    this.server = undefined
    this.endpoint = ''
    await new Promise<void>((resolve, reject) => {
      server.close((error) => (error ? reject(error) : resolve()))
      server.closeAllConnections()
    })
  }

  private async listen(): Promise<void> {
    const route = '/desktop-fixture-' + randomUUID()
    const server = createServer(
      { requestTimeout: ipcTimeout, headersTimeout: ipcTimeout },
      async (request, response) => {
        if (request.method !== 'POST' || request.url !== route) {
          response.writeHead(404).end()
          return
        }
        try {
          const chunks: Buffer[] = []
          let size = 0
          for await (const chunk of request) {
            const bytes = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)
            size += bytes.length
            if (size > requestLimit) throw new Error('桌面测试请求超出大小限制。')
            chunks.push(bytes)
          }
          const { channel, args } = JSON.parse(
            Buffer.concat(chunks).toString(),
            (_, value: unknown) => {
              if (
                value &&
                typeof value === 'object' &&
                'fixtureBytes' in value &&
                typeof value.fixtureBytes === 'string'
              ) {
                return Buffer.from(value.fixtureBytes, 'base64')
              }
              return value
            },
          ) as { channel: string; args: unknown[] }
          const handler = this.handlers.get(channel)
          if (!handler || !Array.isArray(args)) throw new Error('桌面测试夹具收到了未登记的调用。')
          const value = await handler(args)
          response.end(JSON.stringify({ ok: true, value: value ?? null }))
        } catch (error) {
          const message = error instanceof Error ? error.message : '桌面测试夹具异常。'
          response.end(JSON.stringify({ ok: false, code: 'FIXTURE_FAILED', message }))
        }
      },
    )
    await new Promise<void>((resolve, reject) => {
      server.once('error', reject)
      server.listen(0, '127.0.0.1', resolve)
    })
    const address = server.address()
    if (!address || typeof address === 'string') throw new Error('没有分配到桌面测试夹具端口。')
    this.server = server
    this.endpoint = 'http://127.0.0.1:' + address.port + route
  }

  private register<T extends unknown[]>(channel: string, handler: (...args: T) => unknown): void {
    this.handlers.set('desktop:' + channel, (args) => handler(...(args as T)))
  }

  private registerRoutes(): void {
    this.register('session', () => this.backend.session())
    this.register('check-update', () => this.backend.checkClientUpdate())
    this.register('install-update', () => this.backend.installClientUpdate('', () => undefined))
    this.register('download-update', () => this.backend.downloadClientUpdate())
    this.register('about', () => this.backend.loadAbout())
    this.register('user-profile', () => this.backend.loadUserProfile())
    this.register('load-workspace', () => ({ workspace: this.workspace(), pristine: false }))
    this.register('save-workspace', (workspace: AppWorkspace) =>
      this.backend.saveWorkspace(workspace),
    )
    this.register('create-agent', async (profile: NewAgent, requestId: string) => {
      const agent = await this.backend.createAgent(profile, requestId)
      this.updateWorkspace(() => undefined)
      return this.getAgent(agent.id)
    })
    this.register('conversation-previews', (ids: string[]) =>
      ids.map((id) => {
        const latest = this.messages.get(id)?.at(-1)
        return {
          conversation_id: id,
          sequence: latest?.sequence ?? 0,
          content: latest ? replyExcerpt(latest.content) : '',
        }
      }),
    )
    this.register('agent-conversation', (id: string, before?: number | null) => {
      const agent = this.getAgent(id)
      return {
        ...this.messagePage(agent.conversationId!, before),
        runs: this.runs.filter((run) => run.bot_id === id),
      }
    })
    this.register('bot-activities', () => ({ runs: this.runs }))
    this.register('conversation-activity', (id: string) => ({
      runs: this.runs.filter((run) => this.getAgent(run.bot_id).conversationId === id),
    }))
    this.register(
      'conversation-messages',
      ({ conversationId, before }: { conversationId: string; before?: number }) =>
        this.messagePage(conversationId, before),
    )
    this.register(
      'deliver',
      async ({
        agentId,
        clientRequestId,
        ...message
      }: DeliveryInput & {
        agentId: string
        clientRequestId: string
      }) => {
        const { conversationId } = this.getAgent(agentId)
        return this.deliver({
          ...message,
          conversationId: conversationId!,
          botId: agentId,
          requestId: clientRequestId,
        })
      },
    )
    this.register('conversation-send', async (message: FixtureDelivery) => {
      const delivery = await this.deliver(message)
      if (delivery.code) throw new Error(delivery.message)
      return delivery
    })
    this.register(
      'reaction',
      ({ messageId, emoji, isActive }: { messageId: string; emoji: string; isActive: boolean }) =>
        this.react(messageId, emoji, isActive),
    )
    this.register('ensure-agent', (id: string) => {
      this.getAgent(id)
    })
    this.register('console-open', () => {
      throw new Error('这只猫咪的电脑暂不可用。')
    })
    this.registerGroupRoutes()
    this.registerTaskRoutes()
    this.registerFileRoutes()
  }

  private messagePage(
    id: string,
    before?: number | null,
  ): { messages: ServerBubble[]; hasMore: boolean } {
    const all = this.messages.get(id) ?? []
    const history =
      typeof before === 'number' ? all.filter((message) => message.sequence < before) : all
    return {
      messages: history.slice(-messagePageSize),
      hasMore: history.length >= messagePageSize,
    }
  }

  private toServerBubble(message: ChatMessage, speaker: string, sequence: number): ServerBubble {
    return {
      ...message,
      sequence: message.sequence ?? sequence,
      speaker: message.role === 'assistant' ? speaker : '',
      reactions: message.reactions ?? [],
      attachments: message.attachments ?? [],
    }
  }

  private async deliver(delivery: FixtureDelivery): Promise<DeliveryResult> {
    if (!this.isDeliveryEnabled) {
      return {
        code: 'MODEL_NOT_READY',
        message: '当前模型策略不可用，请联系管理员检查服务端模型配置。',
      }
    }
    const { conversationId, content, requestId, replyToId, attachments = [] } = delivery
    const history = this.messages.get(conversationId) ?? []
    const prior = history.find((candidate) => candidate.requestId === requestId)
    if (prior) return { acceptedMessageId: prior.id }
    const reply = history.find((candidate) => candidate.id === replyToId)
    const created = await this.appendMessage(conversationId, {
      id: ulid(),
      requestId,
      role: 'user',
      content,
      createdAt: new Date().toISOString(),
      status: 'sent',
      reactions: [],
      ...(reply && { replyTo: { messageId: reply.id, excerpt: replyExcerpt(reply.content) } }),
      attachments: attachments.map(({ path, sha256 }) => {
        const file = this.fileByPath(path)
        return {
          name: file.name,
          path,
          sha256,
          mediaType: file.media_type,
          kind: attachmentKind(file.name, file.media_type),
        }
      }),
    })
    await this.onDelivery?.(delivery)
    return { acceptedMessageId: created.id }
  }

  private react(messageId: string, emoji: string, isActive: boolean): ServerBubble {
    for (const history of this.messages.values()) {
      const target = history.find((candidate) => candidate.id === messageId)
      if (!target) continue
      const existing = target.reactions.find((candidate) => candidate.emoji === emoji)
      if (isActive && !existing) target.reactions.push({ emoji, count: 1 })
      if (!isActive && existing)
        target.reactions = target.reactions.filter((candidate) => candidate !== existing)
      target.reactionRevision = (target.reactionRevision ?? 0) + 1
      return target
    }
    throw new Error('找不到要操作的测试消息。')
  }

  private getAgent(id: string) {
    const agent = this.backend
      .tryWorkspaceSync()
      .workspace.agents.find((candidate) => candidate.id === id)
    if (!agent) throw new Error('找不到要操作的测试猫咪。')
    return agent
  }

  private getGroup(id: string): GroupDetail {
    const group = this.groups.get(id)
    if (!group) throw new Error('找不到要操作的测试群聊。')
    return group
  }

  private setMembers(group: GroupDetail, ids: string[]): GroupDetail {
    const agents = ids.map((id) => this.getAgent(id))
    group.members = agents.map((agent) => ({ bot_id: agent.id, name: agent.name }))
    group.avatar_presets = agents.map((agent) => agent.avatar)
    group.revision += 1
    return group
  }

  private registerGroupRoutes(): void {
    this.register('groups', () => [...this.groups.values()])
    this.register('group', (id: string) => this.getGroup(id))
    this.register('group-create', ({ name, memberIds }: { name: string; memberIds: string[] }) => {
      const group = this.setMembers(
        {
          id: ulid(),
          name,
          revision: 0,
          conversation_id: ulid(),
          avatar_presets: [],
          members: [],
        },
        memberIds,
      )
      this.groups.set(group.id, group)
      return group
    })
    this.register('group-rename', ({ id, name }: { id: string; name: string }) => {
      const group = this.getGroup(id)
      group.name = name
      group.revision += 1
      return group
    })
    this.register('group-members', ({ id, botIds }: { id: string; botIds: string[] }) => {
      const group = this.getGroup(id)
      this.setMembers(group, [
        ...new Set([...group.members.map((member) => member.bot_id), ...botIds]),
      ])
    })
    this.register('group-member-remove', ({ id, botId }: { id: string; botId: string }) => {
      const group = this.getGroup(id)
      this.setMembers(
        group,
        group.members.map((member) => member.bot_id).filter((member) => member !== botId),
      )
    })
  }

  private registerTaskRoutes(): void {
    this.register('tasks', (id: string) => this.tasks.get(id) ?? [])
    this.register(
      'task-enabled',
      ({ agentId, id, enabled }: { agentId: string; id: string; enabled: boolean }) => {
        const task = this.tasks.get(agentId)?.find((candidate) => candidate.id === id)
        if (!task) throw new Error('找不到要操作的测试定时任务。')
        task.is_enabled = enabled
        task.revision += 1
      },
    )
    this.register('task-delete', ({ agentId, id }: { agentId: string; id: string }) => {
      const tasks = this.tasks.get(agentId) ?? []
      if (!tasks.some((candidate) => candidate.id === id))
        throw new Error('找不到要删除的测试定时任务。')
      this.tasks.set(
        agentId,
        tasks.filter((task) => task.id !== id),
      )
    })
  }

  private fileByPath(path: string): ChatFile {
    const file = [...this.files.values()]
      .flat()
      .find((candidate) => chatFilePath(candidate) === path)
    if (!file) throw new Error('找不到要操作的测试文件。')
    return file
  }

  private upload(conversationId: string, name: string, bytes: Buffer, type?: string): ChatFile {
    const extension = name.split('.').at(-1)?.toLowerCase() ?? ''
    const mediaType =
      attachmentMediaType(name, type ?? '') ??
      (type || fileTypes[extension] || 'application/octet-stream')
    const file: ChatFile = {
      id: ulid(),
      conversation_id: conversationId,
      name,
      media_type: mediaType,
      size: bytes.length,
      sha256: createHash('sha256').update(bytes).digest('hex'),
      revision: 1,
      created_at: new Date().toISOString(),
    }
    const preview: ChatFilePreview = { file }
    if (attachmentMediaType(name, mediaType))
      preview.image = 'data:' + mediaType + ';base64,' + bytes.toString('base64')
    if (mediaType === 'text/plain') preview.text = bytes.toString('utf8')
    this.setChatFilePreview(file.id, preview)
    this.files.set(conversationId, [file, ...(this.files.get(conversationId) ?? [])])
    return file
  }

  private getPreview(id: string, fileId: string): ChatFilePreview {
    const file = this.files.get(id)?.find((candidate) => candidate.id === fileId)
    if (!file) throw new Error('找不到要预览的测试文件。')
    return { ...this.previews.get(fileId), file }
  }

  private registerFileRoutes(): void {
    this.register(
      'upload',
      ({
        name,
        bytes,
        type,
        botId,
        groupId,
      }: {
        name: string
        bytes: Buffer
        type?: string
        botId?: string
        groupId?: string
      }) => {
        const conversationId = groupId
          ? this.getGroup(groupId).conversation_id
          : this.getAgent(botId!).conversationId!
        const file = this.upload(conversationId, name, bytes, type)
        return { path: chatFilePath(file), sha256: file.sha256 }
      },
    )
    this.register('preview', ({ path }: { path: string }) => {
      const file = this.fileByPath(path)
      const preview = this.getPreview(file.conversation_id, file.id)
      if (!preview.image) throw new Error('此测试文件没有可用的媒体预览。')
      return preview.image
    })
    this.register(
      'chat-files',
      ({ id, before, query }: { id: string; before?: string; query?: string }) => {
        const all = this.files.get(id) ?? []
        const matched = all.filter(
          (file) => !query || file.name.toLowerCase().includes(query.toLowerCase()),
        )
        const start = before ? Number(before) : 0
        const end = start + (this.filePageSizes.get(id) ?? 100)
        return {
          files: matched.slice(start, end),
          next: end < matched.length ? String(end) : '',
          used_bytes: all.reduce((size, file) => size + file.size, 0),
          max_bytes: workspaceFileLimit,
          max_file_bytes: CHAT_FILE_MAX_BYTES,
        }
      },
    )
    this.register('chat-file-preview', ({ id, fileId }: { id: string; fileId: string }) =>
      this.getPreview(id, fileId),
    )
  }
}
