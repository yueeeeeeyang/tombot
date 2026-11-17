import { describe, expect, it, vi } from 'vitest'
import { listTasks } from './tasks'
import { taskFixture } from '../shared/tasks.fixture'

describe('任务服务协议', () => {
  it('读取全部游标页，保留运行状态', async () => {
    const call = vi
      .fn()
      .mockResolvedValueOnce({ tasks: [taskFixture], next_before: 'cursor' })
      .mockResolvedValueOnce({
        tasks: [{ ...taskFixture, id: 'second', last_status: 'succeeded' }],
        next_before: '',
      })
    const tasks = await listTasks(call, 'assistant')
    expect(tasks).toHaveLength(2)
    expect(tasks[1]?.last_status).toBe('succeeded')
    expect(call.mock.calls).toEqual([
      ['GET', '/bots/assistant/tasks'],
      ['GET', '/bots/assistant/tasks?before=cursor'],
    ])
  })
  it('拒绝错误结构、跨猫咪、重复页和服务错误', async () => {
    for (const reply of [
      [],
      {},
      { tasks: [{ ...taskFixture, timezone: 'Invalid/Zone' }], next_before: '' },
      { tasks: [{ ...taskFixture, next_run_at: 'tomorrow' }], next_before: '' },
      { tasks: [{ ...taskFixture, bot_id: 'other' }], next_before: '' },
    ]) {
      await expect(listTasks(vi.fn().mockResolvedValue(reply), 'assistant')).rejects.toThrow()
    }
    const repeat = vi.fn().mockResolvedValue({ tasks: [taskFixture], next_before: 'cursor' })
    await expect(listTasks(repeat, 'assistant')).rejects.toThrow('任务列表在读取时发生变化')
    expect(repeat).toHaveBeenCalledTimes(2)
    const fail = vi.fn().mockRejectedValue(new Error('服务离线'))
    await expect(listTasks(fail, 'assistant')).rejects.toThrow('服务离线')
    expect(fail).toHaveBeenCalledOnce()
  })
})
