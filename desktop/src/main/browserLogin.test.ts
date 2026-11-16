import { createHash, randomBytes } from 'node:crypto'
import { setTimeout, clearTimeout } from 'node:timers'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  handlers: new Map<string, (...args: unknown[]) => Promise<unknown>>(),
  readFile: vi.fn(),
  writeFile: vi.fn(),
  openExternal: vi.fn(),
  trustCertificate: vi.fn(),
}))

it('历史与状态并发读取只续期一次，避免重复消费刷新凭据', async () => {
  mocks.readFile.mockResolvedValue(
    JSON.stringify({
      serverUrl: base,
      accessToken: token(),
      refreshToken: token(),
      accessExpiresAt: new Date(0).toISOString(),
    }),
  )
  let release!: () => void
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  let renewals = 0
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => {
      if (url.endsWith('/auth/refresh')) {
        renewals++
        await gate
        return Response.json({
          access_token: token(),
          refresh_token: token(),
          access_expires_at: new Date(Date.now() + 300_000).toISOString(),
        })
      }
      if (url.includes('/workspace'))
        return Response.json({
          snapshot: { bots: [], groups: [], workspace: { revision: 1 }, event_sequence: 0 },
        })
      throw new Error('非预期请求')
    }),
  )
  const reads = [invoke('desktop:load-workspace'), invoke('desktop:load-workspace')]
  await vi.waitFor(() => expect(renewals).toBe(1))
  release()
  for (const result of await Promise.all(reads)) expect(result).toMatchObject({ ok: true })
  expect(renewals).toBe(1)
})
vi.mock('./serverTransport', () => ({
  ServerTransport: class {
    fetch(url: string, options: RequestInit) {
      return fetch(url, options)
    }
    trustCertificate = mocks.trustCertificate
  },
}))
vi.mock('electron', () => ({
  app: { getPath: () => '/测试用户目录' },
  ipcMain: {
    on: (name: string, handler: (...args: unknown[]) => Promise<unknown>) =>
      mocks.handlers.set(name, handler),
    handle: (name: string, handler: (...args: unknown[]) => Promise<unknown>) =>
      mocks.handlers.set(name, handler),
  },
  shell: { openExternal: mocks.openExternal },
  clipboard: {},
}))
vi.mock('node:fs/promises', () => ({
  readFile: mocks.readFile,
  writeFile: mocks.writeFile,
  mkdir: vi.fn(),
  chmod: vi.fn(),
  lstat: vi.fn(),
  open: vi.fn(),
}))

const realFetch = globalThis.fetch
const token = () => randomBytes(32).toString('base64url')
const base = 'https://desktop.example.test'

beforeEach(async () => {
  vi.resetModules()
  mocks.handlers.clear()
  mocks.trustCertificate.mockReset()
  mocks.readFile.mockResolvedValue(JSON.stringify({ serverUrl: base }))
  mocks.writeFile.mockResolvedValue(undefined)
  vi.stubEnv('TOMBOT_SERVER_URL', '')
  vi.stubGlobal('setTimeout', setTimeout)
  vi.stubGlobal('clearTimeout', clearTimeout)
  const { registerDesktopIpc } = await import('./desktopServer')
  registerDesktopIpc()
})
afterEach(() => {
  vi.unstubAllGlobals()
  vi.unstubAllEnvs()
})

async function invoke(name: string, ...args: unknown[]) {
  const handler = mocks.handlers.get(name)
  if (!handler) throw new Error('测试通道未注册')
  return handler(...args)
}

