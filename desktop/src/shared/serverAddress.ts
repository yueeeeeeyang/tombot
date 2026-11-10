const SERVER_ADDRESS_MAX_LENGTH = 2048

/** 用户填写的服务地址不合法。消息可以直接展示。 */
export class ServerAddressError extends Error {
  readonly code = 'SERVER_URL_INVALID'

  constructor(message: string) {
    super(message)
    this.name = 'ServerAddressError'
  }
}

export interface StoredDesktopSession {
  deviceToken: string
  serverUrl: string
}

/**
 * 把用户输入规范成源站。只接受 http/https，拒绝路径、参数和写在地址里的凭据。
 * 用户明确填写的环回地址可以保留；调用方不能在输入为空时另选一个默认主机。
 */
export function normalizeServerAddress(input: string): string {
  const trimmed = input.trim()
  if (!trimmed) throw new ServerAddressError('请输入服务地址。')
  if (trimmed.length > SERVER_ADDRESS_MAX_LENGTH) {
    throw new ServerAddressError('服务地址过长。')
  }
  let url: URL
  try {
    url = new URL(trimmed)
  } catch {
    throw new ServerAddressError('请输入以 http:// 或 https:// 开头的服务地址。')
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') {
    throw new ServerAddressError('服务地址只支持 http 或 https。')
  }
  if (url.username || url.password) {
    throw new ServerAddressError('请不要把账号或密码写进服务地址。')
  }
  if (!url.hostname) {
    throw new ServerAddressError('请输入以 http:// 或 https:// 开头的服务地址。')
  }
  if ((url.pathname !== '/' && url.pathname !== '') || url.search || url.hash) {
    throw new ServerAddressError('请只填写服务主机，不要带路径或参数。')
  }
  return url.origin
}

/** 已保存的地址必须能通过同一套校验。损坏或空白都不能当成可连接的主机。 */
export function storedServerAddress(value: unknown): string {
  if (typeof value !== 'string') return ''
  try {
    return normalizeServerAddress(value)
  } catch {
    return ''
  }
}

/**
 * 开发环境变量只预填表单。非法值原样留下让用户改；没设置时返回空字符串，
 * 不能在这里补本机地址。
 */
export function prefillServerAddress(envValue: string | undefined): string {
  const raw = envValue?.trim() ?? ''
  if (!raw) return ''
  try {
    return normalizeServerAddress(raw)
  } catch {
    return raw
  }
}

/**
 * 决定这次能不能连，以及表单上显示什么。
 * 只有已保存的地址可以发起连接；环境变量永远不会变成自动连接的目标。
 */
export function selectServerAddress(
  saved: unknown,
  envValue: string | undefined,
): { connect: string; prefill: string } {
  const connect = storedServerAddress(saved)
  if (connect) return { connect, prefill: connect }
  return { connect: '', prefill: prefillServerAddress(envValue) }
}

/** 读取本机会话文件。缺字段或损坏时视为还没有地址，不猜测主机。 */
export function parseStoredSession(raw: string): StoredDesktopSession {
  try {
    const parsed = JSON.parse(raw) as { deviceToken?: unknown; serverUrl?: unknown } | null
    if (!parsed || typeof parsed !== 'object') return { deviceToken: '', serverUrl: '' }
    return {
      deviceToken: typeof parsed.deviceToken === 'string' ? parsed.deviceToken : '',
      serverUrl: storedServerAddress(parsed.serverUrl),
    }
  } catch {
    return { deviceToken: '', serverUrl: '' }
  }
}

/** 凭据和地址一起写出，避免只更新其中一个时把另一个抹掉。 */
export function serializeStoredSession(session: StoredDesktopSession): string {
  return JSON.stringify({ deviceToken: session.deviceToken, serverUrl: session.serverUrl })
}
