import { app, ipcMain } from 'electron'
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
    response = await fetch(`${serverUrl()}/api/desktop/v1${path}`, {
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
    throw Object.assign(new Error(codeMessages[code] ?? '服务暂时无法完成这个操作。'), { code })
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
      const status = (await call('GET', '/session')) as {
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
    handle(() => call('POST', '/setup', { setupCode: input.setupCode, password: input.password })),
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
  ipcMain.handle('desktop:deliver', (_event, input: { agentId: string; messageId: string }) =>
    handle(async () => {
      try {
        return await call('POST', '/messages/deliver', input, 70_000)
      } catch (error) {
        const coded = error as { code?: string; message?: string }
        if (coded.code === 'MODEL_NOT_READY') {
          return { code: coded.code, message: coded.message }
        }
        throw error
      }
    }),
  )
  ipcMain.handle('desktop:load-settings', () => handle(() => call('GET', '/settings')))
  ipcMain.handle(
    'desktop:save-settings',
    (_event, input: { draft: Record<string, unknown>; apiKey: string }) =>
      handle(() =>
        call('PUT', '/settings', {
          ...input.draft,
          ...(input.apiKey ? { apiKey: input.apiKey } : {}),
        }),
      ),
  )
}

async function login(password: string): Promise<void> {
  const session = (await call('POST', '/login', {
    password,
    deviceName: 'TomBot 桌面',
    platform: devicePlatform(),
  })) as { device_token?: string }
  if (!session.device_token) {
    throw Object.assign(new Error('登录没有返回设备凭据。'), { code: 'AUTH_INVALID' })
  }
  await storeToken(session.device_token)
}
