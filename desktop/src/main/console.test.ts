import { describe, expect, it, vi } from 'vitest'
import { openConsole, controlConsole } from './console'
const id = '01ARZ3NDEKTSV4RRFFQ69G5FAV'
const bot = '01ARZ3NDEKTSV4RRFFQ69G5FAW'
const console = {
  id,
  bot_id: bot,
  state: 'active',

  error_code: '',
  expires_at: '2026-11-17T00:00:00Z',
}
describe('受限电脑接管通道', () => {
  it('只使用固定用户接口并核对响应归属', async () => {
    const call = vi.fn().mockResolvedValue({ console })
    await expect(openConsole(call, bot, id)).resolves.toEqual(console)
    expect(call).toHaveBeenCalledWith('POST', `/bots/${bot}/console`, { client_request_id: id })
    await expect(controlConsole(call, id, 'close')).resolves.toEqual(console)
    call.mockResolvedValue({ console: { ...console, id: bot } })
    await expect(controlConsole(call, id, 'heartbeat')).rejects.toThrow('不匹配')
    await expect(openConsole(call, id, bot)).rejects.toThrow('不匹配')
    await expect(openConsole(call, 'https://other.test', id)).rejects.toThrow()
  })
  it('控制动作失败不自动重放，不接受错误状态', async () => {
    const call = vi.fn().mockRejectedValue(new Error('中断'))
    await expect(controlConsole(call, id, 'takeover')).rejects.toThrow('中断')
    expect(call).toHaveBeenCalledTimes(1)
    call.mockResolvedValue({ console: { ...console, state: 'unknown' } })
    await expect(controlConsole(call, id, 'release')).rejects.toThrow()
  })
})
