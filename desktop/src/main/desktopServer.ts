import {
  installationIssue,
  installedEnvironment,
  installClientUpdate,
  type FinishUpdate,
} from './installClientUpdate'
import { checkClientUpdate, saveClientUpdate } from './clientUpdates'
import { loadApplicationAbout } from './applicationAbout'
import { readConversationPreviews } from './conversationPreviews'
import { readGroups, createGroup, changeGroup, sendGroupMessage } from './groupChat'
import { attachmentKind } from '../shared/attachmentMedia'
import { chatFilePath, parseChatFilePath, type ChatFile } from '../shared/chatFiles'
import {
  listChatFiles,
  uploadChatFile,
  previewChatFile,
  previewMessageFile,
  readChatFile,
  renameChatFile,
  deleteChatFile,
} from './chatFiles'
import { computerStatus } from '../shared/computer'
import { listTasks } from './tasks'
import { openConsole, controlConsole } from './console'
import { consoleID } from '../shared/console'
import { ConsoleStreams } from './consoleStream'
import { loginResultPage } from './loginResultPage'
import { app, dialog, ipcMain, shell, type IpcMainInvokeEvent } from 'electron'
import { createHash, randomBytes } from 'node:crypto'
import { chmod, mkdir, readFile, writeFile } from 'node:fs/promises'
import { createServer } from 'node:http'
import { join } from 'node:path'
import { readClipboardFiles } from './clipboardFiles'
import { ServerTransport } from './serverTransport'
import { createBot } from './botCreation'
import {
  draftsFromWorkspace,
  workspaceFromSnapshot,
  workspaceSelection,
  type LocalDrafts,
  type ServerGroup,
  type ServerSnapshot,
} from './workspaceSnapshot'
import type { AppWorkspace, NewAgent } from '../renderer/src/features/workspace/model'
import type { UserEvent, UserProfile } from '../renderer/src/features/desktop/backend'
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
  SESSION_INVALID: '登录状态已失效，请重新在浏览器中登录。',
  AUTH_RATE_LIMITED: '尝试次数过多，请稍后再试。',
  SERVICE_UNAVAILABLE: '服务暂不可用，请稍后重试。',
  BOOTSTRAP_RUNNING: '引导正在进行，请稍候。',
  MODEL_NOT_READY: '当前模型策略不可用，请联系管理员检查服务端模型配置。',
  WORKSPACE_CONFLICT: '工作空间刚被更新，请重新发送。',
  OFFLINE: '无法连接服务。请确认地址和网络后重试。',
  SERVER_URL_MISSING: '还没有填写服务地址。',
  AUTH_PENDING: '正在验证服务或等待浏览器登录，请完成后再操作。',
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
// 主进程也锁住服务切换，避免页面重载后绕过等待状态，将授权发往另一台服务。
let isBrowserLoginPending = false
let isCertificateTrustPending = false
let serverTransport: ServerTransport | undefined

function transport(): ServerTransport {
  return (serverTransport ??= new ServerTransport(join(app.getPath('userData'), 'server-trust')))
}
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

const consoleStreams = new ConsoleStreams(async (id) => {
  await controlConsole(call, consoleID.parse(id), 'heartbeat')
  if (!accessToken) throw new Error('请先登录。')
  return transport().websocket(
    `${requireServerAddress()}/api/v2/consoles/${id}/stream`,
    `Bearer ${accessToken}`,
  )
})

function clearCredentials(): void {
  consoleStreams.closeAll()
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
  principal?: { user_id?: string; role?: string; username?: string }
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
  // 目录最多一百项，只有自定义头像需要额外读取，每批最多四个请求。
  const customBots = snapshot.bots.filter((bot) => !bot.is_archived && bot.has_custom_avatar)
  for (let index = 0; index < customBots.length; index += 4) {
    await Promise.all(
      customBots.slice(index, index + 4).map(async (bot) => {
        const avatar = (await call('GET', `/bots/${encodeURIComponent(bot.id)}/avatar`)) as {
          content: string
        }
        const agent = mapped.workspace.agents.find((item) => item.id === bot.id)
        if (agent) agent.customAvatar = avatar.content
      }),
    )
  }
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
  username = session.principal?.username ?? ''
  await persistSession()
}

/** 访问凭据临近到期才刷新，避免每个请求都触发一次轮换。 */
function accessIsStale(): boolean {
  if (!accessExpiresAt) return true
  const expiry = Date.parse(accessExpiresAt)
  if (Number.isNaN(expiry)) return true
  return expiry - Date.now() < 30_000
}

let pendingRefresh: { address: string; task: Promise<void> } | undefined

/** 并发历史、状态和事件请求共享一次续期，防止一次性刷新凭据被重复消费。 */
async function refreshAccess(): Promise<void> {
  await loadSession()
  const address = requireServerAddress()
  if (pendingRefresh?.address === address) return pendingRefresh.task
  const task = renewAccess(address)
  const pending = { address, task }
  pendingRefresh = pending
  try {
    await task
  } finally {
    if (pendingRefresh === pending) pendingRefresh = undefined
  }
}