describe('主进程服务地址确认', () => {
  it('客户端不再注册模型配置或密钥提交通道', () => {
    for (const channel of [
      'desktop:load-settings',
      'desktop:save-settings',
      'desktop:model-providers',
      'desktop:provider-models',
    ]) {
      expect(mocks.handlers.has(channel)).toBe(false)
    }
  })

  it('证书信任绑定当前地址，验证期间不能切换地址或开始授权', async () => {
    let finish!: () => void
    mocks.trustCertificate.mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          finish = resolve
        }),
    )
    expect(
      await invoke(
        'desktop:trust-server-certificate',
        undefined,
        'https://other.example.test',
        '0'.repeat(64),
      ),
    ).toMatchObject({ ok: false, code: 'SERVER_CHANGED' })
    expect(mocks.trustCertificate).not.toHaveBeenCalled()
    const pending = invoke('desktop:trust-server-certificate', undefined, base, '0'.repeat(64))
    await vi.waitFor(() =>
      expect(mocks.trustCertificate).toHaveBeenCalledWith(base, '0'.repeat(64)),
    )
    expect(await invoke('desktop:connect', undefined, 'https://other.example.test')).toMatchObject({
      ok: false,
      code: 'AUTH_PENDING',
    })
    expect(await invoke('desktop:browser-login')).toMatchObject({ ok: false, code: 'AUTH_PENDING' })
    finish()
    expect(await pending).toMatchObject({ ok: true })
    expect(await invoke('desktop:connect', undefined, 'https://other.example.test')).toMatchObject({
      ok: true,
    })
  })

  it('验证失败保留错误分类并解除操作锁', async () => {
    mocks.trustCertificate.mockRejectedValue(
      Object.assign(new Error('指纹不匹配'), { code: 'CERTIFICATE_FINGERPRINT_MISMATCH' }),
    )
    expect(
      await invoke('desktop:trust-server-certificate', undefined, base, '0'.repeat(64)),
    ).toMatchObject({ ok: false, code: 'CERTIFICATE_FINGERPRINT_MISMATCH' })
    expect(await invoke('desktop:connect', undefined, base)).toMatchObject({ ok: true })
  })

  it.each(['', base])('未登录启动先确认地址，已保存地址只预填：%s', async (saved) => {
    mocks.readFile.mockResolvedValue(JSON.stringify({ serverUrl: saved }))
    const fetcher = vi.fn()
    vi.stubGlobal('fetch', fetcher)
    expect(await invoke('desktop:session')).toEqual({
      ok: true,
      value: { phase: 'address', serverUrl: saved },
    })
    expect(fetcher).not.toHaveBeenCalled()
    expect(await invoke('desktop:connect', undefined, base)).toEqual({
      ok: true,
      value: { phase: 'login', serverUrl: base },
    })
  })

  it('环境变量只预填，不能直接发起浏览器授权', async () => {
    mocks.readFile.mockResolvedValue('{}')
    vi.stubEnv('TOMBOT_SERVER_URL', base)
    const fetcher = vi.fn()
    vi.stubGlobal('fetch', fetcher)
    expect(await invoke('desktop:session')).toEqual({
      ok: true,
      value: { phase: 'address', serverUrl: base },
    })
    expect(await invoke('desktop:browser-login')).toMatchObject({
      ok: false,
      code: 'SERVER_URL_MISSING',
    })
    expect(fetcher).not.toHaveBeenCalled()
  })

  it('切换服务清除旧凭据，新服务必须重新登录', async () => {
    mocks.readFile.mockResolvedValue(
      JSON.stringify({
        serverUrl: base,
        accessToken: token(),
        refreshToken: token(),
        accessExpiresAt: new Date(Date.now() + 300_000).toISOString(),
        username: 'previous-user',
      }),
    )
    const fetcher = vi.fn()
    vi.stubGlobal('fetch', fetcher)
    const changed = 'https://changed.example.test:9443'
    expect(await invoke('desktop:connect', undefined, changed + '/')).toEqual({
      ok: true,
      value: { phase: 'login', serverUrl: changed },
    })
    expect(fetcher).not.toHaveBeenCalled()
    const stored = JSON.parse(String(mocks.writeFile.mock.calls.at(-1)?.[1]))
    expect(stored.serverUrl).toBe(changed)
    expect(
      [stored.accessToken, stored.refreshToken, stored.accessExpiresAt, stored.username].every(
        (value) => value === '',
      ),
    ).toBe(true)
  })

  it('有效会话继续自动恢复，续期被拒后回到地址确认', async () => {
    mocks.readFile.mockResolvedValue(
      JSON.stringify({
        serverUrl: base,
        accessToken: token(),
        refreshToken: token(),
        accessExpiresAt: new Date(Date.now() + 300_000).toISOString(),
        username: 'test-user',
      }),
    )
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(Response.json({ principal: { user_id: 'test-user' } }))
      .mockImplementation(async () => Response.json({ code: 'SESSION_INVALID' }, { status: 401 }))
    vi.stubGlobal('fetch', fetcher)
    expect(await invoke('desktop:session')).toEqual({
      ok: true,
      value: { phase: 'ready', serverUrl: base, username: 'test-user' },
    })
    expect(await invoke('desktop:session')).toEqual({
      ok: true,
      value: { phase: 'address', serverUrl: base },
    })
  })
})

