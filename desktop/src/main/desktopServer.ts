import { app, ipcMain, shell, type IpcMainInvokeEvent } from 'electron'
import { createHash, randomBytes } from 'node:crypto'
import { chmod, mkdir, readFile, writeFile } from 'node:fs/promises'
import { createServer } from 'node:http'
import { join } from 'node:path'
import { readClipboardFiles } from './clipboardFiles'
import { ulid } from './ulid'
import {
  draftsFromWorkspace,
  workspaceFromSnapshot,
  workspaceSelection,
  type LocalDrafts,
  type ServerGroup,
  type ServerSnapshot,
} from './workspaceSnapshot'
import type { AppWorkspace } from '../renderer/src/features/workspace/model'
import type { UserEvent } from '../renderer/src/features/desktop/backend'
import {
  normalizeServerAddress,
  parseStoredSession,
  selectServerAddress,
  serializeStoredSession,
} from '../shared/serverAddress'

const codeMessages: Record<string, string> = {
  INVALID_GROUP_MENTION: '请从提及菜单选择当前群成员。',
  AMBIGUOUS_GROUP_MENTION: '有猫咪使用了相同名称，请从提及菜单选择具体成员。',
  GROUP_CHANGED: '群资料已被更新，请重新确认后保存。',
  GROUP_LAST_MEMBER: '群里至少保留一位猫咪。',

  SETUP_CODE_INVALID: '初始化码不正确。',
  PASSWORD_INVALID: '密码至少 12 个字符，且不能超过 1024 字节。',
  ALREADY_INITIALIZED: '服务已经初始化，请改为验证密码。',
  NOT_INITIALIZED: '请先完成初始化。',
  AUTH_INVALID: '密码不正确。',
  AUTH_REQUIRED: '需要先验证密码。',
  AUTH_RATE_LIMITED: '尝试次数过多，请稍后再试。',
  BOOTSTRAP_RUNNING: '引导正在进行，请稍候。',
  MODEL_NOT_READY: '尚未配置可用模型，请先在设置中填写 API 地址和模型名称。',
  WORKSPACE_CONFLICT: '工作空间刚被更新，请重新发送。',
  OFFLINE: '无法连接服务。请确认地址和网络后重试。',
  SERVER_URL_MISSING: '还没有填写服务地址。',
}

interface Failure {
  code: string
  message: string
}

type CallResult<T> = { ok: true; value: T } | ({ ok: false } & Failure)

// 访问与刷新凭据只在主进程内保存；渲染进程只能通过受限通道获得会话摘要。
let accessToken = ''
let refreshToken = ''
let accessExpiresAt = ''
let username = ''
let savedServerAddress = ''
let sessionLoad: Promise<void> | null = null
/** 服务端工作空间修订号；保存选择时必须回传，冲突由服务端判定。 */
let workspaceRevision = 0
/** 机器人到单聊会话的映射；会话只由服务端创建，客户端不自行构造。 */
const conversations = new Map<string, string>()
/** 机器人显示名与头像来自最近一次目录快照，仅用于群成员展示。 */
const botNames = new Map<string, string>()
const botAvatars = new Map<string, string>()

// 服务通常在另一台机器上。没有已保存地址时不能猜一个本机端口。
function requireServerAddress(): string {
  if (!savedServerAddress) {
    throw Object.assign(new Error(codeMessages.SERVER_URL_MISSING), { code: 'SERVER_URL_MISSING' })
  }
  return savedServerAddress
}

function devicePlatform(): string {
  if (process.platform === 'darwin') return 'macos'
  if (process.platform === 'win32') return 'windows'
  return 'linux'
}

function sessionPath(): string {
  return join(app.getPath('userData'), 'desktop-session.json')
}

function loadSession(): Promise<void> {
  // 第二次调用必须等第一次读完。否则连接请求会先写地址，迟到的空读取再把地址清掉。
  sessionLoad ??= readStoredSession()
  return sessionLoad
}

async function readStoredSession(): Promise<void> {
  try {
    const stored = parseStoredSession(await readFile(sessionPath(), 'utf8'))
    accessToken = stored.accessToken
    refreshToken = stored.refreshToken
    accessExpiresAt = stored.accessExpiresAt
    username = stored.username
    savedServerAddress = stored.serverUrl
  } catch {
    clearCredentials()
    savedServerAddress = ''
  }
}

function clearCredentials(): void {
  accessToken = ''
  refreshToken = ''
  accessExpiresAt = ''
  username = ''
}

/** 凭据和地址写在同一个文件里，避免只更新令牌时把地址抹掉。 */
async function persistSession(): Promise<void> {
  const path = sessionPath()
  await mkdir(app.getPath('userData'), { recursive: true })
  await writeFile(
    path,
    serializeStoredSession({
      accessToken,
      refreshToken,
      accessExpiresAt,
      serverUrl: savedServerAddress,
      username,
    }),
    { mode: 0o600 },
  )
  await chmod(path, 0o600)
}

interface AuthSession {
  principal?: { user_id?: string; role?: string }
  access_token?: string
  refresh_token?: string
  access_expires_at?: string
}

/** 本地草稿按服务与用户分区，切换账户或换服务后不会读到别人的草稿。 */
function draftsPath(): string {
  const key = createHash('sha256')
    .update(`${savedServerAddress}\n${username}`)
    .digest('hex')
    .slice(0, 32)
  return join(app.getPath('userData'), `workspace-${key}.json`)
}

