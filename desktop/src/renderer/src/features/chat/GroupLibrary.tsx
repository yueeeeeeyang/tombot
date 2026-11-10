import { useEffect, useState } from 'react'
import { getBackend } from '../desktop/backend'
import type { Artifact } from '../workspace/model'
import { ArtifactLibrary } from './ArtifactLibrary'

/** 群资料按服务端消息游标分页，失败保留已加载文件并提供重试。 */
export function GroupLibrary({ groupId }: { groupId: string }) {
  const [artifacts, setArtifacts] = useState<Artifact[]>([])
  const [cursor, setCursor] = useState(0)
  const [next, setNext] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [attempt, setAttempt] = useState(0)
  useEffect(() => {
    let cancelled = false
    void getBackend()
      .listGroupArtifacts(groupId, cursor)
      .then((page) => {
        if (cancelled) return
        setArtifacts((current) =>
          cursor === 0
            ? page.artifacts
            : [
                ...current,
                ...page.artifacts.filter((item) => !current.some((old) => old.id === item.id)),
              ],
        )
        setNext(page.next)
        setError('')
      })
      .catch((failure: Error) => {
        if (!cancelled) setError(failure.message)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [groupId, cursor, attempt])
  return (
    <>
      {artifacts.length > 0 || (!loading && !error) ? (
        <ArtifactLibrary
          artifacts={artifacts}
          emptyText="资料库还是空的。群里分享的文件会出现在这里。"
        />
      ) : null}
      {loading && <p role="status">正在读取资料库…</p>}
      {error && (
        <p role="alert" className="form-error">
          {error}
        </p>
      )}
      {(error || next > 0) && (
        <button
          type="button"
          className="secondary-button"
          disabled={loading}
          onClick={() => {
            setLoading(true)
            if (error) setAttempt((value) => value + 1)
            else setCursor(next)
          }}
        >
          {error ? '重试' : '加载更多'}
        </button>
      )}
    </>
  )
}
