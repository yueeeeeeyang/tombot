import { describe, expect, it, vi } from 'vitest'
import { z } from 'zod'
import { AdminClient, ApiError } from './client'
import {
  activationInputs,
  enrollment,
  empty,
  failure,
  json,
  makeClient,
  session,
} from '../../tests/fixtures'

describe('管理传输边界', () => {
  it('激活仅走固定公开认证路径，不携带管理会话且不自动登录', async () => {
    const authentication = session()
    const setup = enrollment()
    const input = activationInputs()
    const calls: { path: string; options: RequestInit }[] = []
    const client = new AdminClient(async (url, options = {}) => {
      const path = String(url)
      calls.push({ path, options })
      if (path.endsWith('/session')) return json(authentication)
      if (path.endsWith('/admin-enrollment')) return json(setup)
      return empty()
    })
    await client.restore()
    const signal = new AbortController().signal
    const received = await client.beginEnrollment(input.token, signal)
    expect(received.secret === setup.secret).toBe(true)
    await client.activate(input.token, input.password, input.code, signal)
    expect(calls.slice(1).map((call) => call.path)).toEqual([
      '/api/v2/auth/admin-enrollment',
      '/api/v2/auth/activate',
    ])
    for (const call of calls.slice(1)) {
      expect(call.options.credentials).toBe('omit')
      expect(call.options.cache).toBe('no-store')
      expect(call.options.redirect).toBe('error')
      expect(new Headers(call.options.headers).has('X-CSRF-Token')).toBe(false)
      expect(call.path.includes(input.token)).toBe(false)
    }
    expect(client.snapshot().phase).toBe('authenticated')
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })
  it('拒绝错误登记协议，激活失败不清空无关管理会话', async () => {
    const fixture = makeClient((path) =>
      path.endsWith('/admin-enrollment')
        ? json({ ...enrollment(), period: 60 })
        : failure('AUTH_INVALID', '验证失败', 401),
    )
    await fixture.client.restore()
    const input = activationInputs()
    const signal = new AbortController().signal
    await expect(fixture.client.beginEnrollment(input.token, signal)).rejects.toMatchObject({
      code: 'RESPONSE_INVALID',
    })
    await expect(
      fixture.client.activate(input.token, input.password, input.code, signal),
    ).rejects.toMatchObject({ code: 'AUTH_INVALID' })
    expect(fixture.client.snapshot().phase).toBe('authenticated')
  })
  it('激活接口仅受理但未完成时不能报告激活成功', async () => {
    const client = new AdminClient(async () => new Response(null, { status: 202 }))
    const input = activationInputs()
    await expect(
      client.activate(input.token, input.password, input.code, new AbortController().signal),
    ).rejects.toMatchObject({ code: 'RESPONSE_INVALID' })
  })
  it('使用同源 Cookie 与内存 CSRF，不持久化认证材料', async () => {
    const fixture = makeClient(() => empty())
    await fixture.client.restore()
    await fixture.client.request('/users', z.undefined(), {
      method: 'POST',
      body: { username: '用户输入' },
    })
    const call = fixture.calls.at(-1)!
    expect(call.options.credentials).toBe('same-origin')
    expect(call.options.redirect).toBe('error')
    expect(call.options.cache).toBe('no-store')
    expect(new Headers(call.options.headers).get('X-CSRF-Token')).toBe(
      fixture.authentication.csrf_token,
    )
    expect(new Headers(call.options.headers).has('Authorization')).toBe(false)
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })
  it('超时写入保留结果未知且不自动重放', async () => {
    const fetcher = vi.fn(
      async (_input: unknown, options?: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          options?.signal?.addEventListener('abort', () =>
            reject(new DOMException('取消', 'AbortError')),
          )
        }),
    )
    const client = new AdminClient(fetcher as typeof fetch)
    await expect(
      client.request('/users', z.unknown(), { method: 'POST', body: {}, timeout: 10 }),
    ).rejects.toMatchObject({ code: 'REQUEST_CANCELED' })
    expect(fetcher).toHaveBeenCalledTimes(1)
  })
  it('鉴权失效清空状态，迟到业务响应不能回到旧会话', async () => {
    let finish: ((response: Response) => void) | undefined
    const fixture = makeClient((path) =>
      path === '/late'
        ? new Promise<Response>((resolve) => {
            finish = resolve
          })
        : failure('SESSION_INVALID', '登录已失效', 401),
    )
    await fixture.client.restore()
    const late = fixture.client.request('/late', z.object({ value: z.string() }))
    const rejected = expect(late).rejects.toMatchObject({ code: 'REQUEST_CANCELED' })
    await expect(fixture.client.request('/users', z.unknown())).rejects.toMatchObject({
      code: 'SESSION_INVALID',
    })
    expect(fixture.client.snapshot().phase).toBe('anonymous')
    finish!(json({ value: '旧账户内容' }))
    await rejected
  })
  it('退出失败保留已登录状态并显示真实错误', async () => {
    const authentication = session()
    const client = new AdminClient(async (path) =>
      String(path).endsWith('/session')
        ? json(authentication)
        : failure('IDENTITY_UNAVAILABLE', '撤销未确认', 503),
    )
    await client.restore()
    await expect(client.logout()).rejects.toMatchObject({ code: 'IDENTITY_UNAVAILABLE' })
    expect(client.snapshot().phase).toBe('authenticated')
  })
  it('拒绝错误身份、畸形正文、超大响应及外部路径', async () => {
    const authentication = session()
    authentication.principal.role = 'user'
    const client = new AdminClient(async () => json(authentication))
    await client.restore()
    expect(client.snapshot().phase).toBe('unavailable')
    const malformed = new AdminClient(async () => new Response('<html>服务不可用</html>'))
    await expect(malformed.request('/users', z.unknown())).rejects.toMatchObject({
      code: 'RESPONSE_INVALID',
    })
    const oversized = new AdminClient(async () => new Response(' '.repeat(1024 * 1024 + 1)))
    await expect(oversized.request('/users', z.unknown())).rejects.toMatchObject({
      code: 'RESPONSE_INVALID',
    })
    await expect(malformed.request('//outside.test', z.unknown())).rejects.toBeInstanceOf(ApiError)
  })
  it('并发续期合并为一次，使用最新 CSRF，不重放业务写入', async () => {
    const fixture = makeClient(() => empty())
    const request = vi.fn(async (_name: string, callback: () => Promise<void>) => callback())
    Object.defineProperty(navigator, 'locks', { configurable: true, value: { request } })
    try {
      await fixture.client.restore()
      await Promise.all([fixture.client.refresh(), fixture.client.refresh()])
      expect(fixture.calls.filter((call) => call.path === '/auth/refresh')).toHaveLength(1)
      expect(request).toHaveBeenCalledTimes(1)
    } finally {
      Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined })
    }
  })
})