async function renewAccess(base: string): Promise<void> {
  if (!refreshToken) {
    throw Object.assign(new Error(codeMessages.SESSION_INVALID), { code: 'SESSION_INVALID' })
  }
  const original = refreshToken
  const assertCurrentSession = () => {
    if (base !== savedServerAddress || original !== refreshToken)
      throw Object.assign(new Error('服务或登录状态已变化，请重试。'), { code: 'SERVER_CHANGED' })
  }
  const requireLogin = async () => {
    assertCurrentSession()
    clearCredentials()
    await persistSession()
    throw Object.assign(new Error(codeMessages.SESSION_INVALID), { code: 'SESSION_INVALID' })
  }
  let response: Response
  try {
    response = await transport().fetch(`${base}/api/v2/auth/refresh`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ refresh_token: original }),
      signal: AbortSignal.timeout(20_000),
    })
  } catch (error) {
    // 只有确认请求未发出才能继续使用旧续期凭据。响应丢失时重放会撤销会话。
    if ((error as { isRequestUnsent?: boolean } | null)?.isRequestUnsent) throw error
    return requireLogin()
  }
  let payload: AuthSession & ApiFailure
  try {
    payload = (await response.json()) as AuthSession & ApiFailure
  } catch {
    return requireLogin()
  }
  assertCurrentSession()
  if (!response.ok) {
    // 服务明确拒绝进入续期时仍可使用旧凭据；代理错误无法确认服务是否已经轮换。
    if (
      response.status === 429 ||
      (response.status === 503 && payload?.code === 'IDENTITY_UNAVAILABLE')
    ) {
      const { code, message } = failureOf(payload, response.status)
      throw Object.assign(new Error(message), { code })
    }
    return requireLogin()
  }
  if (!payload?.access_token || !payload.refresh_token) return requireLogin()
  await storeSession(payload)
}

/**
 * 未登录时先确认服务地址；只有本次连接明确确认过地址，才进入登录页。
 * 有效会话仍可直接恢复工作空间。
 */
async function inspectSession(hasConfirmedAddress = false): Promise<{
  phase: 'address' | 'offline' | 'login' | 'ready'
  message?: string
  errorCode?: string
  serverUrl: string
  username?: string
}> {
  await loadSession()
  const choice = selectServerAddress(savedServerAddress, process.env.TOMBOT_SERVER_URL)
  if (!choice.connect) return { phase: 'address', serverUrl: choice.prefill }
  if (!accessToken && !refreshToken) {
    return { phase: hasConfirmedAddress ? 'login' : 'address', serverUrl: choice.connect }
  }
  try {
    if (accessIsStale()) await refreshAccess()
    const session = (await call('GET', '/auth/session')) as {
      principal?: { user_id?: string }
    }
    if (!session.principal?.user_id) {
      throw Object.assign(new Error(codeMessages.SESSION_INVALID), { code: 'SESSION_INVALID' })
    }
    return { phase: 'ready', serverUrl: choice.connect, username }
  } catch (error) {
    const coded = error as { code?: string }
    if (coded.code === 'SESSION_INVALID' || coded.code === 'AUTH_INVALID') {
      return { phase: hasConfirmedAddress ? 'login' : 'address', serverUrl: choice.connect }
    }
    const failed = failure(error)
    return {
      phase: 'offline',
      message: failed.message,
      errorCode: failed.code,
      serverUrl: choice.connect,
    }
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
  authenticated = true,
): Promise<{ status: number; payload: unknown }> {
  const base = requireServerAddress()
  const response = await transport().fetch(`${base}/api/v2${path}`, {
    method,
    headers: {
      'content-type': 'application/json',
      ...(authenticated && accessToken ? { authorization: `Bearer ${accessToken}` } : {}),
    },
    credentials: 'omit',
    redirect: 'error',
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: combineSignals(signal, AbortSignal.timeout(timeout)),
  })
  if (
    response.ok &&
    method === 'GET' &&
    /^\/bots\/[0-7][0-9A-HJKMNP-TV-Z]{25}\/avatar$/.test(path)
  ) {
    return { status: response.status, payload: { content: await readAvatar(response) } }
  }
  let text: string
  try {
    text = await response.text()
  } catch {
    throw Object.assign(new Error(codeMessages.OFFLINE), { code: 'OFFLINE' })
  }
  let payload: unknown = {}
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      if (response.status >= 500) return { status: response.status, payload: {} }
      throw Object.assign(new Error('服务返回了无法识别的内容。'), { code: 'RESPONSE_INVALID' })
    }
  }
  return { status: response.status, payload }
}

/** 头像接口返回小尺寸 PNG；限制读取大小，避免将异常响应当成图片或无界加载。 */
async function readAvatar(response: Response): Promise<string> {
  const reader = response.body?.getReader()
  if (!reader) throw new Error('服务没有返回猫咪头像。')
  const chunks: Uint8Array[] = []
  let size = 0
  try {
    if (response.headers.get('content-type')?.split(';')[0] !== 'image/png')
      throw new Error('猫咪头像格式无效。')
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      size += value.length
      if (size > 72 * 1024) throw new Error('猫咪头像超过大小限制。')
      chunks.push(value)
    }
  } finally {
    await reader.cancel()
    reader.releaseLock()
  }
  const bytes = Buffer.concat(chunks)
  if (bytes.subarray(0, 8).toString('hex') !== '89504e470d0a1a0a')
    throw new Error('猫咪头像格式无效。')
  return `data:image/png;base64,${bytes.toString('base64')}`
}

