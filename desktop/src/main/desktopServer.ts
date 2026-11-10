import { app, ipcMain, type IpcMainInvokeEvent } from 'electron'
import { chmod, mkdir, readFile, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import {
  normalizeServerAddress,
  parseStoredSession,
  selectServerAddress,
  serializeStoredSession,
} from '../shared/serverAddress'

const codeMessages: Record<string, string> = {
  SETUP_CODE_INVALID: '初始化码不正确。',
  PASSWORD_INVALID: '密码至少 12 个字符，且不能超过 1024 字节。',
  ALREADY_INITIALIZED: '服务已经初始化，请改为验证密码。',
  NOT_INITIALIZED: '请先完成初始化。',
  AUTH_INVALID: '密码不正确。',
  AUTH_REQUIRED: '需要先验证密码。',
  AUTH_RATE_LIMITED: '尝试次数过多，请稍后再试。',
  MODEL_NOT_READY: '尚未配置可用模型，请先在设置中填写 API 地址和模型名称。',
  WORKSPACE_CONFLICT: '工作空间刚被更新，请重新发送。',
  WORKSPACE_NOT_PRISTINE: '服务端已有工作空间，本机旧档没有导入。',
  OFFLINE: '无法连接服务。请确认地址和网络后重试。',
  SERVER_URL_MISSING: '还没有填写服务地址。',
}

interface Failure {
  code: string
  message: string
}

type CallResult<T> = { ok: true; value: T } | ({ ok: false } & Failure)

let token = ''
let savedServerAddress = ''
let sessionLoaded = false

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

async function loadSession(): Promise<void> {
  if (sessionLoaded) return
  sessionLoaded = true
  try {
    const stored = parseStoredSession(await readFile(sessionPath(), 'utf8'))
    token = stored.deviceToken
    savedServerAddress = stored.serverUrl
  } catch {
    token = ''
    savedServerAddress = ''
  }
}

/** 凭据和地址写在同一个文件里，避免只更新令牌时把地址抹掉。 */
async function persistSession(): Promise<void> {
  const path = sessionPath()
  await mkdir(app.getPath('userData'), { recursive: true })
  await writeFile(
    path,
    serializeStoredSession({ deviceToken: token, serverUrl: savedServerAddress }),
    { mode: 0o600 },
  )
  await chmod(path, 0o600)
}

async function storeToken(next: string): Promise<void> {
  token = next
  sessionLoaded = true
  await persistSession()
}

async function inspectSession(): Promise<{
  phase: 'address' | 'offline' | 'setup' | 'login' | 'ready'
  message?: string
  serverUrl: string
}> {
  await loadSession()
  const choice = selectServerAddress(savedServerAddress, process.env.TOMBOT_SERVER_URL)
  if (!choice.connect) return { phase: 'address', serverUrl: choice.prefill }
  try {
    const status = (await call('GET', '/auth/status')) as {
      initialized: boolean
      authenticated: boolean
    }
    if (status.authenticated) return { phase: 'ready', serverUrl: choice.connect }
    token = ''
    await persistSession()
    return { phase: status.initialized ? 'login' : 'setup', serverUrl: choice.connect }
  } catch (error) {
    const failed = failure(error)
    return { phase: 'offline', message: failed.message, serverUrl: choice.connect }
  }
}

function failure(error: unknown): Failure {
  const coded = error as { code?: string; message?: string }
  const code = coded.code || 'REQUEST_FAILED'
  return { code, message: codeMessages[code] ?? coded.message ?? '服务暂时无法完成这个操作。' }
}

async function call(
  method: string,
  path: string,
  body?: unknown,
  timeout = 20_000,
): Promise<unknown> {
  await loadSession()
  const base = requireServerAddress()
  let response: Response
  try {
    response = await fetch(`${base}/api/v1${path}`, {
      method,
      headers: {
        'content-type': 'application/json',
        ...(token ? { authorization: `Bearer ${token}` } : {}),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(timeout),
    })
  } catch {
    throw Object.assign(new Error(codeMessages.OFFLINE), { code: 'OFFLINE' })
  }
  const text = await response.text()
  let payload: { error?: { code?: string; message?: string } } = {}
  if (text) {
    try {
      payload = JSON.parse(text) as typeof payload
    } catch {
      throw Object.assign(new Error('服务返回了无法识别的内容。'), { code: 'RESPONSE_INVALID' })
    }
  }
  if (!response.ok) {
    const code = payload.error?.code ?? 'REQUEST_FAILED'
    const serverMessage = payload.error?.message
    const message =
      codeMessages[code] ??
      (serverMessage && /[\u4e00-\u9fff]/.test(serverMessage)
        ? serverMessage
        : '服务暂时无法完成这个操作。')
    throw Object.assign(new Error(message), { code })
  }
  return payload
}

async function handle<T>(task: () => Promise<T>): Promise<CallResult<T>> {
  try {
    return { ok: true, value: await task() }
  } catch (error) {
    return { ok: false, ...failure(error) }
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
        token = ''
        savedServerAddress = next
        await persistSession()
      }
      return inspectSession()
    }),
  )
  ipcMain.handle('desktop:setup', (_event, input: { setupCode: string; password: string }) =>
    handle(() =>
      call('POST', '/auth/setup', { setupCode: input.setupCode, password: input.password }),
    ),
  )
  ipcMain.handle('desktop:login', (_event, password: string) => handle(() => login(password)))
  ipcMain.handle('desktop:load-workspace', () => handle(() => call('GET', '/workspace')))
  ipcMain.handle('desktop:save-workspace', (_event, workspace: unknown) =>
    handle(() => call('PUT', '/workspace', workspace)),
  )
  ipcMain.handle('desktop:import-workspace', (_event, workspace: unknown) =>
    handle(async () => {
      const snapshot = (await call('POST', '/workspace/import', workspace)) as {
        workspace: unknown
      }
      return snapshot.workspace
    }),
  )
  ipcMain.handle('desktop:deliver', (event, input: { agentId: string; messageId: string }) =>
    handle(async () => {
      try {
        return await readStream(event, input)
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
  ipcMain.handle('desktop:bootstrap', () => handle(() => call('POST', '/bootstrap/retry')))
  ipcMain.handle('desktop:memories', (_event, agentId: string) =>
    handle(() => call('GET', `/bots/${encodeURIComponent(agentId)}/memory`)),
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
      const rows = (await call('GET', '/groups?limit=50')) as Array<{
        id: string
        name: string
        revision: number
        conversation_id: string
      }>
      return Array.isArray(rows) ? rows : []
    }),
  )
  ipcMain.handle('desktop:group-create', (_event, input: { name: string; memberIds: string[] }) =>
    handle(async () => {
      for (const id of input.memberIds)
        await call('POST', `/workspace/agents/${encodeURIComponent(id)}/ready`)
      return call('POST', '/groups', {
        name: input.name,
        purpose: '',
        routing: 'all',
        members: input.memberIds,
      })
    }),
  )
  ipcMain.handle('desktop:group', (_event, id: string) =>
    handle(() => call('GET', `/groups/${encodeURIComponent(id)}`)),
  )
  ipcMain.handle(
    'desktop:group-members',
    (_event, input: { id: string; revision: number; botIds: string[] }) =>
      handle(async () => {
        for (const id of input.botIds)
          await call('POST', `/workspace/agents/${encodeURIComponent(id)}/ready`)
        return call('POST', `/groups/${encodeURIComponent(input.id)}/members`, {
          revision: input.revision,
          bot_ids: input.botIds,
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
  ipcMain.handle('desktop:conversation-messages', (_event, conversationId: string) =>
    handle(async () => {
      const rows = (await call(
        'GET',
        `/conversations/${encodeURIComponent(conversationId)}/messages?limit=80`,
      )) as ServerMessage[]
      return (Array.isArray(rows) ? rows : [])
        .flatMap((row) => {
          const bubble = toBubble(row)
          return bubble ? [bubble] : []
        })
        .reverse()
    }),
  )
  ipcMain.handle(
    'desktop:conversation-send',
    (
      event,
      input: {
        conversationId: string
        content: string
        attachments: Array<{ path: string; sha256: string }>
        replyToId?: string
      },
    ) =>
      handle(() =>
        readConversationStream(event, input.conversationId, {
          content: input.content,
          attachments: input.attachments,
          ...(input.replyToId ? { reply_to_id: input.replyToId } : {}),
        }),
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
          baseUrl: draft.baseUrl,
          modelName: draft.modelName,
          temperature: draft.temperature,
          topP: draft.topP,
          maxOutputTokens: draft.maxOutputTokens,
          isStreaming: draft.isStreaming,
          ...(input.apiKey ? { apiKey: input.apiKey } : {}),
        })
      }),
  )
}

async function readStream(
  event: IpcMainInvokeEvent,
  input: { agentId: string; messageId: string },
): Promise<unknown> {
  await loadSession()
  const response = await fetch(`${requireServerAddress()}/api/v1/messages/stream`, {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      accept: 'text/event-stream',
      ...(token ? { authorization: `Bearer ${token}` } : {}),
    },
    body: JSON.stringify(input),
    signal: AbortSignal.timeout(90_000),
  })
  const type = response.headers.get('content-type') ?? ''
  if (!type.includes('text/event-stream')) {
    const text = await response.text()
    const payload = text
      ? (JSON.parse(text) as { error?: { code?: string; message?: string } })
      : {}
    if (!response.ok) {
      const code = payload.error?.code ?? 'REQUEST_FAILED'
      throw Object.assign(new Error(codeMessages[code] ?? '服务暂时无法完成这个操作。'), { code })
    }
    return payload
  }
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
      to_bot_id?: string
      to_name?: string
      conversation_id?: string
    }
    if (name === 'delta' || name === 'process') {
      event.sender.send('desktop:stream-event', { type: name, text: parsed.text ?? '' })
    }
    if (name === 'dm' && parsed.kind === 'bot_message' && parsed.message_id && parsed.to_bot_id) {
      event.sender.send('desktop:stream-event', {
        type: 'dm',
        text: '',
        messageId: parsed.message_id,
        peerId: parsed.to_bot_id,
        peerName: parsed.to_name ?? parsed.to_bot_id,
        conversationId: parsed.conversation_id ?? '',
        direction: 'sent',
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

type ServerMessage = {
  id: string
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
    headers: { ...(token ? { authorization: `Bearer ${token}` } : {}) },
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
    headers: { ...(token ? { authorization: `Bearer ${token}` } : {}) },
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
): Promise<unknown> {
  await loadSession()
  const response = await fetch(
    `${requireServerAddress()}/api/v1/conversations/${encodeURIComponent(conversationId)}/messages`,
    {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        accept: 'text/event-stream',
        ...(token ? { authorization: `Bearer ${token}` } : {}),
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
  return readEventStream(event, response)
}

async function readEventStream(event: IpcMainInvokeEvent, response: Response): Promise<unknown> {
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
      to_bot_id?: string
      to_name?: string
      conversation_id?: string
    }
    if (name === 'delta' || name === 'process') {
      event.sender.send('desktop:stream-event', { type: name, text: parsed.text ?? '' })
    }
    if (name === 'dm' && parsed.kind === 'bot_message' && parsed.message_id && parsed.to_bot_id) {
      event.sender.send('desktop:stream-event', {
        type: 'dm',
        text: '',
        messageId: parsed.message_id,
        peerId: parsed.to_bot_id,
        peerName: parsed.to_name ?? parsed.to_bot_id,
        conversationId: parsed.conversation_id ?? '',
        direction: 'sent',
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

async function login(password: string): Promise<void> {
  const session = (await call('POST', '/auth/login', {
    password,
    deviceName: 'TomBot 桌面',
    platform: devicePlatform(),
  })) as { device_token?: string }
  if (!session.device_token) {
    throw Object.assign(new Error('登录没有返回设备凭据。'), { code: 'AUTH_INVALID' })
  }
  await storeToken(session.device_token)
}
