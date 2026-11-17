import { expect, it, vi } from 'vitest'
import { loadApplicationAbout } from './applicationAbout'
it('读取实际版本与地址，请求具备超时且不携带凭据', async () => {
  const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ server_version: '2.3.4' })))
  await expect(
    loadApplicationAbout('1.2.3', 'https://tom.example.test', { fetch }),
  ).resolves.toEqual({
    clientVersion: '1.2.3',
    serverUrl: 'https://tom.example.test',
    serverVersion: '2.3.4',
  })
  expect(fetch).toHaveBeenCalledWith(
    'https://tom.example.test/api/v2/version',
    expect.objectContaining({
      signal: expect.any(AbortSignal),
      credentials: 'omit',
      redirect: 'error',
    }),
  )
})
it('服务异常或版本缺失保留客户端信息并明确报告错误', async () => {
  for (const response of [new Response('{}'), new Response('', { status: 503 })]) {
    const result = await loadApplicationAbout('1.2.3', 'https://tom.example.test', {
      fetch: vi.fn().mockResolvedValue(response),
    })
    expect(result.clientVersion).toBe('1.2.3')
    expect(result.serverVersion).toBe('')
    expect(result.serverError).toBeTruthy()
  }
})
