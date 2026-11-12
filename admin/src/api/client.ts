import { z } from 'zod'
import {
  enrollmentSchema,
  operationSchema,
  sessionSchema,
  type Operation,
  type Principal,
} from './schemas'

export class ApiError extends Error {
  constructor(
    public readonly code: string,
    message: string,
    public readonly requestID = '',
    public readonly operation?: Operation,
  ) {
    super(message)
  }
}
export type AuthState =
  | { phase: 'checking' }
  | { phase: 'anonymous' }
  | { phase: 'unavailable'; message: string }
  | { phase: 'authenticated'; principal: Principal }

const failureSchema = z.object({
  code: z.string(),
  message: z.string().max(500),
  request_id: z.string().optional(),
  operation: operationSchema.optional(),
})
const PREFIX = '/api/v2/admin'
const MAX_RESPONSE_BYTES = 1024 * 1024
type RequestOptions = {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE'
  body?: unknown
  signal?: AbortSignal
  timeout?: number
}

/** 管理凭据由 HttpOnly Cookie 保存；页面只保留内存中的 CSRF，写请求绝不自动重放。 */
export class AdminClient {
  private csrf = ''
  private epoch = 0
  private state: AuthState = { phase: 'checking' }
  private listeners = new Set<() => void>()
  private pending = new Set<AbortController>()
  private refreshPromise: Promise<void> | null = null

  constructor(private readonly fetcher: typeof fetch = window.fetch.bind(window)) {}
  snapshot = () => this.state
  subscribe = (listener: () => void) => {
    this.listeners.add(listener)
    return () => {
      this.listeners.delete(listener)
    }
  }
  private update(state: AuthState) {
    this.state = state
    this.listeners.forEach((notify) => notify())
  }
  private clear() {
    this.epoch++
    this.csrf = ''
    this.pending.forEach((controller) => controller.abort())
    this.pending.clear()
    this.update({ phase: 'anonymous' })
  }

  async request<T>(path: string, schema: z.ZodType<T>, options: RequestOptions = {}): Promise<T> {
    if (!path.startsWith('/') || path.startsWith('//') || path.includes('..') || path.includes('#'))
      throw new ApiError('REQUEST_INVALID', '请求路径无效')
    return this.exchange(PREFIX + path, schema, options, true)
  }

