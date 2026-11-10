import { app, ipcMain, type IpcMainInvokeEvent } from 'electron'
import { chmod, mkdir, readFile, writeFile } from 'node:fs/promises'
import { join } from 'node:path'

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
  OFFLINE: '无法连接本机服务。请确认服务已启动后重试。',
}

interface Failure {
  code: string
  message: string
}

type CallResult<T> = { ok: true; value: T } | ({ ok: false } & Failure)

let token = ''
let tokenLoaded = false

function serverUrl(): string {
  return process.env.TOMBOT_SERVER_URL?.trim() || 'http://127.0.0.1:8787'
}

function devicePlatform(): string {
  if (process.platform === 'darwin') return 'macos'
  if (process.platform === 'win32') return 'windows'
  return 'linux'
}

function sessionPath(): string {
  return join(app.getPath('userData'), 'desktop-session.json')
}

async function loadToken(): Promise<void> {
  if (tokenLoaded) return
  tokenLoaded = true
  try {
    const parsed = JSON.parse(await readFile(sessionPath(), 'utf8')) as { deviceToken?: unknown }
    token = typeof parsed.deviceToken === 'string' ? parsed.deviceToken : ''
  } catch {
    token = ''
  }
}

async function storeToken(next: string): Promise<void> {
  token = next
  tokenLoaded = true
  const path = sessionPath()
  await mkdir(app.getPath('userData'), { recursive: true })
  await writeFile(path, JSON.stringify({ deviceToken: next }), { mode: 0o600 })
  await chmod(path, 0o600)
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
  await loadToken()
  let response: Response
  try {
    response = await fetch(`${serverUrl()}/api/v1${path}`, {
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
  ipcMain.handle('desktop:session', () =>
    handle(async () => {
      const status = (await call('GET', '/auth/status')) as {
        initialized: boolean
        authenticated: boolean
      }
      if (status.authenticated) return { phase: 'ready' }
      token = ''
      await mkdir(app.getPath('userData'), { recursive: true })
      await writeFile(sessionPath(), JSON.stringify({ deviceToken: '' }), { mode: 0o600 })
      await chmod(sessionPath(), 0o600)
      return { phase: status.initialized ? 'login' : 'setup' }
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
  await loadToken()
  const response = await fetch(`${serverUrl()}/api/v1/messages/stream`, {
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
    const parsed = JSON.parse(data) as { text?: string; code?: string; message?: string }
    if (name === 'delta' || name === 'process') {
      event.sender.send('desktop:stream-event', { type: name, text: parsed.text ?? '' })
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
