import { useEffect, useState } from 'react'
import { getBackend } from '../desktop/backend'

const INTRODUCTION_REFRESH_MS = 30_000

/** 介绍只读取独立展示投影；不修改工作空间，也不把文字作为下一轮聊天内容。 */
export function CatIntroduction({ agentId, revision }: { agentId: string; revision: number }) {
  const [text, setText] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [attempt, setAttempt] = useState(0)
  useEffect(() => {
    let cancelled = false
    let timer: ReturnType<typeof setTimeout>
    async function load() {
      try {
        const result = await getBackend().loadIntroduction(agentId)
        if (cancelled) return
        setText(result.text)
        setError('')
        timer = setTimeout(() => void load(), INTRODUCTION_REFRESH_MS)
      } catch (failure) {
        if (!cancelled) setError((failure as Error).message)
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    void load()
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [agentId, revision, attempt])
  return (
    <>
      <strong>{text || (loading ? '正在整理介绍…' : '相处久一点，会更了解彼此。')}</strong>
      {error && (
        <div className="introduction-error">
          <p role="alert">介绍暂时没能更新：{error}</p>
          <button
            type="button"
            className="text-button"
            onClick={() => setAttempt((value) => value + 1)}
          >
            重试
          </button>
        </div>
      )}
    </>
  )
}