async function readLocalDrafts(): Promise<LocalDrafts> {
  try {
    const parsed = JSON.parse(await readFile(draftsPath(), 'utf8')) as Partial<LocalDrafts>
    const drafts: LocalDrafts = { drafts: {}, artifacts: {} }
    if (parsed.drafts && typeof parsed.drafts === 'object') {
      for (const [id, value] of Object.entries(parsed.drafts)) {
        if (typeof value === 'string') drafts.drafts[id] = value
      }
    }
    if (parsed.artifacts && typeof parsed.artifacts === 'object')
      drafts.artifacts = parsed.artifacts
    return drafts
  } catch {
    // 没有草稿或格式损坏都按空草稿处理；服务端目录才是权威副本。
    return { drafts: {}, artifacts: {} }
  }
}

async function writeLocalDrafts(drafts: LocalDrafts): Promise<void> {
  const path = draftsPath()
  await mkdir(app.getPath('userData'), { recursive: true })
  await writeFile(path, JSON.stringify(drafts), { mode: 0o600 })
  await chmod(path, 0o600)
}

/**
 * 读取服务端目录并映射成本地工作台；草稿、产物等本机状态从分区文件补回。
 * 单聊会话只由服务端创建，这里同时记录机器人到会话的对应关系供投递使用。
 */
async function loadWorkspace(): Promise<{ workspace: AppWorkspace; pristine: boolean }> {
  const response = (await call('GET', '/workspace?limit=100')) as { snapshot?: ServerSnapshot }
  const snapshot = response.snapshot
  if (!snapshot)
    throw Object.assign(new Error('服务没有返回工作空间。'), { code: 'RESPONSE_INVALID' })
  workspaceRevision = snapshot.workspace.revision
  const mapped = workspaceFromSnapshot(snapshot, await readLocalDrafts())
  conversations.clear()
  botNames.clear()
  botAvatars.clear()
  for (const [agentID, conversationID] of mapped.conversations)
    conversations.set(agentID, conversationID)
  for (const bot of snapshot.bots) {
    botNames.set(bot.id, bot.name)
    botAvatars.set(bot.id, bot.avatar_ref)
  }
  // 快照本身已包含全部资源状态；历史事件无需重放，游标从当前水位开始。
  eventCursor = Math.max(eventCursor, snapshot.event_sequence)
  return { workspace: mapped.workspace, pristine: false }
}

/**
 * 保存工作台：草稿留在本机，选择（当前与主机器人）写回服务端。
 * 修订冲突时重新读取后再决定，客户端不自动覆盖别人的修改。
 */
async function saveWorkspace(workspace: AppWorkspace): Promise<void> {
  await writeLocalDrafts(draftsFromWorkspace(workspace))
  if (workspaceRevision < 1) return
  const response = (await call(
    'PUT',
    '/workspace',
    workspaceSelection(workspace, workspaceRevision),
  )) as {
    workspace?: { revision: number }
  }
  if (response.workspace) workspaceRevision = response.workspace.revision
}

async function storeSession(session: AuthSession): Promise<void> {
  await loadSession()
  if (!session.access_token || !session.refresh_token) {
    throw Object.assign(new Error('登录没有返回访问凭据。'), { code: 'AUTH_INVALID' })
  }
  accessToken = session.access_token
  refreshToken = session.refresh_token
  accessExpiresAt = session.access_expires_at ?? ''
  await persistSession()
}

/** 访问凭据临近到期才刷新，避免每个请求都触发一次轮换。 */
function accessIsStale(): boolean {
  if (!accessExpiresAt) return true
  const expiry = Date.parse(accessExpiresAt)
  if (Number.isNaN(expiry)) return true
  return expiry - Date.now() < 30_000
}

async function refreshAccess(): Promise<void> {
  await loadSession()
  if (!refreshToken) {
    throw Object.assign(new Error(codeMessages.AUTH_REQUIRED), { code: 'SESSION_INVALID' })
  }
  const base = requireServerAddress()
  const response = await fetch(`${base}/api/v2/auth/refresh`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ refresh_token: refreshToken }),
    signal: AbortSignal.timeout(20_000),
  })
  const text = await response.text()
  const payload = text ? (JSON.parse(text) as AuthSession & { code?: string }) : {}
  if (!response.ok) {
    clearCredentials()
    await persistSession()
    throw Object.assign(new Error(codeMessages.AUTH_REQUIRED), { code: 'SESSION_INVALID' })
  }
  await storeSession(payload)
}

/**
 * 会话检查不再依赖"服务是否初始化"。v2 只有三种结果：
 * 没有地址、凭据无效需要重新登录、凭据有效可以进入工作空间。
 */
async function inspectSession(): Promise<{
  phase: 'address' | 'offline' | 'login' | 'ready'
  message?: string
  serverUrl: string
  username?: string
}> {
  await loadSession()
  const choice = selectServerAddress(savedServerAddress, process.env.TOMBOT_SERVER_URL)
  if (!choice.connect) return { phase: 'address', serverUrl: choice.prefill }
  if (!accessToken && !refreshToken) {
    return { phase: 'login', serverUrl: choice.connect }
  }
  try {
    if (accessIsStale()) await refreshAccess()
    const session = (await call('GET', '/auth/session')) as {
      principal?: { user_id?: string }
    }
    if (!session.principal?.user_id) {
      throw Object.assign(new Error(codeMessages.AUTH_REQUIRED), { code: 'SESSION_INVALID' })
    }
    return { phase: 'ready', serverUrl: choice.connect, username }
  } catch (error) {
    const coded = error as { code?: string }
    if (coded.code === 'SESSION_INVALID' || coded.code === 'AUTH_INVALID') {
      return { phase: 'login', serverUrl: choice.connect }
    }
    return { phase: 'offline', message: failure(error).message, serverUrl: choice.connect }
  }
}

