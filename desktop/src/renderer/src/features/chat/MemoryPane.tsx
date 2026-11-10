import { useEffect, useState } from 'react'

export interface CatMemory {
  path: string
  title: string
  summary: string
  sha256: string
}

/** 只查看这只猫咪的记忆。增删改由服务端 agent 在对话里用记忆工具完成。 */
export function MemoryPane({
  agentId,
  onLoad,
}: {
  agentId: string
  onLoad: (agentId: string) => Promise<CatMemory[]>
}) {
  const [items, setItems] = useState<CatMemory[]>([])
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    void onLoad(agentId).then(
      (next) => {
        if (!cancelled) setItems(Array.isArray(next) ? next : [])
      },
      (failure: Error) => {
        if (!cancelled) setError(failure.message)
      },
    )
    return () => {
      cancelled = true
    }
  }, [agentId, onLoad])

  return (
    <div
      className="cat-panel-body"
      role="tabpanel"
      id="cat-panel-memory"
      aria-labelledby="cat-tab-memory"
    >
      {items.length === 0 ? <p role="status">还没有记忆</p> : null}
      <ul className="memory-list">
        {items.map((item) => (
          <li key={item.path}>
            <strong>{item.title || item.path}</strong>
            {item.summary ? <p>{item.summary}</p> : null}
          </li>
        ))}
      </ul>
      {error ? <p role="alert">{error}</p> : null}
    </div>
  )
}
