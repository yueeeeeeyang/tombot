import { afterEach, describe, expect, it, vi } from 'vitest'
import { MAX_AGENTS } from '../workspace/model'
import {
  AGENT_ACTIVITY_EVENT,
  MAX_AGENT_ID_LENGTH,
  clearAgentActivities,
  getAgentActivity,
  installAgentActivityEvents,
  setAgentActivity,
  subscribeAgentActivity,
} from './activity'

describe('助手活动', () => {
  afterEach(() => {
    clearAgentActivities()
  })

  it('写入工作中并在重复写入时保持安静', () => {
    const listener = vi.fn()
    const unsubscribe = subscribeAgentActivity(listener)
    expect(getAgentActivity('assistant')).toBe('idle')
    setAgentActivity('assistant', 'idle')
    expect(listener).not.toHaveBeenCalled()
    setAgentActivity('assistant', 'working')
    setAgentActivity('assistant', 'working')
    expect(getAgentActivity('assistant')).toBe('working')
    expect(listener).toHaveBeenCalledTimes(1)
    setAgentActivity('assistant', 'idle')
    expect(getAgentActivity('assistant')).toBe('idle')
    expect(listener).toHaveBeenCalledTimes(2)
    unsubscribe()
    unsubscribe()
    setAgentActivity('developer', 'working')
    expect(listener).toHaveBeenCalledTimes(2)
  })

  it('未知助手视为空闲，清空时只在确实有活动时通知', () => {
    const listener = vi.fn()
    const unsubscribe = subscribeAgentActivity(listener)
    expect(getAgentActivity('missing-agent')).toBe('idle')
    clearAgentActivities()
    expect(listener).not.toHaveBeenCalled()
    setAgentActivity('assistant', 'working')
    setAgentActivity('developer', 'working')
    clearAgentActivities()
    expect(getAgentActivity('assistant')).toBe('idle')
    expect(getAgentActivity('developer')).toBe('idle')
    expect(listener).toHaveBeenCalledTimes(3)
    unsubscribe()
  })

  it('不把活动写进本地存储', () => {
    localStorage.setItem('tombot.workspace.v2', 'keep')
    setAgentActivity('assistant', 'working')
    expect(localStorage.getItem('tombot.workspace.v2')).toBe('keep')
    expect(localStorage.length).toBe(1)
  })

  it('拒绝空标识、超长标识、未修剪文本和超额的工作中助手', () => {
    expect(() => setAgentActivity('', 'working')).toThrow('助手标识无效')
    expect(() => setAgentActivity(' assistant', 'working')).toThrow('助手标识无效')
    expect(() => setAgentActivity('assistant\n', 'working')).toThrow('助手标识无效')
    expect(() => getAgentActivity(1 as unknown as string)).toThrow('助手标识无效')
    expect(() => setAgentActivity('a'.repeat(MAX_AGENT_ID_LENGTH + 1), 'working')).toThrow(
      '助手标识无效',
    )
    setAgentActivity('a'.repeat(MAX_AGENT_ID_LENGTH), 'working')
    expect(getAgentActivity('a'.repeat(MAX_AGENT_ID_LENGTH))).toBe('working')
    clearAgentActivities()

    for (let index = 0; index < MAX_AGENTS; index += 1) {
      setAgentActivity(`agent-${index}`, 'working')
    }
    expect(() => setAgentActivity('agent-extra', 'working')).toThrow(
      `同时工作的助手不能超过 ${MAX_AGENTS} 个`,
    )
    expect(getAgentActivity('agent-extra')).toBe('idle')
    expect(getAgentActivity('agent-0')).toBe('working')
    setAgentActivity('agent-0', 'idle')
    setAgentActivity('agent-extra', 'working')
    expect(getAgentActivity('agent-extra')).toBe('working')
    expect(() => setAgentActivity('assistant', 'busy' as 'idle')).toThrow(
      '助手活动只能是空闲或工作中',
    )
    expect(() => subscribeAgentActivity(undefined as unknown as () => void)).toThrow('活动订阅无效')
  })

  it('用事件更新活动，非法事件只警告且可重复安装', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const first = installAgentActivityEvents()
    const second = installAgentActivityEvents()
    window.dispatchEvent(
      new CustomEvent(AGENT_ACTIVITY_EVENT, {
        detail: { agentId: 'assistant', activity: 'working', ignored: true },
      }),
    )
    expect(getAgentActivity('assistant')).toBe('working')
    first()
    window.dispatchEvent(
      new CustomEvent(AGENT_ACTIVITY_EVENT, {
        detail: { agentId: 'assistant', activity: 'idle' },
      }),
    )
    expect(getAgentActivity('assistant')).toBe('idle')
    second()
    second()
    window.dispatchEvent(
      new CustomEvent(AGENT_ACTIVITY_EVENT, {
        detail: { agentId: 'assistant', activity: 'working' },
      }),
    )
    expect(getAgentActivity('assistant')).toBe('idle')

    const release = installAgentActivityEvents()
    window.dispatchEvent(new Event(AGENT_ACTIVITY_EVENT))
    window.dispatchEvent(new CustomEvent(AGENT_ACTIVITY_EVENT, { detail: null }))
    window.dispatchEvent(
      new CustomEvent(AGENT_ACTIVITY_EVENT, {
        detail: { agentId: 'assistant', activity: 'sleeping' },
      }),
    )
    window.dispatchEvent(
      new CustomEvent(AGENT_ACTIVITY_EVENT, {
        detail: { agentId: ' assistant', activity: 'working' },
      }),
    )
    expect(getAgentActivity('assistant')).toBe('idle')
    expect(warn.mock.calls.map((call) => String(call[0]))).toEqual([
      expect.stringContaining('不是活动消息'),
      expect.stringContaining('活动事件缺少内容'),
      expect.stringContaining('活动只能是空闲或工作中'),
      expect.stringContaining('助手= assistant'),
    ])
    release()
  })
})