function failure(error: unknown): Failure {
  const coded = error as { code?: string; message?: string }
  const code = coded.code || 'REQUEST_FAILED'
  return { code, message: codeMessages[code] ?? coded.message ?? '服务暂时无法完成这个操作。' }
}

interface ApiFailure {
  code?: string
  message?: string
  request_id?: string
}

/** 发送一次请求；非 v2 错误格式按未分类失败处理，不透出服务端正文。 */
async function send(
  method: string,
  path: string,
  body: unknown,
  timeout: number,
  signal: AbortSignal | undefined,
): Promise<{ status: number; payload: unknown }> {
  const base = requireServerAddress()
  let response: Response
  try {
    response = await fetch(`${base}/api/v2${path}`, {
      method,
      headers: {
        'content-type': 'application/json',
        ...(accessToken ? { authorization: `Bearer ${accessToken}` } : {}),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: combineSignals(signal, AbortSignal.timeout(timeout)),
    })
  } catch (error) {
    if (signal?.aborted) throw error
    if (error instanceof Error && error.name === 'AbortError') {
      throw Object.assign(new Error('连接服务超时。'), { code: 'OFFLINE' })
    }
    throw Object.assign(new Error(codeMessages.OFFLINE), { code: 'OFFLINE' })
  }
  const text = await response.text()
  let payload: unknown = {}
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      throw Object.assign(new Error('服务返回了无法识别的内容。'), { code: 'RESPONSE_INVALID' })
    }
  }
  return { status: response.status, payload }
}

function failureOf(payload: unknown, status: number): Failure {
  const failure = (payload ?? {}) as ApiFailure
  const code = failure.code ?? (status === 401 ? 'SESSION_INVALID' : 'REQUEST_FAILED')
  const serverMessage = failure.message
  const message =
    codeMessages[code] ??
    (serverMessage && /[\u4e00-\u9fff]/.test(serverMessage)
      ? serverMessage
      : '服务暂时无法完成这个操作。')
  return { code, message }
}

/**
 * 业务请求：访问凭据过期时用刷新凭据换一次新凭据后重试一次。
 * 刷新失败或重试仍返回 401 时清空本地凭据，让界面回到登录页，不能带着失效凭据继续请求。
 */
async function call(
  method: string,
  path: string,
  body?: unknown,
  timeout = 20_000,
  signal?: AbortSignal,
): Promise<unknown> {
  await loadSession()
  if (accessIsStale()) await refreshAccess()
  let result = await send(method, path, body, timeout, signal)
  if (result.status === 401) {
    const failure = failureOf(result.payload, 401)
    if (failure.code !== 'SESSION_INVALID')
      throw Object.assign(new Error(failure.message), { code: failure.code })
    await refreshAccess()
    result = await send(method, path, body, timeout, signal)
  }
  if (result.status >= 400) {
    const failure = failureOf(result.payload, result.status)
    throw Object.assign(new Error(failure.message), { code: failure.code })
  }
  return result.payload
}

async function handle<T>(task: () => Promise<T>): Promise<CallResult<T>> {
  try {
    return { ok: true, value: await task() }
  } catch (error) {
    return { ok: false, ...failure(error) }
  }
}

function combineSignals(parent: AbortSignal | undefined, timeout: AbortSignal): AbortSignal {
  if (!parent) return timeout
  const controller = new AbortController()
  const abort = () => controller.abort()
  if (parent.aborted || timeout.aborted) {
    controller.abort()
    return controller.signal
  }
  parent.addEventListener('abort', abort, { once: true })
  timeout.addEventListener('abort', abort, { once: true })
  return controller.signal
}

/** 事件游标按服务端序号单调推进；重连从上次已确认的位置继续。 */
let eventCursor = 0

/**
 * 订阅当前用户事件。服务端在到期前主动收尾，这里按 retry 提示自动续订；
 * 只有凭据失效或订阅被取消才停止。事件只作为"重新读取"的提示，不携带正文。
 */
