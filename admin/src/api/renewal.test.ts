import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { z } from 'zod'
import { AdminClient } from './client'
import { empty, failure, json, session } from '../../tests/fixtures'

function browserSession() {
  let current = session()
  let accessValid = true
  let refreshValid = true
  let offline = false
  let loseResponse = false
  let installCookies = true
  const calls: string[] = []
  const fetcher = vi.fn(async (url: RequestInfo | URL, options: RequestInit = {}) => {
    const path = String(url).replace('/api/v2/admin', '')
    calls.push(path)
    if (offline) throw new TypeError('测试网络离线')
    if (path === '/auth/session')
      return accessValid && Date.parse(current.access_expires_at) > Date.now()
        ? json(current)
        : failure('SESSION_INVALID', '访问凭据已过期', 401)
    if (path === '/auth/refresh-context')
      return refreshValid
        ? json({ csrf_token: current.csrf_token })
        : failure('SESSION_INVALID', '续期凭据已失效', 401)
    if (path === '/auth/refresh') {
      expect(new Headers(options.headers).get('X-CSRF-Token')).toBe(current.csrf_token)
      if (!refreshValid) return failure('SESSION_INVALID', '续期凭据已失效', 401)
      if (installCookies) {
        current = { ...session(), principal: current.principal }
        accessValid = true
      } else {
        refreshValid = false
        accessValid = false
      }
      if (loseResponse) throw new TypeError('测试续期响应丢失')
      return json(current)
    }
    return empty()
  })
  return {
    client: new AdminClient(fetcher),
    fetcher,
    calls,
    expireAccess: () => {
      accessValid = false
    },
    setOffline: (value: boolean) => {
      offline = value
    },
    loseRefreshResponse: (cookiesApplied: boolean) => {
      loseResponse = true
      installCookies = cookiesApplied
    },
    rotateElsewhere: () => {
      current = { ...session(), principal: current.principal }
    },
    current: () => current,
  }
}

