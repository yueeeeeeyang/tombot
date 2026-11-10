import { useEffect, useState, type FormEvent } from 'react'

export interface CatRoutine {
  id: string
  name: string
  cron: string
  prompt: string
  is_enabled: boolean
  revision: number
}

/** 管理这只猫咪的定时任务。到点由服务端 agent 执行，结果写回对话。 */
export function RoutinePane({
  agentId,
  onLoad,
  onCreate,
  onToggle,
  onDelete,
}: {
  agentId: string
  onLoad: (agentId: string) => Promise<CatRoutine[]>
  onCreate: (agentId: string, name: string, prompt: string) => Promise<void>
  onToggle: (routine: CatRoutine) => Promise<void>
  onDelete: (routine: CatRoutine) => Promise<void>
}) {
  const [items, setItems] = useState<CatRoutine[]>([])
  const [name, setName] = useState('')
  const [prompt, setPrompt] = useState('')
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

  async function refresh(): Promise<void> {
    setItems(await onLoad(agentId))
  }

  async function submit(event: FormEvent): Promise<void> {
    event.preventDefault()
    try {
      await onCreate(agentId, name.trim(), prompt.trim())
      setName('')
      setPrompt('')
      await refresh()
      setError('')
    } catch (failure) {
      setError((failure as Error).message)
    }
  }

  return (
    <div
      className="cat-panel-body"
      role="tabpanel"
      id="cat-panel-routine"
      aria-labelledby="cat-tab-routine"
    >
      {items.length === 0 ? <p role="status">还没有定时任务</p> : null}
      <ul className="routine-list">
        {items.map((item) => (
          <li key={item.id}>
            <strong>{item.name}</strong>
            <p>{item.is_enabled ? '每天 8:00' : '已暂停'}</p>
            <button
              type="button"
              onClick={() =>
                void onToggle(item)
                  .then(refresh)
                  .catch((failure: Error) => setError(failure.message))
              }
            >
              {item.is_enabled ? '暂停' : '恢复'}
            </button>
            <button
              type="button"
              onClick={() =>
                void onDelete(item)
                  .then(refresh)
                  .catch((failure: Error) => setError(failure.message))
              }
            >
              删除
            </button>
          </li>
        ))}
      </ul>
      <form onSubmit={(event) => void submit(event)}>
        <label>
          名称
          <input
            value={name}
            maxLength={80}
            onChange={(event) => setName(event.target.value)}
            required
          />
        </label>
        <label>
          到点要做的事
          <textarea
            value={prompt}
            maxLength={2000}
            rows={3}
            onChange={(event) => setPrompt(event.target.value)}
            required
          />
        </label>
        <button type="submit">添加定时任务</button>
      </form>
      {error ? <p role="alert">{error}</p> : null}
    </div>
  )
}