async function pumpEvents(event: IpcMainInvokeEvent, signal: AbortSignal): Promise<void> {
  while (!signal.aborted) {
    let response: Response
    try {
      await loadSession()
      if (accessIsStale()) await refreshAccess()
      response = await fetch(`${requireServerAddress()}/api/v2/events?after=${eventCursor}`, {
        headers: {
          accept: 'text/event-stream',
          ...(accessToken ? { authorization: `Bearer ${accessToken}` } : {}),
        },
        signal,
      })
    } catch (error) {
      if (signal.aborted || (error instanceof Error && error.name === 'AbortError')) return
      await new Promise((resolve) => setTimeout(resolve, 2_000))
      continue
    }
    if (response.status === 401) {
      try {
        await refreshAccess()
      } catch {
        return
      }
      continue
    }
    if (
      !response.ok ||
      !(response.headers.get('content-type') ?? '').includes('text/event-stream')
    ) {
      await new Promise((resolve) => setTimeout(resolve, 5_000))
      continue
    }
    const reader = response.body?.getReader()
    if (!reader) return
    const decoder = new TextDecoder()
    let buffer = ''
    let cursorExpired = false
    while (!signal.aborted) {
      const chunk = await reader.read()
      if (chunk.done) break
      buffer += decoder.decode(chunk.value, { stream: true })
      const blocks = buffer.split('\n\n')
      buffer = blocks.pop() ?? ''
      for (const block of blocks) {
        if (block.startsWith(':') || block.startsWith('retry:')) continue
        const sequence = /^id: (\d+)$/m.exec(block)?.[1]
        const data = block
          .split('\n')
          .filter((line) => line.startsWith('data: '))
          .map((line) => line.slice(6))
          .join('\n')
        if (!data) continue
        if (block.startsWith('event: error')) {
          // 游标早于服务端清理边界时，缺失的事件已经删除；必须重新读取快照。
          // 只跳过这一帧会让本地状态永久落后于服务端。
          let code = ''
          try {
            code = (JSON.parse(data) as { code?: string }).code ?? ''
          } catch (error) {
            // 错误帧本身不可解析时按普通失败处理，由外层退避后重新订阅。
            console.error('事件错误帧无法解析', error)
          }
          if (code === 'EVENT_CURSOR_EXPIRED') cursorExpired = true
          break
        }
        if (!sequence) continue
        try {
          const parsed = JSON.parse(data) as UserEvent
          eventCursor = Math.max(eventCursor, Number(sequence))
          if (!event.sender.isDestroyed()) event.sender.send('desktop:user-event', parsed)
        } catch {
          // 单个损坏帧不终止订阅；服务端序号仍会推进，下次续订不会重复消费。
        }
      }
      if (cursorExpired) break
    }
    if (cursorExpired) {
      // 快照会覆盖清理边界内缺失的变更；游标随之推进到快照水位后重新续订。
      await reader.cancel().catch(() => undefined)
      await loadWorkspace()
      continue
    }
    // 连接被服务端正常收尾时立即续订，交由其 retry 提示控制节奏。
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
}

/** 注册渲染进程可调用的服务通道。令牌只留在主进程，页面拿不到。 */
export function registerDesktopIpc(): void {
  ipcMain.handle('desktop:session', () => handle(() => inspectSession()))
  ipcMain.handle('desktop:connect', (_event, input: unknown) =>
    handle(async () => {
      await loadSession()
      const next = normalizeServerAddress(typeof input === 'string' ? input : '')
      if (next !== savedServerAddress) {
        // 凭据属于上一台服务，换地址后不能继续带过去。
        clearCredentials()
        savedServerAddress = next
        await persistSession()
      }
      return inspectSession()
    }),
  )
  ipcMain.handle('desktop:login', (_event, input: LoginInput) =>
    handle(() => login(typeof input === 'string' ? { username: '', password: input } : input)),
  )
  ipcMain.handle('desktop:browser-login', () => handle(() => beginBrowserLogin()))
  ipcMain.handle('desktop:logout', () => handle(() => logout()))
  ipcMain.handle('desktop:load-workspace', () => handle(() => loadWorkspace()))
  ipcMain.handle('desktop:clipboard-files', () => handle(readClipboardFiles))
  ipcMain.handle('desktop:save-workspace', (_event, workspace: AppWorkspace) =>
    handle(() => saveWorkspace(workspace)),
  )
  ipcMain.handle('desktop:watch-events', (event) =>
    handle(async () => {
      const controller = new AbortController()
      const detach = () => controller.abort()
      event.sender.once('destroyed', detach)
      try {
        await pumpEvents(event, controller.signal)
        return null
      } finally {
        event.sender.removeListener('destroyed', detach)
      }
    }),
  )
  ipcMain.handle(
    'desktop:deliver',
    (event, input: { agentId: string; content: string; requestId: string }) =>
      handle(async () => {
        try {
          return await deliverMessage(event, input)
        } catch (error) {
          const coded = error as { code?: string; message?: string }
          if (coded.code === 'MODEL_NOT_READY') {
            return { code: coded.code, message: coded.message }
          }
          throw error
        }
      }),
  )
  ipcMain.handle('desktop:load-settings', () => handle(() => call('GET', '/model-settings')))
  ipcMain.handle('desktop:model-providers', () =>
    handle(async () => {
      const listed = (await call('GET', '/model-providers')) as Array<{ id: string; name: string }>
      return listed
    }),
  )
  ipcMain.handle(
    'desktop:provider-models',
    (_event, input: { providerId: string; apiKey: string }) =>
      handle(async () => {
        const result = (await call(
          'POST',
          `/model-providers/${encodeURIComponent(input.providerId)}/models`,
          { apiKey: input.apiKey },
        )) as { models?: Array<{ id: string; name: string }> }
        return result.models ?? []
      }),
  )
  ipcMain.handle('desktop:introduction', (_event, agentId: string) =>
    handle(() =>
      call(
        'POST',
        `/workspace/agents/${encodeURIComponent(agentId)}/introduction`,
        undefined,
        30_000,
      ),
    ),
  )
  ipcMain.handle('desktop:routines', (_event, agentId: string) =>
    handle(async () => {
      const rows = (await call('GET', `/routines?bot_id=${encodeURIComponent(agentId)}`)) as Array<{
        id: string
        name: string
        cron: string
        prompt: string
        is_enabled: boolean
        revision: number
        target?: { bot_id?: string }
      }>
      return (Array.isArray(rows) ? rows : []).filter((item) => item.target?.bot_id === agentId)
    }),
  )
  ipcMain.handle(
    'desktop:routine-enabled',
    (_event, input: { id: string; revision: number; enabled: boolean }) =>
      handle(() =>
        call('PATCH', `/routines/${encodeURIComponent(input.id)}`, {
          revision: input.revision,
          is_enabled: input.enabled,
        }),
      ),
  )
  ipcMain.handle('desktop:routine-delete', (_event, input: { id: string; revision: number }) =>
    handle(() =>
      call('DELETE', `/routines/${encodeURIComponent(input.id)}?revision=${input.revision}`),
    ),
  )
  ipcMain.handle('desktop:groups', () =>
    handle(async () => {
      const response = (await call('GET', '/groups?limit=50')) as { groups?: ServerGroup[] }
      return (response.groups ?? []).map((group) => ({
        id: group.id,
        name: group.name,
        revision: group.revision,
        conversation_id: group.conversation_id,
        // v2 不单独保存群头像；用成员头像拼出稳定预览。
        avatar_presets: group.member_ids.flatMap((botID) => {
          const avatar = botAvatars.get(botID)
          return avatar ? [avatar] : []
        }),
      }))
    }),
  )
  ipcMain.handle('desktop:group-create', (_event, input: { name: string; memberIds: string[] }) =>
    handle(() =>
      call('POST', '/groups', {
        client_request_id: ulid(),
        name: input.name,
        purpose: '',
        member_ids: input.memberIds,
      }),
    ),
  )
  ipcMain.handle(
    'desktop:group-rename',
    (_event, input: { id: string; revision: number; name: string }) =>
      handle(async () => {
        // v2 用整体替换保存群组；先读当前成员再连同新名称提交，避免误删成员。
        const current = await readGroupSpec(input.id)
        return call('PUT', `/groups/${encodeURIComponent(input.id)}`, {
          expected_revision: input.revision,
          ...current,
          name: input.name,
        })
      }),
  )
  // 群组产物属于 v1 语义，v2 尚无对应能力；明确拒绝而不是返回空列表。
  ipcMain.handle('desktop:group-artifacts', () =>
    handle(async () => {
      throw Object.assign(new Error('群组资料库暂不可用。'), { code: 'NOT_AVAILABLE' })
    }),
  )
  ipcMain.handle('desktop:group', (_event, id: string) => handle(() => groupDetail(id)))
  ipcMain.handle(
    'desktop:group-members',
    (_event, input: { id: string; revision: number; botIds: string[] }) =>
      handle(async () => {
        const current = await readGroupSpec(input.id)
        return call('PUT', `/groups/${encodeURIComponent(input.id)}`, {
          expected_revision: input.revision,
          ...current,
          member_ids: input.botIds,
        })
      }),
  )
  ipcMain.handle(
    'desktop:group-member-remove',
    (_event, input: { id: string; revision: number; botId: string }) =>
      handle(() =>
        call(
          'DELETE',
          `/groups/${encodeURIComponent(input.id)}/members/${encodeURIComponent(input.botId)}?revision=${input.revision}`,
        ),
      ),
  )
  ipcMain.handle(
    'desktop:conversation-messages',
    (_event, input: { conversationId: string; before?: number }) =>
      handle(() => listMessagesPage(input.conversationId, input.before ?? 0)),
  )
  ipcMain.handle(
    'desktop:conversation-send',
    (
      event,
      input: {
        requestId: string
        conversationId: string
        content: string
        attachments: Array<{ path: string; sha256: string }>
        replyToId?: string
        mentions: string[]
      },
    ) =>
      handle(() =>
        readConversationStream(
          event,
          input.conversationId,
          {
            content: input.content,
            mentions: input.mentions,
            attachments: input.attachments,
            ...(input.replyToId ? { reply_to_id: input.replyToId } : {}),
          },
          input.requestId,
        ),
      ),
  )
  ipcMain.handle(
    'desktop:upload',
    (
      _event,
      input: {
        name: string
        type: string
        bytes: Uint8Array
        botId?: string
        shared?: boolean
      },
    ) => handle(() => uploadFile(input)),
  )
  ipcMain.handle('desktop:preview', (_event, input: { path: string; botId?: string }) =>
    handle(() => previewFile(input.path, input.botId)),
  )
  ipcMain.handle('desktop:reaction', (_event, input: { messageId: string; emoji: string }) =>
    handle(() =>
      call('PUT', `/messages/${encodeURIComponent(input.messageId)}/reaction`, {
        emoji: input.emoji,
      }),
    ),
  )
  ipcMain.handle('desktop:ensure-agent', (_event, agentId: string) =>
    handle(() => call('POST', `/workspace/agents/${encodeURIComponent(agentId)}/ready`)),
  )
  ipcMain.handle('desktop:computer', (_event, agentId: string) =>
    handle(async () => {
      const result = (await call('POST', '/computer', { agentId })) as { url: string }
      return result.url
    }),
  )
  ipcMain.handle(
    'desktop:save-settings',
    (_event, input: { draft: Record<string, unknown>; apiKey: string }) =>
      handle(() => {
        const draft = input.draft
        return call('PUT', '/model-settings', {
          language: draft.language,
          provider: draft.provider,
          providerName: draft.providerName,
          baseUrl: draft.baseUrl,
          modelName: draft.modelName,
          ...(input.apiKey ? { apiKey: input.apiKey } : {}),
        })
      }),
  )
}

/**
 * 投递一条用户消息：先受理成持久运行，再轮询到终态，最后读回会话消息。
 * v2 没有逐字流式接口，模型输出只能整段返回，因此这里不发送 delta 事件。
 */
async function deliverMessage(
  event: IpcMainInvokeEvent,
  input: { agentId: string; content: string; requestId: string },
): Promise<unknown> {
  const conversationID = await conversationFor(input.agentId)
  const accepted = (await call(
    'POST',
    `/conversations/${encodeURIComponent(conversationID)}/messages`,
    {
      client_request_id: ulid(),
      content: input.content,
    },
  )) as { submission?: { run_id?: string } }
  const runID = accepted.submission?.run_id
  if (!runID) throw Object.assign(new Error('服务没有返回运行标识。'), { code: 'RESPONSE_INVALID' })
  event.sender.send('desktop:stream-event', {
    requestId: input.requestId,
    type: 'process',
    text: '正在处理',
    runId: runID,
    isWorking: true,
  })
  const run = await waitForRun(runID)
  if (run.status !== 'succeeded') {
    throw Object.assign(new Error(runError(run.error_code)), {
      code: run.error_code || 'RUN_FAILED',
    })
  }
  const messages = await listConversation(conversationID)
  return { messages }
}

/** 轮询运行到终态；取消与失败都按服务端给出的状态收尾，不在这里猜测结果。 */
async function waitForRun(runID: string): Promise<{ status: string; error_code?: string }> {
  const deadline = Date.now() + 10 * 60_000
  while (Date.now() < deadline) {
    const result = (await call('GET', `/runs/${encodeURIComponent(runID)}`)) as {
      run?: { status?: string; error_code?: string }
    }
    const status = result.run?.status ?? ''
    if (
      status === 'succeeded' ||
      status === 'failed' ||
      status === 'canceled' ||
      status === 'unknown'
    ) {
      return { status, error_code: result.run?.error_code }
    }
    await new Promise((resolve) => setTimeout(resolve, 800))
  }
  throw Object.assign(new Error('运行超过等待时限，请稍后重新读取结果。'), { code: 'RUN_TIMEOUT' })
}

function runError(code: string | undefined): string {
  const messages: Record<string, string> = {
    CANCELED: '这次运行已取消。',
    MODEL_FAILED: '模型调用失败，请稍后重试。',
    MODEL_NOT_CONFIGURED: '管理员尚未配置可用模型。',
    COMPUTER_UNAVAILABLE: '这台机器人暂时没有可用电脑。',
    TOOL_RESULT_UNKNOWN: '工具执行结果未知，已停止继续操作。',
  }
  return (code && messages[code]) || '运行没有成功完成。'
}

/** 机器人到会话的映射可能还没加载；找不到时重新读取一次目录。 */
async function conversationFor(agentID: string): Promise<string> {
  const known = conversations.get(agentID)
  if (known) return known
  await loadWorkspace()
  const refreshed = conversations.get(agentID)
  if (!refreshed)
    throw Object.assign(new Error('找不到这台机器人的单聊会话。'), { code: 'CONVERSATION_MISSING' })
  return refreshed
}

/** 一页消息数量；服务端单页上限是 100，取小值保证首屏加载快且能继续向前翻。 */
const messagePageSize = 80

/**
 * 读取会话消息的一页。服务端按序号倒序返回，界面按时间正序展示，因此这里翻转一次。
 * before 为空表示读取最新一页；返回的 hasMore 由服务端是否给满整页决定，
 * 客户端据此决定是否显示"加载更早"，不猜测剩余数量。
 */
async function listMessagesPage(
  conversationID: string,
  before = 0,
): Promise<{ messages: unknown[]; hasMore: boolean }> {
  const query = new URLSearchParams({ limit: String(messagePageSize) })
  if (before > 0) query.set('before', String(before))
  const response = (await call(
    'GET',
    `/conversations/${encodeURIComponent(conversationID)}/messages?${query.toString()}`,
  )) as { messages?: ServerMessage[] }
  const rows = response.messages ?? []
  const messages = rows
    .flatMap((row) => {
      const bubble = toBubble(row)
      return bubble ? [bubble] : []
    })
    .reverse()
  return { messages, hasMore: rows.length === messagePageSize }
}

async function listConversation(conversationID: string): Promise<unknown[]> {
  return (await listMessagesPage(conversationID)).messages
}

/** 读取群组的可写字段；v2 的保存是整体替换，改名和成员调整都要带上原有内容。 */
async function readGroupSpec(
  id: string,
): Promise<{ name: string; purpose: string; member_ids: string[] }> {
  const response = (await call('GET', `/groups/${encodeURIComponent(id)}`)) as {
    group?: ServerGroup
  }
  const group = response.group
  if (!group) throw Object.assign(new Error('找不到这个群组。'), { code: 'RESOURCE_NOT_FOUND' })
  return { name: group.name, purpose: group.purpose, member_ids: group.member_ids }
}

/** 群组详情把成员标识补成名字，界面才能显示成员列表。 */
async function groupDetail(id: string): Promise<unknown> {
  const response = (await call('GET', `/groups/${encodeURIComponent(id)}`)) as {
    group?: ServerGroup
  }
  const group = response.group
  if (!group) throw Object.assign(new Error('找不到这个群组。'), { code: 'RESOURCE_NOT_FOUND' })
  const members = group.member_ids.map((botID) => ({ bot_id: botID, name: botName(botID) }))
  return {
    id: group.id,
    name: group.name,
    revision: group.revision,
    conversation_id: group.conversation_id,
    avatar_presets: group.member_ids.flatMap((botID) => {
      const avatar = botAvatars.get(botID)
      return avatar ? [avatar] : []
    }),
    members,
  }
}

/** 群成员名字来自最近一次工作台快照；缺失时回退到标识，不编造名称。 */
function botName(botID: string): string {
  return botNames.get(botID) ?? botID
}

type ServerMessage = {
  id: string
  sequence: number
  sender_kind: string
  sender_bot_id?: string
  created_at: string
  blocks?: Array<{
    kind: string
    text?: string
    artifact?: { path?: string; sha256?: string; media_type?: string }
  }>
  reply_to_id?: string
  reactions?: Array<{ emoji: string; count: number }>
}

function toBubble(message: ServerMessage): {
  id: string
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
} | null {
  const content = (message.blocks ?? [])
    .filter((block) => block.kind === 'text')
    .map((block) => block.text ?? '')
    .join('')
  const attachments = (message.blocks ?? []).flatMap((block) => {
    if (block.kind !== 'artifact' || !block.artifact?.path || !block.artifact.sha256) return []
    const name = block.artifact.path.split('/').pop() || block.artifact.path
    const mediaType = block.artifact.media_type ?? ''
    return [
      {
        name,
        path: block.artifact.path,
        sha256: block.artifact.sha256,
        mediaType,
        kind:
          mediaType === 'image/png' || mediaType === 'image/jpeg'
            ? ('image' as const)
            : ('file' as const),
      },
    ]
  })
  if (!content.trim() && attachments.length === 0) return null
  return {
    id: message.id,
    sequence: message.sequence,
    role: message.sender_kind === 'user' ? 'user' : 'assistant',
    speaker: message.sender_bot_id ?? '',
    content: content.trim() || attachments.map((item) => item.name).join('、'),
    createdAt: message.created_at,
    ...(message.reply_to_id ? { replyToId: message.reply_to_id } : {}),
    reactions: message.reactions ?? [],
    attachments,
  }
}

async function uploadFile(input: {
  name: string
  type: string
  bytes: Uint8Array
  botId?: string
  shared?: boolean
}): Promise<{ path: string; sha256: string }> {
  await loadSession()
  const form = new FormData()
  form.append(
    'file',
    new Blob([Buffer.from(input.bytes)], { type: input.type || 'application/octet-stream' }),
    input.name,
  )
  const query = input.shared ? 'scope=shared' : `bot_id=${encodeURIComponent(input.botId ?? '')}`
  const response = await fetch(`${requireServerAddress()}/api/v1/fs/upload?${query}`, {
    method: 'POST',
    headers: { ...(accessToken ? { authorization: `Bearer ${accessToken}` } : {}) },
    body: form,
    signal: AbortSignal.timeout(60_000),
  })
  const payload = (await response.json()) as {
    path?: string
    sha256?: string
    error?: { message?: string }
  }
  if (!response.ok || !payload.path || !payload.sha256) {
    throw Object.assign(new Error(payload.error?.message ?? '文件没有上传成功。'), {
      code: 'UPLOAD_FAILED',
    })
  }
  return { path: payload.path, sha256: payload.sha256 }
}

async function previewFile(path: string, botId?: string): Promise<string> {
  await loadSession()
  const query = path.startsWith('shared/')
    ? `scope=shared&path=${encodeURIComponent(path)}`
    : `bot_id=${encodeURIComponent(botId ?? '')}&path=${encodeURIComponent(path)}`
  const response = await fetch(`${requireServerAddress()}/api/v1/fs/download?${query}`, {
    headers: { ...(accessToken ? { authorization: `Bearer ${accessToken}` } : {}) },
    signal: AbortSignal.timeout(20_000),
  })
  if (!response.ok) throw Object.assign(new Error('附件暂时无法显示。'), { code: 'PREVIEW_FAILED' })
  const bytes = Buffer.from(await response.arrayBuffer())
  if (bytes.length > 1_500_000)
    throw Object.assign(new Error('图片太大，无法在气泡里预览。'), { code: 'PREVIEW_FAILED' })
  const lower = path.toLowerCase()
  const type = lower.endsWith('.png')
    ? 'image/png'
    : lower.endsWith('.jpg') || lower.endsWith('.jpeg')
      ? 'image/jpeg'
      : 'application/octet-stream'
  return `data:${type};base64,${bytes.toString('base64')}`
}

async function readConversationStream(
  event: IpcMainInvokeEvent,
  conversationId: string,
  body: unknown,
  requestId: string,
): Promise<unknown> {
  await loadSession()
  const response = await fetch(
    `${requireServerAddress()}/api/v1/conversations/${encodeURIComponent(conversationId)}/messages`,
    {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        accept: 'text/event-stream',
        ...(accessToken ? { authorization: `Bearer ${accessToken}` } : {}),
      },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(90_000),
    },
  )
  const type = response.headers.get('content-type') ?? ''
  if (!type.includes('text/event-stream')) {
    const text = await response.text()
    const payload = text
      ? (JSON.parse(text) as { error?: { code?: string; message?: string } })
      : {}
    if (!response.ok) {
      throw Object.assign(new Error(payload.error?.message ?? '服务暂时无法完成这个操作。'), {
        code: payload.error?.code ?? 'REQUEST_FAILED',
      })
    }
    return payload
  }
  return readEventStream(event, response, requestId)
}