describe('管理会话自动续期', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => {
    vi.clearAllTimers()
    vi.useRealTimers()
  })

  it('刷新页面恢复真实到期时间，并在到期前自动轮换', async () => {
    const fixture = browserSession()
    await fixture.client.restore()
    await vi.advanceTimersByTimeAsync(540_000)
    expect(fixture.calls.filter((path) => path === '/auth/refresh')).toHaveLength(1)
    expect(fixture.client.snapshot().phase).toBe('authenticated')
    await vi.advanceTimersByTimeAsync(540_000)
    expect(fixture.calls.filter((path) => path === '/auth/refresh')).toHaveLength(2)
  })

  it('重新打开页面时访问 Cookie 已过期，仍可用续期 Cookie 恢复', async () => {
    const fixture = browserSession()
    fixture.expireAccess()
    await fixture.client.restore()
    expect(fixture.calls).toEqual(['/auth/session', '/auth/refresh-context', '/auth/refresh'])
    expect(fixture.client.snapshot().phase).toBe('authenticated')
  })

  it('后台定时器被推迟后恢复前台，合并并发续期并等待完成再执行业务', async () => {
    const fixture = browserSession()
    await fixture.client.restore()
    vi.setSystemTime(Date.now() + 700_000)
    fixture.client.resumeSessionClock()
    await fixture.client.request('/users', z.undefined())
    expect(fixture.calls.slice(-4)).toEqual([
      '/auth/session',
      '/auth/refresh-context',
      '/auth/refresh',
      '/users',
    ])
    expect(fixture.client.snapshot().phase).toBe('authenticated')
  })

  it('两个标签页串行检查共享 Cookie，只轮换一次且旧页写入使用最新 CSRF', async () => {
    let queue: Promise<unknown> = Promise.resolve()
    Object.defineProperty(navigator, 'locks', {
      configurable: true,
      value: {
        request: (_name: string, _options: LockOptions, callback: () => Promise<unknown>) => {
          const next = queue.then(callback)
          queue = next
          return next
        },
      },
    })
    const fixture = browserSession()
    const other = new AdminClient(fixture.fetcher)
    await Promise.all([fixture.client.restore(), other.restore()])
    vi.setSystemTime(Date.now() + 700_000)
    await Promise.all([fixture.client.refresh(false), other.refresh(false)])
    expect(fixture.calls.filter((path) => path === '/auth/refresh')).toHaveLength(1)
    fixture.rotateElsewhere()
    await other.request('/users', z.undefined(), { method: 'POST', body: {} })
    expect(new Headers(fixture.fetcher.mock.calls.at(-1)![1]?.headers).get('X-CSRF-Token')).toBe(
      fixture.current().csrf_token,
    )
  })

  it('网络中断显示续期错误并保留页面，恢复网络后可继续续期', async () => {
    const fixture = browserSession()
    await fixture.client.restore()
    fixture.setOffline(true)
    await vi.advanceTimersByTimeAsync(540_000)
    expect(fixture.client.snapshot()).toMatchObject({
      phase: 'authenticated',
      renewalError: expect.stringContaining('连接失败'),
    })
    fixture.setOffline(false)
    fixture.client.resumeSessionClock()
    await fixture.client.refresh(false)
    expect(fixture.client.snapshot()).toEqual({
      phase: 'authenticated',
      principal: fixture.current().principal,
    })
    expect(fixture.calls.filter((path) => path === '/auth/refresh')).toHaveLength(1)
  })

  it.each([true, false])(
    '续期响应丢失后先只读核实，不重发已消费的续期值（Cookie 已应用：%s）',
    async (cookiesApplied) => {
      const fixture = browserSession()
      await fixture.client.restore()
      fixture.loseRefreshResponse(cookiesApplied)
      await expect(fixture.client.refresh()).rejects.toMatchObject({ code: 'CONNECTION_FAILED' })
      if (cookiesApplied) {
        await fixture.client.refresh(false)
        expect(fixture.client.snapshot().phase).toBe('authenticated')
      } else {
        await expect(fixture.client.refresh(false)).rejects.toMatchObject({
          code: 'SESSION_INVALID',
        })
        expect(fixture.client.snapshot().phase).toBe('anonymous')
      }
      expect(fixture.calls.filter((path) => path === '/auth/refresh')).toHaveLength(1)
    },
  )

  it('再验证密码错误不注销有效管理会话，写请求鉴权失败也不自动重放', async () => {
    const authentication = session()
    const writes: string[] = []
    const client = new AdminClient(async (url) => {
      const path = String(url)
      if (path.endsWith('/session')) return json(authentication)
      writes.push(path)
      return path.endsWith('/verify')
        ? failure('AUTH_INVALID', '验证信息无效', 401)
        : failure('SESSION_INVALID', '会话需要恢复', 401)
    })
    await client.restore()
    await expect(client.verify(crypto.randomUUID(), '000000')).rejects.toMatchObject({
      code: 'AUTH_INVALID',
    })
    expect(client.snapshot().phase).toBe('authenticated')
    await expect(
      client.request('/users', z.unknown(), { method: 'POST', body: {} }),
    ).rejects.toMatchObject({ code: 'SESSION_RENEWED' })
    expect(writes.filter((path) => path.endsWith('/users'))).toHaveLength(1)
    expect(client.snapshot().phase).toBe('authenticated')
  })

  it('更换身份后，迟到的后台续期不能污染新会话', async () => {
    const previous = session()
    const next = session()
    next.principal.session_id = '0'.repeat(25) + '8'
    let delayed = false
    let finish: ((response: Response) => void) | undefined
    const client = new AdminClient(async (url) => {
      if (String(url).endsWith('/login')) return json(next)
      if (delayed)
        return new Promise<Response>((resolve) => {
          finish = resolve
        })
      return json(previous)
    })
    await client.restore()
    delayed = true
    vi.setSystemTime(Date.now() + 700_000)
    client.resumeSessionClock()
    await client.login('another-admin', crypto.randomUUID(), '000000')
    finish!(json(previous))
    await vi.advanceTimersByTimeAsync(0)
    expect(client.snapshot()).toEqual({ phase: 'authenticated', principal: next.principal })
  })
})
