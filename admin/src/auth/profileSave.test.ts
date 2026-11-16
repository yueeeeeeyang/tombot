import { afterEach, expect, it, vi } from 'vitest'
import { saveProfile } from '../../../ui/profile-save.js'

const response = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
afterEach(() => vi.unstubAllGlobals())

it('昵称与头像分别提交字段，使用同源 Cookie 和有时限的单次请求', async () => {
  const fetch = vi.fn().mockResolvedValue(response(200, { display_name: '新昵称' }))
  vi.stubGlobal('fetch', fetch)
  const csrf = crypto.randomUUID()
  await expect(
    saveProfile('/api/v2/web/auth/profile', csrf, { displayName: '新昵称' }),
  ).resolves.toEqual({ displayName: '新昵称' })
  const options = fetch.mock.calls[0]![1] as RequestInit
  expect(options.method).toBe('POST')
  expect(options.credentials).toBe('same-origin')
  expect(options.redirect).toBe('error')
  expect(options.signal).toBeInstanceOf(AbortSignal)
  const nicknameForm = options.body as FormData
  expect([...nicknameForm.keys()]).toEqual(['csrf', 'display_name'])
  expect(nicknameForm.get('csrf')).toBe(csrf)
  expect(nicknameForm.get('display_name')).toBe('新昵称')
  fetch.mockResolvedValue(response(200, { display_name: '最新昵称' }))
  await saveProfile('/api/v2/web/auth/profile', csrf, {
    avatar: new Blob(['image'], { type: 'image/png' }),
  })
  const avatarForm = fetch.mock.calls[1]![1].body as FormData
  expect([...avatarForm.keys()]).toEqual(['csrf', 'avatar'])
  expect((avatarForm.get('avatar') as File).type).toBe('image/png')
})

it('业务拒绝与失效会话不显示成功，不自动重试', async () => {
  const fetch = vi.fn()
  vi.stubGlobal('fetch', fetch)
  for (const [status, expected] of [
    [401, '登录已失效'],
    [403, '页面校验已失效'],
    [503, '服务暂不可用'],
  ] as const) {
    fetch.mockResolvedValue(response(status, { message: '服务暂不可用' }))
    await expect(
      saveProfile('/api/v2/web/auth/profile', crypto.randomUUID(), { displayName: '昵称' }),
    ).rejects.toThrow(expected)
  }
  expect(fetch).toHaveBeenCalledTimes(3)
})

it('断网、超时或响应异常保留结果未确认状态，禁止默认成功', async () => {
  const fetch = vi.fn()
  vi.stubGlobal('fetch', fetch)
  for (const failure of [new TypeError('network'), new DOMException('timeout', 'TimeoutError')]) {
    fetch.mockRejectedValue(failure)
    await expect(
      saveProfile('/api/v2/web/auth/profile', crypto.randomUUID(), { displayName: '昵称' }),
    ).rejects.toThrow('保存结果未确认')
  }
  for (const invalid of [response(200, {}), new Response('invalid', { status: 200 })]) {
    fetch.mockResolvedValue(invalid)
    await expect(
      saveProfile('/api/v2/web/auth/profile', crypto.randomUUID(), { displayName: '昵称' }),
    ).rejects.toThrow('保存结果未确认')
  }
  expect(fetch).toHaveBeenCalledTimes(4)
})