async function readEventStream(
  event: IpcMainInvokeEvent,
  response: Response,
  requestId: string,
): Promise<unknown> {
  const reader = response.body?.getReader()
  if (!reader)
    throw Object.assign(new Error('服务没有返回运行过程。'), { code: 'RESPONSE_INVALID' })
  const decoder = new TextDecoder()
  let buffer = ''
  let donePayload: unknown
  const push = async (block: string) => {
    const name = /^event: (\S+)/m.exec(block)?.[1]
    const data = block
      .split('\n')
      .filter((line) => line.startsWith('data: '))
      .map((line) => line.slice(6))
      .join('\n')
    if (!name || !data) return
    const parsed = JSON.parse(data) as {
      text?: string
      code?: string
      message?: string
      kind?: string
      message_id?: string
      bot_id?: string
      run_id?: string
      is_working?: boolean
      is_finished?: boolean
      to_bot_id?: string
      to_name?: string
      conversation_id?: string
    }
    if (name === 'delta' || name === 'process' || name === 'activity') {
      event.sender.send('desktop:stream-event', {
        requestId,
        type: name,
        text: parsed.text ?? '',
        runId: parsed.run_id,
        isWorking: parsed.is_working,
        isFinished: parsed.is_finished,
        messageId: parsed.message_id,
        botId: parsed.bot_id,
      })
    }
    if ((name === 'dm' || name === 'user_message') && parsed.message_id) {
      const snapshot = (await call('GET', '/workspace')) as {
        workspace: {
          agents: Array<{
            id: string
            messages: Array<{ id: string; dmNotice?: { conversationId: string } }>
          }>
        }
      }
      const incoming = snapshot.workspace.agents.flatMap((agent) => {
        const messages = agent.messages.filter((message) =>
          name === 'user_message'
            ? message.id === parsed.message_id
            : message.dmNotice?.conversationId === parsed.conversation_id,
        )
        return messages.length ? [{ id: agent.id, messages }] : []
      })
      event.sender.send('desktop:stream-event', {
        requestId,
        type: 'incoming',
        text: '',
        incoming,
      })
    }
    if (name === 'done') donePayload = parsed
    if (name === 'error') {
      throw Object.assign(new Error(parsed.message ?? 'agent 运行失败。'), {
        code: parsed.code ?? 'MODEL_FAILED',
      })
    }
  }
  while (true) {
    const chunk = await reader.read()
    if (chunk.done) break
    buffer += decoder.decode(chunk.value, { stream: true })
    const blocks = buffer.split('\n\n')
    buffer = blocks.pop() ?? ''
    for (const block of blocks) await push(block)
  }
  if (buffer.trim()) await push(buffer)
  if (!donePayload)
    throw Object.assign(new Error('agent 没有返回结果。'), { code: 'RESPONSE_INVALID' })
  return donePayload
}

