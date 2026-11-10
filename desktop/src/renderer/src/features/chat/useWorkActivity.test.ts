import { act, renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { getAgentActivity } from '../avatars/activity'
import type { StreamEvent } from '../desktop/backend'
import { useWorkActivity } from './useWorkActivity'

const activity = (
  botId: string,
  runId: string,
  isWorking: boolean,
  isFinished = false,
): StreamEvent => ({
  type: 'activity',
  text: '',
  botId,
  runId,
  isWorking,
  isFinished,
})

describe('实际执行活动', () => {
  it('交付后发送者空闲，目标执行，收到结果恢复；停止后丢弃迟到摘要', () => {
    const { result, unmount } = renderHook(useWorkActivity)
    const receive = (event: StreamEvent) =>
      act(() => result.current.receiveActivity('request', event))
    receive(activity('sender', 'root', false))
    expect(result.current.workStates).toEqual({})
    receive(activity('sender', 'root', true))
    expect(getAgentActivity('sender')).toBe('working')
    receive(activity('sender', 'root', false))
    receive(activity('recipient', 'child', true))
    receive({ type: 'process', text: '正在核验资料', botId: 'recipient', runId: 'child' })
    expect(result.current.workStates).toEqual({ recipient: { summary: '正在核验资料' } })
    expect(getAgentActivity('sender')).toBe('idle')
    expect(getAgentActivity('recipient')).toBe('working')
    receive(activity('recipient', 'child', false, true))
    receive({ type: 'process', text: '迟到摘要', botId: 'recipient', runId: 'child' })
    receive(activity('recipient', 'child', true))
    expect(result.current.workStates).toEqual({})
    receive(activity('sender', 'root', true))
    expect(result.current.workStates).toEqual({ sender: { summary: '' } })
    unmount()
    expect(getAgentActivity('sender')).toBe('idle')
  })

  it('没有回复不会重新开始；失败、取消、断线只清理所属请求', () => {
    const { result } = renderHook(useWorkActivity)
    act(() => {
      result.current.receiveActivity('first', activity('sender', 'root', true))
      result.current.receiveActivity('first', activity('recipient', 'child', true))
      result.current.receiveActivity('first', activity('sender', 'root', false))
      result.current.receiveActivity('second', activity('recipient', 'other', true))
      result.current.receiveActivity('first', activity('recipient', 'child', false, true))
    })
    expect(getAgentActivity('sender')).toBe('idle')
    expect(getAgentActivity('recipient')).toBe('working')
    act(() => result.current.clearActivity('first'))
    expect(result.current.workStates).toEqual({ recipient: { summary: '' } })
    act(() => result.current.clearActivity('second'))
    expect(result.current.workStates).toEqual({})
    expect(getAgentActivity('recipient')).toBe('idle')
  })

  it('缺少身份、其他运行的摘要和未执行时的摘要都不激活动画', () => {
    const { result } = renderHook(useWorkActivity)
    act(() => {
      result.current.receiveActivity('request', { type: 'activity', text: '', isWorking: true })
      result.current.receiveActivity('request', {
        type: 'process',
        text: '摘要',
        runId: 'unknown',
        botId: 'recipient',
      })
      result.current.receiveActivity('request', activity('sender', 'root', true))
      result.current.receiveActivity('request', activity('recipient', 'root', true))
      result.current.receiveActivity('request', activity('sender', 'root', false))
      result.current.receiveActivity('request', {
        type: 'process',
        text: '旧摘要',
        runId: 'root',
        botId: 'sender',
      })
    })
    expect(result.current.workStates).toEqual({})
  })
})
