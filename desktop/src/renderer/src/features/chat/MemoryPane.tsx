import { useEffect, useState, type FormEvent } from 'react'

export interface CatMemory {
  path: string
  title: string
  summary: string
  sha256: string
}

/** 查看和管理这只猫咪在服务端的记忆。正文由 agent 运行时读写同一批文件。 */
export function MemoryPane({
  agentId,
  onLoad,
  onCreate,
  onDelete,
}: {
  agentId: string
  onLoad: (agentId: string) => Promise<CatMemory[]>
  onCreate: (agentId: string, title: string, content: string) => Promise<void>
  onDelete: (agentId: string, memory: CatMemory) => Promise<void>
}) {
  const [items, setItems] = useState<CatMemory[]>([])
  const [title, setTitle] = useState('')
  const [content, setContent] = useState('')
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    void onLoad(agentId).then(
      (next) => {
        if (!cancelled) setItems(next)
      },
      (failure: Error) => {
        if (!cancelled) setError(failure.message)
      },
    )
    return () => {
      cancelled = true
    }
  }, [agentId, onLoad])

  async function submit(event: FormEvent): Promise<void> {
    event.preventDefault()
    try {
      await onCreate(agentId, title.trim(), content.trim())
      setTitle('')
      setContent('')
      setItems(await onLoad(agentId))
      setError('')
    } catch (failure) {
      setError((failure as Error).message)
    }
  }

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
            <p>{item.summary}</p>
            <button
              type="button"
              onClick={() => {
                void onDelete(agentId, item)
                  .then(() => onLoad(agentId))
                  .then(setItems)
                  .catch((failure: Error) => setError(failure.message))
              }}
            >
              删除
            </button>
          </li>
        ))}
      </ul>
      <form onSubmit={(event) => void submit(event)}>
        <label>
          标题
          <input
            value={title}
            maxLength={80}
            onChange={(event) => setTitle(event.target.value)}
            required
          />
        </label>
        <label>
          内容
          <textarea
            value={content}
            maxLength={4000}
            rows={3}
            onChange={(event) => setContent(event.target.value)}
            required
          />
        </label>
        <button type="submit">记下</button>
      </form>
      {error ? <p role="alert">{error}</p> : null}
    </div>
  )
}