  private async exchange<T>(
    path: string,
    schema: z.ZodType<T>,
    options: RequestOptions,
    adminSession: boolean,
    expectedStatus?: number,
  ): Promise<T> {
    const generation = this.epoch
    const controller = new AbortController()
    this.pending.add(controller)
    const timer = setTimeout(() => controller.abort(), options.timeout ?? 30_000)
    const signal = options.signal
      ? AbortSignal.any([controller.signal, options.signal])
      : controller.signal
    const method = options.method ?? 'GET'
    try {
      const response = await this.fetcher(path, {
        method,
        credentials: adminSession ? 'same-origin' : 'omit',
        cache: 'no-store',
        redirect: 'error',
        signal,
        headers: {
          Accept: 'application/json',
          ...(method !== 'GET'
            ? {
                'Content-Type': 'application/json',
                ...(adminSession && this.csrf ? { 'X-CSRF-Token': this.csrf } : {}),
              }
            : {}),
        },
        ...(options.body !== undefined ? { body: JSON.stringify(options.body) } : {}),
      })
      const reader = response.body?.getReader()
      const chunks: Uint8Array[] = []
      let size = 0
      if (reader) {
        try {
          for (;;) {
            const result = await reader.read()
            if (result.done) break
            size += result.value.byteLength
            if (size > MAX_RESPONSE_BYTES) {
              await reader.cancel()
              throw new ApiError('RESPONSE_INVALID', '服务响应超出大小限制')
            }
            chunks.push(result.value)
          }
        } finally {
          reader.releaseLock()
        }
      }
      if (generation !== this.epoch || signal.aborted)
        throw new ApiError('REQUEST_CANCELED', '请求已取消')
      if (response.ok && expectedStatus !== undefined && response.status !== expectedStatus)
        throw new ApiError('RESPONSE_INVALID', '服务未返回预期的完成状态，请先核实操作结果')
      if (adminSession && response.status === 401 && path !== PREFIX + '/auth/login') this.clear()
      const bytes = new Uint8Array(size)
      let offset = 0
      chunks.forEach((chunk) => {
        bytes.set(chunk, offset)
        offset += chunk.byteLength
      })
      let value: unknown
      if (size > 0) {
        try {
          value = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes))
        } catch {
          throw new ApiError('RESPONSE_INVALID', '服务返回了无法识别的响应')
        }
      }
      if (!response.ok) {
        const failure = failureSchema.safeParse(value)
        if (!failure.success) throw new ApiError('SERVICE_UNAVAILABLE', '服务暂不可用，请稍后重试')
        throw new ApiError(
          failure.data.code,
          failure.data.message,
          failure.data.request_id,
          failure.data.operation,
        )
      }
      const parsed = schema.safeParse(value)
      if (!parsed.success)
        throw new ApiError('RESPONSE_INVALID', '服务返回的数据不完整，请重新读取')
      return parsed.data
    } catch (error) {
      if (error instanceof ApiError) throw error
      if (signal.aborted)
        throw new ApiError('REQUEST_CANCELED', '请求已取消或超时；写入结果需重新查询')
      throw new ApiError('CONNECTION_FAILED', '连接失败；如已提交操作，请先查询当前状态')
    } finally {
      clearTimeout(timer)
      this.pending.delete(controller)
    }
  }

  /** 登记只使用本次输入的激活材料，不发送管理 Cookie，也不改变当前会话。 */
  beginEnrollment(activationToken: string, signal: AbortSignal) {
    return this.exchange(
      '/api/v2/auth/admin-enrollment',
      enrollmentSchema,
      { method: 'POST', body: { activation_token: activationToken }, signal },
      false,
      200,
    )
  }

  /** 激活不隐式登录，提交一次后由页面处理明确失败或未知结果。 */
  activate(activationToken: string, password: string, code: string, signal: AbortSignal) {
    return this.exchange(
      '/api/v2/auth/activate',
      z.undefined(),
      { method: 'POST', body: { activation_token: activationToken, password, code }, signal },
      false,
      204,
    )
  }

  async restore() {
    const generation = this.epoch
    try {
      const result = await this.request('/auth/session', sessionSchema)
      if (generation !== this.epoch) return
      this.csrf = result.csrf_token
      this.update({ phase: 'authenticated', principal: result.principal })
    } catch (error) {
      if (generation !== this.epoch) return
      this.update({ phase: 'unavailable', message: errorMessage(error) })
    }
  }
  async login(username: string, password: string, code: string) {
    this.clear()
    const result = await this.request('/auth/login', sessionSchema, {
      method: 'POST',
      body: { username, password, code, device_name: 'Web 管理平台' },
    })
    this.csrf = result.csrf_token
    this.update({ phase: 'authenticated', principal: result.principal })
  }
  async logout() {
    await this.request('/auth/logout', z.undefined(), { method: 'POST', body: {} })
    this.clear()
  }
  async verify(password: string, code: string) {
    await this.request('/auth/verify', z.undefined(), { method: 'POST', body: { password, code } })
  }
  /** 续期单独发起，失败不重放可能已执行的管理动作；多标签页通过浏览器锁串行轮换。 */
  refresh(): Promise<void> {
    if (this.refreshPromise) return this.refreshPromise
    const generation = this.epoch
    const run = async () => {
      if (generation !== this.epoch) throw new ApiError('REQUEST_CANCELED', '会话已更换')
      const current = await this.request('/auth/session', sessionSchema)
      this.csrf = current.csrf_token
      const result = await this.request('/auth/refresh', sessionSchema, {
        method: 'POST',
        body: {},
      })
      this.csrf = result.csrf_token
      this.update({ phase: 'authenticated', principal: result.principal })
    }
    this.refreshPromise = (
      navigator.locks
        ? navigator.locks.request('tombot-admin-refresh', run)
        : Promise.reject(
            new ApiError('BROWSER_UNSUPPORTED', '浏览器不支持安全会话续期，请重新登录'),
          )
    ).finally(() => {
      this.refreshPromise = null
    })
    return this.refreshPromise
  }
}

export function errorMessage(error: unknown): string {
  return error instanceof ApiError ? error.message : '操作失败，请重新查询当前状态'
}