function failureOf(payload: unknown, status: number): Failure {
  const failure = (payload ?? {}) as ApiFailure
  let code = failure.code || 'REQUEST_FAILED'
  if (!failure.code) {
    if (status === 401) code = 'SESSION_INVALID'
    if (status === 429) code = 'AUTH_RATE_LIMITED'
    if (status >= 500) code = 'SERVICE_UNAVAILABLE'
  }
  const serverMessage = failure.message
  const message =
    codeMessages[code] ??
    (serverMessage && /[\u4e00-\u9fff]/.test(serverMessage)
      ? serverMessage
      : '服务暂时无法完成这个操作。')
  return { code, message }
}

/** 登录与授权码交换发生在会话建立之前，不携带旧凭据，也不触发续期或自动重发。 */
async function authenticate(
  path: '/auth/login' | '/auth/browser/start' | '/auth/browser/exchange',
  body: unknown,
): Promise<unknown> {
  await loadSession()
  const result = await send('POST', path, body, 20_000, undefined, false)
  if (result.status >= 400) {
    const failed = failureOf(result.payload, result.status)
    throw Object.assign(new Error(failed.message), { code: failed.code })
  }
  return result.payload
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
const sessionsNeedingEventRefresh = new WeakSet<IpcMainInvokeEvent['sender']>()
const EVENT_RETRY_LIMIT = 5

/** 退避期间也能取消订阅，以免页面重载后留下旧订阅和计时器。 */
function waitForEventRetry(delay: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve()
  return new Promise((resolve) => {
    const finish = () => {
      clearTimeout(timer)
      signal.removeEventListener('abort', finish)
      resolve()
    }
    const timer = setTimeout(finish, delay)
    signal.addEventListener('abort', finish, { once: true })
  })
}

/**
 * 订阅当前用户事件。服务端正常收尾时续订，连续失败后交给会话探测重新启动。
 * 恢复后通知界面读取最新状态，同时仍按上次已确认的游标接收漏掉的事件。
 */
async function pumpEvents(
  event: IpcMainInvokeEvent,
  watchId: string,
  signal: AbortSignal,
): Promise<void> {
  const { sender } = event
  const publishRefresh = () => {
    if (signal.aborted || sender.isDestroyed() || !sessionsNeedingEventRefresh.delete(sender))
      return
    sender.send(
      'desktop:user-event',
      {
        sequence: eventCursor,
        kind: 'workspace.refresh',
        resource_id: '',
        payload: {},
        created_at: new Date().toISOString(),
      } satisfies UserEvent,
      watchId,
    )
  }
  let failures = 0
  while (!signal.aborted) {
    try {
      await loadSession()
      if (accessIsStale()) await refreshAccess()
      const previousAccess = accessToken
      const response = await transport().fetch(
        `${requireServerAddress()}/api/v2/events?after=${eventCursor}`,
        {
          headers: {
            accept: 'text/event-stream',
            ...(accessToken ? { authorization: `Bearer ${accessToken}` } : {}),
          },
          signal: AbortSignal.any([signal, AbortSignal.timeout(70_000)]),
        },
      )
      if (response.status === 401) {
        await response.body?.cancel()
        if (accessToken === previousAccess) await refreshAccess()
        throw new Error('登录状态已更新，正在恢复消息订阅。')
      }
      if (
        !response.ok ||
        !(response.headers.get('content-type') ?? '').includes('text/event-stream')
      ) {
        await response.body?.cancel()
        throw new Error('暂时无法订阅消息状态。')
      }
      const reader = response.body?.getReader()
      if (!reader) throw new Error('消息订阅未返回内容。')
      let received = false
      let cursorExpired = false
      try {
        const decoder = new TextDecoder()
        let buffer = ''
        while (!signal.aborted) {
          const chunk = await reader.read()
          if (signal.aborted) return
          if (chunk.done) break
          received = true
          buffer += decoder.decode(chunk.value, { stream: true })
          if (buffer.length > 1_048_576) throw new Error('消息事件超过允许大小。')
          const blocks = buffer.split('\n\n')
          buffer = blocks.pop() ?? ''
          for (const block of blocks) {
            if (block.startsWith(':')) failures = 0
            const data = block
              .split('\n')
              .filter((line) => line.startsWith('data: '))
              .map((line) => line.slice(6))
              .join('\n')
            if (!data) continue
            const parsed = JSON.parse(data) as UserEvent & { code?: string }
            if (block.startsWith('event: error')) {
              if (parsed.code !== 'EVENT_CURSOR_EXPIRED') throw new Error('消息订阅被服务中断。')
              cursorExpired = true
              break
            }
            const sequence = Number(/^id: (\d+)$/m.exec(block)?.[1])
            if (
              !Number.isSafeInteger(sequence) ||
              sequence <= 0 ||
              typeof parsed.kind !== 'string' ||
              !parsed.payload
            ) {
              throw new Error('消息事件格式无效。')
            }
            failures = 0
            if (sequence <= eventCursor) continue
            if (!sender.isDestroyed()) sender.send('desktop:user-event', parsed, watchId)
            eventCursor = sequence
          }
          if (cursorExpired) break
          publishRefresh()
        }
      } finally {
        try {
          await reader.cancel()
        } finally {
          reader.releaseLock()
        }
      }
      if (cursorExpired) {
        sessionsNeedingEventRefresh.add(sender)
        await loadWorkspace()
        publishRefresh()
      }
      if (received) failures = 0
      else throw new Error('消息订阅意外结束。')
    } catch (error) {
      if (signal.aborted) return
      const code = (error as { code?: string } | null)?.code
      if (code === 'SESSION_INVALID' || code?.startsWith('CERTIFICATE_')) throw error
      sessionsNeedingEventRefresh.add(sender)
      failures++
      if (failures >= EVENT_RETRY_LIMIT)
        throw Object.assign(
          new Error('实时状态连接中断。服务恢复后会自动重新订阅。', { cause: error }),
          {
            code: 'EVENT_STREAM_UNAVAILABLE',
          },
        )
    }
    await waitForEventRetry(Math.min(8_000, 500 * 2 ** failures), signal)
  }
}

const eventWatches = new Map<string, AbortController>()

/** 注册渲染进程可调用的服务通道。令牌只留在主进程，页面拿不到。 */
export function registerDesktopIpc(finishUpdate: FinishUpdate): void {
  let updateConnection = new AbortController()
  ipcMain.handle('desktop:session', () => handle(() => inspectSession()))
  ipcMain.handle('desktop:connect', (_event, input: unknown) =>
    handle(async () => {
      await loadSession()
      if (isBrowserLoginPending || isCertificateTrustPending) {
        throw Object.assign(new Error(codeMessages.AUTH_PENDING), { code: 'AUTH_PENDING' })
      }
      const next = normalizeServerAddress(typeof input === 'string' ? input : '')
      if (next !== savedServerAddress) {
        // 凭据属于上一台服务，换地址后不能继续带过去。
        clearCredentials()
        updateConnection.abort(new Error('服务地址已变更，请重新检查更新。'))
        updateConnection = new AbortController()
        savedServerAddress = next
        await persistSession()
      }
      return inspectSession(true)
    }),
  )
  ipcMain.handle(
    'desktop:trust-server-certificate',
    (_event, address: unknown, fingerprint: unknown) =>
      handle(async () => {
        await loadSession()
        if (isBrowserLoginPending || isCertificateTrustPending) {
          throw Object.assign(new Error(codeMessages.AUTH_PENDING), { code: 'AUTH_PENDING' })
        }
        if (address !== requireServerAddress()) {
          throw Object.assign(new Error('服务地址已变化，请重新核对当前服务的证书。'), {
            code: 'SERVER_CHANGED',
          })
        }
        isCertificateTrustPending = true
        try {
          await transport().trustCertificate(requireServerAddress(), fingerprint)
        } finally {
          isCertificateTrustPending = false
        }
      }),
  )
  ipcMain.handle('desktop:login', (_event, input: LoginInput) =>
    handle(() => login(typeof input === 'string' ? { username: '', password: input } : input)),
  )
  ipcMain.handle('desktop:browser-login', () =>
    handle(async () => {
      if (isBrowserLoginPending || isCertificateTrustPending) {
        throw Object.assign(new Error(codeMessages.AUTH_PENDING), { code: 'AUTH_PENDING' })
      }
      isBrowserLoginPending = true
      try {
        return await beginBrowserLogin()
      } finally {
        isBrowserLoginPending = false
      }
    }),
  )
  ipcMain.handle('desktop:logout', () => handle(() => logout()))
  const platform = process.platform + '-' + process.arch
  const latest = async (origin: string, signal: AbortSignal) => {
    const release = await checkClientUpdate(transport(), origin, platform, app.getVersion(), signal)
    signal.throwIfAborted()
    return release
  }
  let downloading = false
  ipcMain.handle('desktop:check-update', () =>
    handle(async () => {
      const origin = requireServerAddress()
      const { signal } = updateConnection
      const release = await latest(origin, signal)
      return (
        release && {
          id: release.id,
          version: release.version,
          notes: release.notes,
          installUnavailable: installationIssue(installedEnvironment(), origin, release),
        }
      )
    }),
  )
  async function useUpdatePackage(event: IpcMainInvokeEvent, id: unknown, install: boolean) {
    if (downloading) throw new Error('正在下载客户端安装包。')
    downloading = true
    try {
      const origin = requireServerAddress()
      const { signal } = updateConnection
      const release = await latest(origin, signal)
      if (!release || release.id !== id || requireServerAddress() !== origin)
        throw new Error('可用版本已变更，请重新检查更新。')
      if (install) {
        const assertCurrent = () => {
          signal.throwIfAborted()
          if (event.sender.isDestroyed())
            throw new Error('升级窗口或服务地址已变更，请重新检查更新。')
        }
        try {
          console.info(
            `开始安装客户端升级，发布 ID=${release.id}，平台=${platform}，版本=${release.version}`,
          )
          await installClientUpdate(
            transport(),
            origin,
            release,
            installedEnvironment(),
            assertCurrent,
            finishUpdate,
            (message) => {
              if (!event.sender.isDestroyed()) event.sender.send('desktop:update-progress', message)
            },
            signal,
          )
        } catch (cause) {
          console.warn(`客户端升级未完成，发布 ID=${release.id}，平台=${platform}`)
          const code = (cause as NodeJS.ErrnoException)?.code
          if (code === 'ENOSPC') throw new Error('磁盘空间不足，请清理空间后重新升级。', { cause })
          if (code === 'EACCES' || code === 'EPERM' || code === 'EROFS')
            throw new Error('无法写入安装目录，请检查客户端是否位于可写目录。', { cause })
          throw cause
        }
        return true
      }
      const selected = await dialog.showSaveDialog({
        title: '保存客户端安装包',
        defaultPath: join(
          app.getPath('downloads'),
          `TomBot-${release.version}-${platform}${release.extension}`,
        ),
        buttonLabel: '保存',
      })
      if (selected.canceled || !selected.filePath) return false
      signal.throwIfAborted()
      await saveClientUpdate(transport(), origin, release, selected.filePath, undefined, signal)
      shell.showItemInFolder(selected.filePath)
      return true
    } finally {
      downloading = false
    }
  }
  ipcMain.handle('desktop:download-update', (event, id: unknown) =>
    handle(() => useUpdatePackage(event, id, false)),
  )
  ipcMain.handle('desktop:install-update', (event, id: unknown) =>
    handle(() => useUpdatePackage(event, id, true)),
  )
  ipcMain.handle('desktop:about', () =>
    handle(() => loadApplicationAbout(app.getVersion(), requireServerAddress(), transport())),
  )
  ipcMain.handle('desktop:conversation-previews', (_event, ids: string[]) =>
    handle(() => readConversationPreviews(call, ids)),
  )
  ipcMain.handle('desktop:load-workspace', () => handle(() => loadWorkspace()))
  ipcMain.handle('desktop:user-profile', () => handle(() => loadUserProfile()))
  ipcMain.handle('desktop:create-agent', (_event, profile: NewAgent, requestId: string) =>
    handle(async () => {
      const id = await createBot(call, profile, requestId)
      // 首只猫咪会修改工作空间修订号；刷新后才能保存选择并向正确的会话投递。
      const snapshot = await loadWorkspace()
      const agent = snapshot.workspace.agents.find((item) => item.id === id)
      if (!agent) throw new Error('服务目录尚未确认新猫咪，请重试。')
      return agent
    }),
  )
  ipcMain.handle('desktop:clipboard-files', () => handle(readClipboardFiles))
  ipcMain.handle('desktop:save-workspace', (_event, workspace: AppWorkspace) =>
    handle(() => saveWorkspace(workspace)),
  )
  ipcMain.on('desktop:unwatch-events', (event, watchId: string) => {
    eventWatches.get(`${event.sender.id}:${watchId}`)?.abort()
  })
  ipcMain.handle('desktop:agent-conversation', (_event, agentId: string, before?: number) =>
    handle(async () => {
      const conversationID = await conversationFor(agentId)
      const [page, activity] = await Promise.all([
        listMessagesPage(conversationID, before),
        call('GET', `/conversations/${encodeURIComponent(conversationID)}/activity`) as Promise<{
          runs: unknown[]
        }>,
      ])
      return { ...page, runs: activity.runs }
    }),
  )
  ipcMain.handle('desktop:watch-events', (event, watchId: string, needsRefresh: unknown) =>
    handle(async () => {
      const controller = new AbortController()
      const key = `${event.sender.id}:${watchId}`
      eventWatches.get(key)?.abort()
      eventWatches.set(key, controller)
      if (needsRefresh === true) sessionsNeedingEventRefresh.add(event.sender)
      const detach = () => controller.abort()
      event.sender.once('destroyed', detach)
      event.sender.once('did-start-navigation', detach)
      try {
        await pumpEvents(event, watchId, controller.signal)
        return null
      } finally {
        event.sender.removeListener('destroyed', detach)
        event.sender.removeListener('did-start-navigation', detach)
        if (eventWatches.get(key) === controller) eventWatches.delete(key)
      }
    }),
  )
  ipcMain.handle(
    'desktop:deliver',
    (
      event,
      input: {
        agentId: string
        content: string
        requestId: string
        clientRequestId: string
        attachments?: Array<{ path: string; sha256: string }>
        replyToId?: string
      },
    ) =>
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
  ipcMain.handle('desktop:computer-status', () =>
    handle(async () => computerStatus.parse(await call('GET', '/computer'))),
  )
  ipcMain.handle('desktop:computer-restart', (_event, version: string) =>
    handle(async () => {
      const expected = computerStatus.shape.version.parse(version)
      return computerStatus.parse(
        await call('POST', '/computer/restart', { expected_version: expected }),
      )
    }),
  )
  ipcMain.handle('desktop:tasks', (_event, agentId: string) =>
    handle(() => listTasks(call, agentId)),
  )
  ipcMain.handle(
    'desktop:task-enabled',
    (_event, input: { agentId: string; id: string; revision: number; enabled: boolean }) =>
      handle(async () => {
        await call(
          'POST',
          `/bots/${encodeURIComponent(input.agentId)}/tasks/${encodeURIComponent(input.id)}/state`,
          { expected_revision: input.revision, is_enabled: input.enabled },
        )
      }),
  )
  ipcMain.handle(
    'desktop:task-delete',
    (_event, input: { agentId: string; id: string; revision: number }) =>
      handle(async () => {
        await call(
          'DELETE',
          `/bots/${encodeURIComponent(input.agentId)}/tasks/${encodeURIComponent(input.id)}`,
          { expected_revision: input.revision },
        )
      }),
  )
  ipcMain.handle('desktop:groups', () =>
    handle(async () => {
      const groups = await readGroups(call)
      return groups.map((group) => ({
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
  ipcMain.handle(
    'desktop:group-create',
    (_event, input: { name: string; memberIds: string[]; requestId: string }) =>
      handle(async () => {
        const group = await createGroup(call, input.name, input.memberIds, input.requestId)
        return groupDetail(group.id)
      }),
  )
  ipcMain.handle(
    'desktop:group-rename',
    (_event, input: { id: string; revision: number; name: string }) =>
      handle(async () => {
        await changeGroup(call, input.id, input.revision, { name: input.name })
        return groupDetail(input.id)
      }),
  )
  ipcMain.handle(
    'desktop:chat-files',
    (_event, input: { id: string; before?: string; query?: string }) =>
      handle(() => listChatFiles(call, input.id, input.before, input.query)),
  )
  ipcMain.handle(
    'desktop:chat-file-upload',
    (_event, input: { id: string; file: { name: string; bytes: Uint8Array }; requestId: string }) =>
      handle(() => uploadChatFile(call, input.id, input.file, input.requestId)),
  )
  ipcMain.handle('desktop:chat-file-preview', (_event, input: { id: string; fileId: string }) =>
    handle(() => previewChatFile(call, input.id, input.fileId)),
  )
  ipcMain.handle(
    'desktop:chat-file-rename',
    (_event, input: { id: string; fileId: string; revision: number; name: string }) =>
      handle(() => renameChatFile(call, input.id, input.fileId, input.revision, input.name)),
  )
  ipcMain.handle(
    'desktop:chat-file-delete',
    (_event, input: { id: string; fileId: string; revision: number }) =>
      handle(() => deleteChatFile(call, input.id, input.fileId, input.revision)),
  )
  ipcMain.handle('desktop:chat-file-download', (_event, input: { id: string; fileId: string }) =>
    handle(async () => {
      const { file, bytes } = await readChatFile(call, input.id, input.fileId)
      const selected = await dialog.showSaveDialog({
        title: '保存聊天文件',
        defaultPath: join(app.getPath('downloads'), file.name),
        buttonLabel: '保存',
      })
      if (selected.canceled || !selected.filePath) return false
      await writeFile(selected.filePath, bytes)
      return true
    }),
  )
  ipcMain.handle('desktop:group', (_event, id: string) => handle(() => groupDetail(id)))
  ipcMain.handle(
    'desktop:group-members',
    (_event, input: { id: string; revision: number; botIds: string[] }) =>
      handle(async () => {
        await changeGroup(call, input.id, input.revision, { add: input.botIds })
      }),
  )
  ipcMain.handle(
    'desktop:group-member-remove',
    (_event, input: { id: string; revision: number; botId: string }) =>
      handle(async () => {
        await changeGroup(call, input.id, input.revision, { remove: input.botId })
      }),
  )
  ipcMain.handle('desktop:group-delete', (_event, input: { id: string; revision: number }) =>
    handle(() =>
      call('POST', `/groups/${encodeURIComponent(input.id)}/state`, {
        expected_revision: input.revision,
        is_archived: true,
      }),
    ),
  )
  ipcMain.handle('desktop:bot-activities', () => handle(() => call('GET', '/activity')))
  ipcMain.handle('desktop:conversation-activity', (_event, id: string) =>
    handle(() => call('GET', `/conversations/${encodeURIComponent(id)}/activity`)),
  )

  ipcMain.handle(
    'desktop:conversation-messages',
    (_event, input: { conversationId: string; before?: number }) =>
      handle(() => listMessagesPage(input.conversationId, input.before ?? 0)),
  )
  ipcMain.handle(
    'desktop:conversation-send',
    (
      _event,
      input: {
        requestId: string
        conversationId: string
        content: string
        attachments: Array<{ path: string; sha256: string }>
        replyToId?: string
        mentions: string[]
      },
    ) => handle(() => sendGroupMessage(call, input)),
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
        groupId?: string
        requestId?: string
      },
    ) => handle(() => uploadFile(input)),
  )
  ipcMain.handle('desktop:preview', (_event, input: { path: string; botId?: string }) =>
    handle(() => previewMessageFile(call, input.path)),
  )
  ipcMain.handle(
    'desktop:reaction',
    (
      _event,
      input: { messageId: string; emoji: string; isActive: boolean; clientRequestId: string },
    ) =>
      handle(async () => {
        const response = (await call(
          'PUT',
          `/messages/${encodeURIComponent(input.messageId)}/reaction`,
          {
            emoji: input.emoji,
            is_active: input.isActive,
            client_request_id: input.clientRequestId,
          },
        )) as { message?: ServerMessage }
        const message = response.message && toBubble(response.message)
        if (!message)
          throw Object.assign(new Error('服务没有返回表情更新后的消息。'), {
            code: 'RESPONSE_INVALID',
          })
        return message
      }),
  )
  ipcMain.handle('desktop:ensure-agent', (_event, agentId: string) =>
    handle(() => call('POST', `/workspace/agents/${encodeURIComponent(agentId)}/ready`)),
  )
  ipcMain.handle('desktop:console-open', (_event, botId: string, requestId: string) =>
    handle(() => openConsole(call, botId, requestId)),
  )
  ipcMain.handle('desktop:console-status', (_event, id: string) =>
    handle(() => controlConsole(call, id, 'heartbeat')),
  )
  ipcMain.handle('desktop:console-close', (_event, id: string) =>
    handle(() => controlConsole(call, id, 'close')),
  )
  ipcMain.handle('desktop:console-control', (_event, id: string, takeover: boolean) =>
    handle(() => {
      if (typeof takeover !== 'boolean') throw new Error('接管动作无效。')
      return controlConsole(call, id, takeover ? 'takeover' : 'release')
    }),
  )
  consoleStreams.register(ipcMain)
}

/** 只把公开资料交给界面；不接受远程头像地址，避免图片绕过主进程连接和认证边界。 */
async function loadUserProfile(): Promise<UserProfile> {
  const response = (await call('GET', '/auth/profile')) as {
    profile?: { username?: unknown; display_name?: unknown; avatar?: unknown }
  }
  const profile = response.profile
  const maxAvatarLength = Math.ceil((2 << 20) / 3) * 4 + 'data:image/png;base64,'.length
  if (
    !profile ||
    typeof profile.username !== 'string' ||
    !profile.username.trim() ||
    typeof profile.display_name !== 'string' ||
    typeof profile.avatar !== 'string' ||
    profile.avatar.length > maxAvatarLength ||
    (profile.avatar !== '' && !/^data:image\/png;base64,[A-Za-z0-9+/]+={0,2}$/.test(profile.avatar))
  ) {
    throw Object.assign(new Error('服务没有返回有效的用户资料。'), { code: 'RESPONSE_INVALID' })
  }
  return { username: profile.username, displayName: profile.display_name, avatar: profile.avatar }
}

/** 受理后立即返回；运行和回复通过用户事件刷新，不占用客户端发送通道。 */
async function deliverMessage(
  _event: IpcMainInvokeEvent,
  input: {
    agentId: string
    content: string
    requestId: string
    clientRequestId: string
    attachments?: Array<{ path: string; sha256: string }>
    replyToId?: string
  },
): Promise<unknown> {
  const conversationID = await conversationFor(input.agentId)
  const accepted = (await call(
    'POST',
    `/conversations/${encodeURIComponent(conversationID)}/messages`,
    {
      client_request_id: input.clientRequestId,
      content: input.content,
      ...(input.attachments?.length
        ? {
            file_ids: input.attachments.map(
              (attachment) => parseChatFilePath(attachment.path).fileId,
            ),
          }
        : {}),
      ...(input.replyToId ? { reply_to_id: input.replyToId } : {}),
    },
  )) as { submission?: { run_id?: string; message_id?: string } }
  if (!accepted.submission?.run_id || !accepted.submission.message_id) {
    throw Object.assign(new Error('服务没有返回消息和运行标识。'), { code: 'RESPONSE_INVALID' })
  }
  return { acceptedMessageId: accepted.submission.message_id }
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
  client_request_id?: string
  sequence: number
  sender_kind: string
  sender_bot_id?: string
  created_at: string
  blocks?: Array<{
    kind: string
    text?: string
    task_result?: boolean
    user_message?: boolean
    bot_message?: { sender_id: string }
    dm_notice?: {
      direction: 'sent' | 'received'
      peer_id: string
      peer_name: string
      conversation_id: string
    }
    mentions?: Array<{ id: string; name: string }>
    chat_file?: ChatFile
    artifact?: { path?: string; sha256?: string; media_type?: string }
  }>
  reply_to?: { message_id: string; excerpt: string }
  reactions?: Array<{ emoji: string; count: number }>
  reaction_revision?: number
}

function toBubble(message: ServerMessage): {
  isNotice: boolean
  dmNotice?: {
    direction: 'sent' | 'received'
    peerId: string
    peerName: string
    conversationId: string
  }
  id: string
  requestId?: string
  sequence: number
  role: 'user' | 'assistant'
  speaker: string
  content: string
  createdAt: string
  replyTo?: { messageId: string; excerpt: string }
  reactions: Array<{ emoji: string; count: number }>
  reactionRevision: number
  mentions: Array<{ id: string; name: string }>
  attachments: Array<{
    name: string
    path: string
    sha256: string
    mediaType: string
    kind: 'image' | 'file'
  }>
} | null {
  const notice = message.blocks?.find((block) => block.dm_notice)?.dm_notice
  const sender = message.blocks?.find((block) => block.bot_message)?.bot_message?.sender_id
  const content = (message.blocks ?? [])
    .filter((block) => block.kind === 'text' || block.kind === 'dm_notice')
    .map((block) => block.text ?? '')
    .join('')
  const attachments = (message.blocks ?? []).flatMap((block) => {
    if (block.kind === 'chat_file' && block.chat_file) {
      const file = block.chat_file
      return [
        {
          name: file.name,
          path: chatFilePath(file),
          sha256: file.sha256,
          mediaType: file.media_type,
          kind: attachmentKind(file.name, file.media_type),
        },
      ]
    }
    if (block.kind !== 'artifact' || !block.artifact?.path || !block.artifact.sha256) return []
    const name = block.artifact.path.split('/').pop() || block.artifact.path
    const mediaType = block.artifact.media_type ?? ''
    return [
      {
        name,
        path: block.artifact.path,
        sha256: block.artifact.sha256,
        mediaType,
        kind: attachmentKind(name, mediaType),
      },
    ]
  })
  if (!content.trim() && attachments.length === 0) return null
  return {
    isNotice:
      message.sender_kind === 'system' &&
      !(message.blocks ?? []).some(
        (block) => block.task_result || block.user_message || block.bot_message,
      ),
    id: message.id,
    ...(message.client_request_id ? { requestId: message.client_request_id } : {}),
    sequence: message.sequence,
    role: message.sender_kind === 'user' ? 'user' : 'assistant',
    speaker: message.sender_bot_id ?? sender ?? '',
    ...(notice
      ? {
          dmNotice: {
            direction: notice.direction,
            peerId: notice.peer_id,
            peerName: notice.peer_name,
            conversationId: notice.conversation_id,
          },
        }
      : {}),
    content: content.trim() || attachments.map((item) => item.name).join('、'),
    createdAt: message.created_at,
    ...(message.reply_to
      ? { replyTo: { messageId: message.reply_to.message_id, excerpt: message.reply_to.excerpt } }
      : {}),
    mentions: (message.blocks ?? []).flatMap((block) => block.mentions ?? []),
    reactions: message.reactions ?? [],
    reactionRevision: message.reaction_revision ?? 0,
    attachments,
  }
}

async function uploadFile(input: {
  name: string
  type: string
  bytes: Uint8Array
  botId?: string
  shared?: boolean
  groupId?: string
  requestId?: string
}): Promise<{ path: string; sha256: string }> {
  if (Boolean(input.groupId) === Boolean(input.botId) || input.shared)
    throw new Error('请选择一个有效的聊天会话。')
  const conversationId = input.groupId
    ? (
        (await call('GET', `/groups/${encodeURIComponent(input.groupId)}`)) as {
          group: ServerGroup
        }
      ).group.conversation_id
    : await conversationFor(input.botId!)
  const file = await uploadChatFile(call, conversationId, input, input.requestId ?? '')
  return { path: chatFilePath(file), sha256: file.sha256 }
}

interface LoginInput {
  username: string
  password: string
  code?: string
}

/** 使用系统浏览器完成登录并校验回调，不在桌面收集口令。 */
async function beginBrowserLogin(): Promise<{ phase: 'ready'; username?: string }> {
  await loadSession()
  const state = Buffer.from(await randomBytes(32)).toString('base64url')
  const verifier = Buffer.from(await randomBytes(32)).toString('base64url')
  const challenge = createHash('sha256').update(verifier).digest('base64url')
  await new Promise<void>((resolve, reject) => {
    const close = () => {
      clearTimeout(timer)
      server.close()
    }
    let isCompleting = false
    const server = createServer((request, response) => {
      const url = new URL(request.url ?? '/', 'http://127.0.0.1')
      if (url.pathname !== '/callback') {
        response.writeHead(404).end()
        return
      }
      if (request.method !== 'GET') {
        response.writeHead(405, { Allow: 'GET' }).end()
        return
      }
      if (isCompleting) {
        response.writeHead(409).end()
        return
      }
      isCompleting = true
      clearTimeout(timer)
      // 成功页必须等待授权交换与本地保存完成，避免浏览器显示成功而客户端仍失败。
      void (async () => {
        try {
          const code = url.searchParams.get('code')
          const returned = url.searchParams.get('state')
          if (
            !code ||
            returned !== state ||
            url.searchParams.getAll('code').length !== 1 ||
            url.searchParams.getAll('state').length !== 1
          ) {
            throw Object.assign(new Error('登录回调无效，请重新发起登录。'), {
              code: 'AUTHORIZATION_INVALID',
            })
          }
          const result = (await authenticate('/auth/browser/exchange', {
            code,
            code_verifier: verifier,
            state: returned,
          })) as AuthSession
          await storeSession(result)
          const page = loginResultPage(true)
          response.writeHead(200, page.headers).end(page.html)
          resolve()
        } catch (error) {
          const page = loginResultPage(false)
          response.writeHead(400, page.headers).end(page.html)
          reject(error)
        } finally {
          close()
        }
      })()
    })
    server.on('error', (error) => {
      close()
      reject(error)
    })
    // 用户可能在浏览器里直接关闭页面；超时后释放监听，不能永久等待。
    const timer = setTimeout(() => {
      close()
      reject(Object.assign(new Error('登录超时，请重新发起。'), { code: 'AUTH_TIMEOUT' }))
    }, 10 * 60_000)
    timer.unref()
    // 只监听本机回环，端口由系统分配，避免与他人冲突。
    server.listen(0, '127.0.0.1', async () => {
      try {
        const address = server.address()
        if (!address || typeof address === 'string') throw new Error('无法分配本地回调端口。')
        const redirectURI = `http://127.0.0.1:${address.port}/callback`
        const started = (await authenticate('/auth/browser/start', {
          redirect_uri: redirectURI,
          state,
          code_challenge: challenge,
          device_name: 'TomBot 桌面',
          platform: devicePlatform(),
        })) as { authorization_url?: string }
        if (!started.authorization_url) throw new Error('服务没有返回授权页地址。')
        await shell.openExternal(`${requireServerAddress()}${started.authorization_url}`)
      } catch (error) {
        close()
        reject(error)
      }
    })
  })
  return { phase: 'ready', username }
}

/** 账号登录：用户名和密码由用户输入，动态验证码只在管理员账户要求时提交。 */
async function login(input: LoginInput): Promise<void> {
  await loadSession()
  const result = (await authenticate('/auth/login', {
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