interface LoginInput {
  username: string
  password: string
  code?: string
}

/**
 * 浏览器登录：在本机回环端口等待一次性回调，用系统浏览器完成登录，
 * 再用授权码 + PKCE 校验串换取会话。口令不经过桌面进程。
 */
async function beginBrowserLogin(): Promise<{ phase: 'ready'; username?: string }> {
  await loadSession()
  const state = Buffer.from(await randomBytes(32)).toString('base64url')
  const verifier = Buffer.from(await randomBytes(32)).toString('base64url')
  const challenge = createHash('sha256').update(verifier).digest('base64url')
  const callback = await new Promise<{ code: string; state: string }>((resolve, reject) => {
    const server = createServer((request, response) => {
      const url = new URL(request.url ?? '/', 'http://127.0.0.1')
      if (url.pathname !== '/callback') {
        response.writeHead(404).end()
        return
      }
      const code = url.searchParams.get('code')
      const returned = url.searchParams.get('state')
      response.writeHead(200, { 'content-type': 'text/html; charset=utf-8' })
      response.end(
        code && returned === state
          ? '<p>登录成功，请回到桌面客户端。</p>'
          : '<p>登录未完成，请回到桌面客户端重试。</p>',
      )
      server.close()
      if (!code || returned !== state) {
        reject(Object.assign(new Error(codeMessages.AUTH_INVALID), { code: 'AUTH_INVALID' }))
        return
      }
      resolve({ code, state: returned })
    })
    server.on('error', reject)
    // 只监听本机回环，端口由系统分配，避免与他人冲突。
    server.listen(0, '127.0.0.1', async () => {
      try {
        const address = server.address()
        if (!address || typeof address === 'string') throw new Error('无法分配本地回调端口。')
        const redirectURI = `http://127.0.0.1:${address.port}/callback`
        const started = (await call('POST', '/auth/browser/start', {
          redirect_uri: redirectURI,
          state,
          code_challenge: challenge,
          device_name: 'TomBot 桌面',
          platform: devicePlatform(),
        })) as { authorization_url?: string }
        if (!started.authorization_url) throw new Error('服务没有返回授权页地址。')
        await shell.openExternal(`${requireServerAddress()}${started.authorization_url}`)
      } catch (error) {
        server.close()
        reject(error)
      }
    })
    // 用户可能在浏览器里直接关闭页面；超时后释放监听，不能永久等待。
    setTimeout(() => {
      server.close()
      reject(Object.assign(new Error('登录超时，请重新发起。'), { code: 'AUTH_TIMEOUT' }))
    }, 10 * 60_000).unref()
  })
  const result = (await call('POST', '/auth/browser/exchange', {
    code: callback.code,
    code_verifier: verifier,
    state: callback.state,
  })) as AuthSession
  await storeSession(result)
  return { phase: 'ready' }
}

/** 账号登录：用户名和密码由用户输入，动态验证码只在管理员账户要求时提交。 */
async function login(input: LoginInput): Promise<void> {
  await loadSession()
  const result = (await call('POST', '/auth/login', {
    username: input.username.trim(),
    password: input.password,
    ...(input.code ? { code: input.code.trim() } : {}),
    device_name: 'TomBot 桌面',
    platform: devicePlatform(),
  })) as AuthSession
  await storeSession(result)
  username = input.username.trim()
  await persistSession()
}

/** 退出时先撤销服务端会话；即使撤销失败也要清掉本地凭据，不能留下可用令牌。 */
async function logout(): Promise<void> {
  await loadSession()
  try {
    await call('POST', '/auth/logout', {})
  } finally {
    clearCredentials()
    await persistSession()
  }
}