describe('主进程浏览器登录', () => {
  it.each([false, true])(
    '首次登录及旧凭据过期都可发起授权，不触发续期（旧凭据：%s）',
    async (expired) => {
      if (expired)
        mocks.readFile.mockResolvedValue(
          JSON.stringify({
            serverUrl: base,
            accessToken: token(),
            refreshToken: token(),
            accessExpiresAt: new Date(0).toISOString(),
          }),
        )
      const calls: { path: string; options: RequestInit }[] = []
      let started: { state: string; redirect_uri: string; code_challenge: string }
      const authorizationCode = token()
      const session = {
        access_token: token(),
        refresh_token: token(),
        access_expires_at: new Date(Date.now() + 300_000).toISOString(),
        principal: { username: 'test-user' },
      }
      vi.stubGlobal(
        'fetch',
        vi.fn(async (input: string, options: RequestInit) => {
          calls.push({ path: input, options })
          if (input.endsWith('/browser/start')) {
            started = JSON.parse(String(options.body))
            return Response.json({ authorization_url: '/authorize?authorization=temporary' })
          }
          if (input.endsWith('/browser/exchange')) {
            const body = JSON.parse(String(options.body))
            expect(body.code === authorizationCode && body.state === started.state).toBe(true)
            expect(
              createHash('sha256').update(body.code_verifier).digest('base64url') ===
                started.code_challenge,
            ).toBe(true)
            return Response.json(session)
          }
          throw new Error('登录前不应请求会话续期或业务接口')
        }),
      )
      mocks.openExternal.mockImplementation(async (address: string) => {
        expect(address).toBe(base + '/authorize?authorization=temporary')
        expect(
          await invoke('desktop:connect', undefined, 'https://changed.example.test'),
        ).toMatchObject({ ok: false, code: 'AUTH_PENDING' })
        expect(await invoke('desktop:browser-login')).toMatchObject({
          ok: false,
          code: 'AUTH_PENDING',
        })
        const callback = new URL(started.redirect_uri)
        expect(callback.hostname).toBe('127.0.0.1')
        callback.searchParams.set('code', authorizationCode)
        callback.searchParams.set('state', started.state)
        const response = await realFetch(callback, { signal: AbortSignal.timeout(3000) })
        expect(response.status).toBe(200)
        const html = await response.text()
        expect(html).toContain('<h1>授权成功</h1>')
        expect(html.includes(authorizationCode) || html.includes(started.state)).toBe(false)
        expect(mocks.writeFile).toHaveBeenCalled()
      })
      expect(await invoke('desktop:browser-login')).toEqual({
        ok: true,
        value: { phase: 'ready', username: 'test-user' },
      })
      expect(calls.map(({ path }) => path)).toEqual([
        base + '/api/v2/auth/browser/start',
        base + '/api/v2/auth/browser/exchange',
      ])
      for (const { options } of calls) {
        expect(new Headers(options.headers).has('authorization')).toBe(false)
        expect(options.credentials).toBe('omit')
        expect(options.redirect).toBe('error')
      }
      const stored = JSON.parse(String(mocks.writeFile.mock.calls.at(-1)?.[1]))
      expect(
        stored.accessToken === session.access_token &&
          stored.refreshToken === session.refresh_token,
      ).toBe(true)
      expect(stored.username).toBe('test-user')
      expect(stored.serverUrl).toBe(base)
    },
  )

  it.each(['start', 'open', 'state', 'exchange'])(
    '阶段 %s 失败不保存会话且不自动重发',
    async (stage) => {
      const paths: string[] = []
      let started: { redirect_uri: string; state: string }
      vi.stubGlobal(
        'fetch',
        vi.fn(async (input: string, options: RequestInit) => {
          paths.push(input)
          if (input.endsWith('/browser/start')) {
            started = JSON.parse(String(options.body))
            return stage === 'start'
              ? Response.json({ code: 'AUTH_RATE_LIMITED', message: '稍后再试' }, { status: 429 })
              : Response.json({ authorization_url: '/authorize?authorization=temporary' })
          }
          return Response.json(
            { code: 'AUTHORIZATION_INVALID', message: '登录授权已失效，请重新登录' },
            { status: 400 },
          )
        }),
      )
      mocks.openExternal.mockImplementation(async () => {
        if (stage === 'open') throw new Error('无法打开系统浏览器')
        const callback = new URL(started.redirect_uri)
        callback.searchParams.set('code', token())
        callback.searchParams.set('state', stage === 'state' ? token() : started.state)
        const response = await realFetch(callback, { signal: AbortSignal.timeout(3000) })
        expect(response.status).toBe(400)
        expect(await response.text()).toContain('<h1>登录未完成</h1>')
      })
      expect(await invoke('desktop:browser-login')).toMatchObject({ ok: false })
      expect(mocks.writeFile).not.toHaveBeenCalled()
      expect(paths).toHaveLength(stage === 'exchange' ? 2 : 1)
      expect(paths.some((path) => path.endsWith('/refresh'))).toBe(false)
      expect(
        await invoke('desktop:connect', undefined, 'https://changed.example.test'),
      ).toMatchObject({
        ok: true,
        value: { phase: 'login', serverUrl: 'https://changed.example.test' },
      })
    },
  )

  it('未登录时业务入口仍不能访问，普通登录只调用公开认证接口', async () => {
    const fetcher = vi.fn(async () =>
      Response.json({
        access_token: token(),
        refresh_token: token(),
        access_expires_at: new Date(Date.now() + 300_000).toISOString(),
        principal: { username: 'test-user' },
      }),
    )
    vi.stubGlobal('fetch', fetcher)
    expect(await invoke('desktop:load-workspace')).toMatchObject({
      ok: false,
      code: 'SESSION_INVALID',
    })
    expect(fetcher).not.toHaveBeenCalled()
    expect(
      await invoke('desktop:login', undefined, { username: 'test-user', password: token() }),
    ).toMatchObject({ ok: true })
    expect(fetcher).toHaveBeenCalledTimes(1)
    expect(String((fetcher.mock.calls[0] as unknown[])[0])).toBe(base + '/api/v2/auth/login')
  })
})
