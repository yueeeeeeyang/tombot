import { z } from 'zod'
import {
  adminSetupStatusSchema,
  administratorSetupSchema,
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
export type OperationMessage = { text: string; tone: 'success' | 'info' }

export function isVerificationCanceled(error: unknown): boolean {
  return error instanceof ApiError && error.code === 'VERIFICATION_CANCELED'
}

export type AuthState =
  | { phase: 'checking' }
  | { phase: 'anonymous' }
  | { phase: 'unavailable'; message: string }
  | { phase: 'authenticated'; principal: Principal; renewalError?: string }

const failureSchema = z.object({
  code: z.string(),
  message: z.string().max(500),
  request_id: z.string().optional(),
  operation: operationSchema.optional(),
})
const PREFIX = '/api/v2/admin'
const MAX_RESPONSE_BYTES = 1024 * 1024
const REFRESH_LEAD_MS = 60_000
const SESSION_LOCK = 'tombot-admin-refresh'
const refreshContextSchema = z.object({ csrf_token: z.string().min(1).max(128) })
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
  private refreshTimer: ReturnType<typeof setTimeout> | null = null
  private accessExpiresAt = 0
  private messenger: ((message: OperationMessage) => void) | null = null
  private verifier: (() => Promise<void>) | null = null

  constructor(private readonly fetcher: typeof fetch = window.fetch.bind(window)) {}
  snapshot = () => this.state
  /** 页面注册再验证界面；只在服务端明确要求重新验证时调用，验证成功后重放被拒绝的那一次请求。 */
  setVerifier(verifier: (() => Promise<void>) | null) {
    this.verifier = verifier
  }
  /** 操作反馈由页面消息层展示，取消验证仍以异常终止原业务流程。 */
  setMessenger(messenger: ((message: OperationMessage) => void) | null) {
    this.messenger = messenger
  }
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
    this.refreshPromise = null
    this.csrf = ''
    this.accessExpiresAt = 0
    if (this.refreshTimer) {
      clearTimeout(this.refreshTimer)
      this.refreshTimer = null
    }
    this.pending.forEach((controller) => controller.abort())
    this.pending.clear()
    this.update({ phase: 'anonymous' })
  }

  /** 在到期前续期；短期凭据至少等待 5 秒，避免客户端时钟偏差形成忙循环。 */
  private scheduleRefresh(expiresAt: string) {
    if (this.refreshTimer) {
      clearTimeout(this.refreshTimer)
      this.refreshTimer = null
    }
    const expires = Date.parse(expiresAt)
    this.accessExpiresAt = expires
    const delay = Math.max(expires - Date.now() - REFRESH_LEAD_MS, 5_000)
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = null
      this.renewInBackground()
    }, delay)
  }

  /** 页面回到前台时按当前时间重新排期，避免后台标签页的定时器被浏览器推迟。 */
  resumeSessionClock() {
    if (this.state.phase !== 'authenticated' || !this.accessExpiresAt) return
    const remaining = this.accessExpiresAt - Date.now()
    if (remaining <= REFRESH_LEAD_MS || this.state.renewalError) {
      this.renewInBackground()
      return
    }
    this.scheduleRefresh(new Date(this.accessExpiresAt).toISOString())
  }

  async request<T>(path: string, schema: z.ZodType<T>, options: RequestOptions = {}): Promise<T> {
    if (!path.startsWith('/') || path.startsWith('//') || path.includes('..') || path.includes('#'))
      throw new ApiError('REQUEST_INVALID', '请求路径无效')
    return this.sessionRequest(path, schema, options)
  }

  /** 普通请求持共享锁，续期持独占锁，避免另一标签页在请求途中废弃访问凭据及 CSRF。 */
  private async sessionLock<T>(mode: LockMode, action: () => Promise<T>): Promise<T> {
    if (!navigator.locks)
      throw new ApiError('BROWSER_UNSUPPORTED', '浏览器不支持安全会话续期，请使用新版浏览器')
    try {
      return await navigator.locks.request(
        SESSION_LOCK,
        { mode, signal: AbortSignal.timeout(30_000) },
        action,
      )
    } catch (error) {
      if (error instanceof DOMException && error.name === 'TimeoutError')
        throw new ApiError('REQUEST_CANCELED', '等待会话同步超时，请稍后重试')
      throw error
    }
  }

  private acceptSession(result: z.infer<typeof sessionSchema>) {
    if (
      this.state.phase === 'authenticated' &&
      this.state.principal.session_id !== result.principal.session_id
    )
      this.clear()
    this.csrf = result.csrf_token
    this.scheduleRefresh(result.access_expires_at)
    this.update({ phase: 'authenticated', principal: result.principal })
  }

  private reportRenewalFailure(error: unknown) {
    if (this.state.phase === 'authenticated')
      this.update({ ...this.state, renewalError: `会话续期暂未完成：${errorMessage(error)}` })
  }

  private renewInBackground() {
    const generation = this.epoch
    void this.refresh(false).catch((error) => {
      // 退出或切换身份后，旧续期的取消结果不应覆盖新会话的提示。
      if (generation === this.epoch) this.reportRenewalFailure(error)
    })
  }

  private async sessionRequest<T>(
    path: string,
    schema: z.ZodType<T>,
    options: RequestOptions,
    recovered = false,
    verified = false,
  ): Promise<T> {
    const generation = this.epoch
    try {
      if (this.state.phase !== 'authenticated' || path === '/auth/login')
        return await this.exchange(PREFIX + path, schema, options, true)
      if (this.refreshPromise) await this.refreshPromise
      else if (this.accessExpiresAt - Date.now() <= REFRESH_LEAD_MS) await this.refresh(false)
      return await this.sessionLock('shared', async () => {
        if (generation !== this.epoch) throw new ApiError('REQUEST_CANCELED', '会话已更换')
        if (options.method && options.method !== 'GET') {
          // Cookie 在标签页间共享，内存中的 CSRF 不共享；写入前读取当前值。
          const current = await this.exchange(PREFIX + '/auth/session', sessionSchema, {}, true)
          this.acceptSession(current)
          if (generation !== this.epoch)
            throw new ApiError('REQUEST_CANCELED', '会话已更换，请重新操作')
        }
        return this.exchange(PREFIX + path, schema, options, true)
      })
    } catch (error) {
      if (generation !== this.epoch) throw error
      if (error instanceof ApiError && error.code === 'SESSION_INVALID') {
        if (!recovered && this.state.phase === 'authenticated') {
          await this.refresh(false)
          if (generation !== this.epoch)
            throw new ApiError('REQUEST_CANCELED', '会话已更换，请重新操作')
          if (!options.method || options.method === 'GET')
            return this.sessionRequest(path, schema, options, true, verified)
          throw new ApiError('SESSION_RENEWED', '会话已恢复，请核实当前状态后重新提交操作')
        }
        this.clear()
      }
      // 已明确在业务执行前被拒绝的再验证请求才允许重放，弹窗期间不持浏览器锁。
      if (
        error instanceof ApiError &&
        error.code === 'VERIFICATION_REQUIRED' &&
        !verified &&
        this.verifier
      ) {
        try {
          await this.verifier()
        } catch (cause) {
          if (isVerificationCanceled(cause))
            this.messenger?.({ tone: 'info', text: '已取消授权，操作已终止' })
          throw cause
        }
        if (generation !== this.epoch || options.signal?.aborted)
          throw new ApiError('REQUEST_CANCELED', '请求已取消，请重新操作')
        this.messenger?.({ tone: 'success', text: '授权成功，正在继续操作' })
        const result = await this.sessionRequest(path, schema, options, recovered, true)
        this.messenger?.({ tone: 'success', text: '操作成功' })
        return result
      }
      throw error
    }
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

  /** 首位管理员状态是公开只读接口，不需要会话；只返回是否还需要创建。 */
  adminSetupStatus(signal?: AbortSignal) {
    return this.exchange('/api/v2/auth/admin-setup', adminSetupStatusSchema, { signal }, false, 200)
  }

  /** 首位管理员由创建者当场设定口令；动态验证器密钥只在本次响应返回一次。 */
  createFirstAdministrator(
    username: string,
    displayName: string,
    password: string,
    signal: AbortSignal,
  ) {
    return this.exchange(
      '/api/v2/auth/admin-setup',
      administratorSetupSchema,
      { method: 'POST', body: { username, display_name: displayName, password }, signal },
      false,
      201,
    )
  }

  /** 激活只作用于普通用户：管理员在创建时已设置口令与验证器。 */
  activate(activationToken: string, password: string, signal: AbortSignal) {
    return this.exchange(
      '/api/v2/auth/activate',
      z.undefined(),
      { method: 'POST', body: { activation_token: activationToken, password }, signal },
      false,
      204,
    )
  }

  async restore() {
    const generation = this.epoch
    try {
      await this.refresh(false)
    } catch (error) {
      if (generation !== this.epoch) return
      this.update({ phase: 'unavailable', message: errorMessage(error) })
    }
  }
  async login(username: string, password: string, code: string) {
    this.clear()
    const result = await this.sessionLock('exclusive', () =>
      this.exchange(
        PREFIX + '/auth/login',
        sessionSchema,
        {
          method: 'POST',
          body: { username, password, code, device_name: 'Web 管理平台' },
        },
        true,
      ),
    )
    this.acceptSession(result)
  }
  async logout() {
    await this.request('/auth/logout', z.undefined(), { method: 'POST', body: {} })
    this.clear()
  }
  async verify(password: string, code: string) {
    await this.request('/auth/verify', z.undefined(), { method: 'POST', body: { password, code } })
  }
  /** 先检查共享 Cookie 的当前状态；轮换请求不自动重试，也不重放业务写入。 */
  refresh(force = true): Promise<void> {
    if (this.refreshPromise) return this.refreshPromise
    const generation = this.epoch
    const run = async () => {
      if (generation !== this.epoch) throw new ApiError('REQUEST_CANCELED', '会话已更换')
      try {
        const current = await this.exchange(PREFIX + '/auth/session', sessionSchema, {}, true)
        if (!force && Date.parse(current.access_expires_at) - Date.now() > REFRESH_LEAD_MS) {
          this.acceptSession(current)
          return
        }
      } catch (error) {
        if (!(error instanceof ApiError) || error.code !== 'SESSION_INVALID') throw error
      }
      // 访问 Cookie 已到期也能取回 CSRF；只读校验会拒绝已消费的续期值，避免响应丢失后的重放。
      const context = await this.exchange(
        PREFIX + '/auth/refresh-context',
        refreshContextSchema,
        {},
        true,
      )
      this.csrf = context.csrf_token
      const result = await this.exchange(
        PREFIX + '/auth/refresh',
        sessionSchema,
        {
          method: 'POST',
          body: {},
        },
        true,
      )
      this.acceptSession(result)
    }
    const promise = this.sessionLock('exclusive', run)
      .catch((error: unknown) => {
        if (generation === this.epoch) {
          if (error instanceof ApiError && error.code === 'SESSION_INVALID') this.clear()
          else this.reportRenewalFailure(error)
        }
        throw error
      })
      .finally(() => {
        if (this.refreshPromise === promise) this.refreshPromise = null
      })
    this.refreshPromise = promise
    return promise
  }
}

export function errorMessage(error: unknown): string {
  return error instanceof ApiError ? error.message : '操作失败，请重新查询当前状态'
}
