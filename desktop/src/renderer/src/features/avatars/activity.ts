import { useSyncExternalStore } from 'react'
import { MAX_AGENTS } from '../workspace/model'
import type { AgentActivity } from './pose'

export const AGENT_ACTIVITY_EVENT = 'tombot:agent-activity'
export const MAX_AGENT_ID_LENGTH = 80

const activities = new Map<string, AgentActivity>()
const listeners = new Set<() => void>()
let subscriberCount = 0

/**
 * 写入一只助手的活动。只留在内存里：刷新后回到空闲，
 * 不进工作空间，因此不需要存储迁移。重复写入不会通知订阅者。
 */
export function setAgentActivity(agentId: string, activity: AgentActivity): void {
  if (!isAgentId(agentId)) throw new Error('助手标识无效')
  if (activity !== 'idle' && activity !== 'working') {
    throw new Error('助手活动只能是空闲或工作中')
  }
  const current = activities.get(agentId) ?? 'idle'
  if (current === activity) return
  if (activity === 'idle') {
    activities.delete(agentId)
  } else if (!activities.has(agentId) && activities.size >= MAX_AGENTS) {
    throw new Error(`同时工作的助手不能超过 ${MAX_AGENTS} 个`)
  } else {
    activities.set(agentId, activity)
  }
  for (const listener of listeners) listener()
}

/** 读取活动。未知助手视为空闲；标识本身不合法时失败，避免和空闲混在一起。 */
export function getAgentActivity(agentId: string): AgentActivity {
  if (!isAgentId(agentId)) throw new Error('助手标识无效')
  return activities.get(agentId) ?? 'idle'
}

/** 传输层断开时清掉全部工作中状态。没有活动时不通知。 */
export function clearAgentActivities(): void {
  if (activities.size === 0) return
  activities.clear()
  for (const listener of listeners) listener()
}

/** 订阅一只助手的活动。没有标识时保持空闲，选择器和产品标志不进入工作中。 */
export function useAgentActivity(agentId?: string): AgentActivity {
  return useSyncExternalStore(
    subscribeAgentActivity,
    () => (agentId ? getAgentActivity(agentId) : 'idle'),
    () => 'idle',
  )
}

export function subscribeAgentActivity(listener: () => void): () => void {
  if (typeof listener !== 'function') throw new Error('活动订阅无效')
  listeners.add(listener)
  let isActive = true
  return () => {
    if (!isActive) return
    isActive = false
    listeners.delete(listener)
  }
}

/**
 * 让同一渲染进程里的传输层用事件驱动活动，而不必放宽预加载桥接。
 * 非法事件只警告，避免一条坏消息把界面打崩；状态不会被改成成功。
 */
export function installAgentActivityEvents(): () => void {
  if (subscriberCount === 0) {
    window.addEventListener(AGENT_ACTIVITY_EVENT, handleAgentActivityEvent)
  }
  subscriberCount += 1
  let isReleased = false
  return () => {
    if (isReleased) return
    isReleased = true
    subscriberCount -= 1
    if (subscriberCount === 0) {
      window.removeEventListener(AGENT_ACTIVITY_EVENT, handleAgentActivityEvent)
    }
  }
}

function handleAgentActivityEvent(event: Event): void {
  if (!(event instanceof CustomEvent)) {
    console.warn('忽略无效的助手活动事件，原因=不是活动消息')
    return
  }
  let detail: { agentId: string; activity: AgentActivity }
  try {
    detail = readActivityDetail(event.detail)
  } catch (error) {
    console.warn(`忽略无效的助手活动事件，原因=${errorText(error)}`)
    return
  }
  try {
    setAgentActivity(detail.agentId, detail.activity)
  } catch (error) {
    console.warn(`助手活动未更新，助手=${detail.agentId}，原因=${errorText(error)}`)
  }
}

function readActivityDetail(value: unknown): { agentId: string; activity: AgentActivity } {
  if (!value || typeof value !== 'object') throw new Error('活动事件缺少内容')
  const detail = value as { agentId?: unknown; activity?: unknown }
  if (detail.activity !== 'idle' && detail.activity !== 'working') {
    throw new Error('活动只能是空闲或工作中')
  }
  if (typeof detail.agentId !== 'string') throw new Error('助手标识无效')
  return { agentId: detail.agentId, activity: detail.activity }
}

function isAgentId(value: string): boolean {
  return (
    typeof value === 'string' &&
    value.length >= 1 &&
    value.length <= MAX_AGENT_ID_LENGTH &&
    value.trim() === value &&
    !hasControlCharacter(value)
  )
}

/** 控制字符会让同一只猫在日志和事件里对不上，标识里不能出现。 */
function hasControlCharacter(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    if (value.charCodeAt(index) <= 31) return true
  }
  return false
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : '未知错误'
}
