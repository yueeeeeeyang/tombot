import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { setAgentActivity } from '../avatars/activity'
import type { StreamEvent } from '../desktop/backend'

type RunActivity = {
  botId: string
  isWorking: boolean
  isFinished: boolean
  summary: string
}
type Activities = Record<string, Record<string, RunActivity>>
export type WorkStates = Record<string, { summary: string }>

/** 按运行而不是请求计数；一个任务停止时不能清掉同一只猫的其他任务。 */
export function useWorkActivity() {
  const [sources, setSources] = useState<Activities>({})
  const previousIds = useRef<string[]>([])
  const receiveActivity = useCallback((source: string, event: StreamEvent) => {
    if (!event.runId || !event.botId) return
    if (event.type !== 'activity' && event.type !== 'process') return
    const { runId, botId } = event
    setSources((current) => {
      const runs = current[source] ?? {}
      const previous = runs[runId]
      // 结束标记保留到流关闭，重复或迟到的摘要不能重新点亮头像。
      if (previous?.isFinished || (previous && previous.botId !== botId)) return current
      if (event.type === 'process' && (!previous?.isWorking || !event.text.trim())) return current
      if (event.type === 'activity' && typeof event.isWorking !== 'boolean') return current
      const isWorking = event.type === 'process' || (event.isWorking === true && !event.isFinished)
      const summary =
        event.type === 'process' ? event.text : isWorking ? (previous?.summary ?? '') : ''
      return {
        ...current,
        [source]: {
          ...runs,
          [runId]: { botId, isWorking, isFinished: event.isFinished === true, summary },
        },
      }
    })
  }, [])
  const clearActivity = useCallback((source: string) => {
    setSources((current) => {
      const next = { ...current }
      delete next[source]
      return next
    })
  }, [])
  const workStates = useMemo(() => {
    const working: WorkStates = {}
    for (const runs of Object.values(sources)) {
      for (const run of Object.values(runs)) {
        if (run.isWorking) working[run.botId] = { summary: run.summary }
      }
    }
    return working
  }, [sources])
  useEffect(() => {
    const ids = Object.keys(workStates)
    for (const id of new Set([...previousIds.current, ...ids])) {
      setAgentActivity(id, workStates[id] ? 'working' : 'idle')
    }
    previousIds.current = ids
  }, [workStates])
  useEffect(
    () => () => {
      for (const id of previousIds.current) setAgentActivity(id, 'idle')
    },
    [],
  )
  return { workStates, receiveActivity, clearActivity }
}
