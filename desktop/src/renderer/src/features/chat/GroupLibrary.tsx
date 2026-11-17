import { useCallback, useEffect, useRef, useState } from 'react'
import { Download, FileText, Image, Pencil, RefreshCw, Search, Trash2, Upload } from 'lucide-react'
import { Dialog } from '../../components/Dialog'
import { getBackend } from '../desktop/backend'
import {
  GROUP_FILE_MAX_BYTES,
  type GroupFile,
  type GroupFilePage,
  type GroupFilePreview,
} from '../../../../shared/groupFiles'
import { ulid } from '../../../../shared/ulid'

function sizeLabel(bytes: number): string {
  return bytes < 1024
    ? `${bytes} B`
    : bytes < 1024 * 1024
      ? `${(bytes / 1024).toFixed(1)} KB`
      : `${(bytes / 1024 / 1024).toFixed(1)} MB`
}
function errorText(error: unknown): string {
  return error instanceof Error ? error.message : '文件操作失败，请重试。'
}

/** 切换群组时销毁查询与弹窗状态，旧请求不能污染新群。 */
export function GroupLibrary({ groupId }: { groupId: string }) {
  return <Library key={groupId} groupId={groupId} />
}
function Library({ groupId }: { groupId: string }) {
  const [files, setFiles] = useState<GroupFile[]>([])
  const [page, setPage] = useState<GroupFilePage>()
  const [query, setQuery] = useState('')
  const [search, setSearch] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [actionError, setActionError] = useState('')
  const [notice, setNotice] = useState('')
  const [refresh, setRefresh] = useState(0)
  const [failedCursor, setFailedCursor] = useState('')
  const [uploading, setUploading] = useState(false)
  const [remaining, setRemaining] = useState<Array<{ file: File; id: string }>>([])
  const [operation, setOperation] = useState<{
    kind: 'preview' | 'rename' | 'delete'
    file: GroupFile
  }>()
  const sequence = useRef({ value: 0 })
  const input = useRef<HTMLInputElement>(null)
  const uploadLock = useRef(false)
  const load = useCallback(
    async (before = '') => {
      const current = ++sequence.current.value
      setLoading(true)
      try {
        const result = await getBackend().listGroupFiles(groupId, before, query)
        if (current !== sequence.current.value) return
        setFiles((previous) =>
          before
            ? [
                ...previous,
                ...result.files.filter((file) => !previous.some((old) => old.id === file.id)),
              ]
            : result.files,
        )
        setPage(result)
        setError('')
        setFailedCursor('')
      } catch (failure) {
        if (current === sequence.current.value) {
          setError(errorText(failure))
          setFailedCursor(before)
        }
      } finally {
        if (current === sequence.current.value) setLoading(false)
      }
    },
    [groupId, query],
  )
  useEffect(() => {
    const requests = sequence.current
    const timer = setTimeout(() => void load(), 150)
    return () => {
      clearTimeout(timer)
      requests.value++
    }
  }, [load, refresh])
  useEffect(() => {
    const reload = () => setRefresh((value) => value + 1)
    const stop = getBackend().watchEvents?.(
      (event) => {
        if (
          event.kind === 'workspace.refresh' ||
          (event.kind === 'group.library.changed' && event.payload.group_id === groupId)
        )
          reload()
      },
      (failure) => setError(errorText(failure)),
    )
    window.addEventListener('focus', reload)
    return () => {
      stop?.()
      window.removeEventListener('focus', reload)
    }
  }, [groupId])
  async function upload(entries: Array<{ file: File; id: string }>) {
    if (uploadLock.current) return
    uploadLock.current = true
    setUploading(true)
    setActionError('')
    setNotice('')
    let uploaded = 0
    try {
      for (const entry of entries) {
        setNotice(`正在上传 ${uploaded + 1}/${entries.length}：${entry.file.name}`)
        await getBackend().uploadGroupFile(
          groupId,
          { name: entry.file.name, bytes: new Uint8Array(await entry.file.arrayBuffer()) },
          entry.id,
        )
        uploaded++
      }
      setRemaining([])
      setNotice(`已上传 ${uploaded} 个文件`)
    } catch (failure) {
      setRemaining(entries.slice(uploaded))
      setActionError(errorText(failure))
      setNotice(uploaded ? `已上传 ${uploaded} 个文件，其余文件可重试。` : '')
    } finally {
      uploadLock.current = false
      setUploading(false)
      setRefresh((value) => value + 1)
    }
  }
  async function download(file: GroupFile) {
    try {
      if (await getBackend().downloadGroupFile(groupId, file.id)) setNotice(`已保存 ${file.name}`)
    } catch (failure) {
      setActionError(errorText(failure))
    }
  }
  return (
    <section className="group-library" aria-label="群组资料库">
      <div className="group-library-toolbar">
        <button
          className="primary-button"
          disabled={uploading}
          onClick={() => input.current?.click()}
        >
          <Upload size={15} />
          上传文件
        </button>
        <button
          className="icon-button"
          aria-label="刷新资料库"
          disabled={loading}
          onClick={() => setRefresh((value) => value + 1)}
        >
          <RefreshCw size={16} />
        </button>
        <input
          ref={input}
          aria-label="选择群文件"
          type="file"
          multiple
          hidden
          disabled={uploading}
          onChange={(event) => {
            const selected = Array.from(event.target.files ?? [])
            event.target.value = ''
            if (!selected.length) return
            if (selected.length > 20 || selected.some((file) => file.size > GROUP_FILE_MAX_BYTES)) {
              setActionError('每次最多上传 20 个文件，单个文件最多 16 MB。')
              return
            }
            setRemaining([])
            void upload(selected.map((file) => ({ file, id: ulid() })))
          }}
        />
      </div>
      <form
        className="group-library-search"
        role="search"
        onSubmit={(event) => {
          event.preventDefault()
          setLoading(true)
          setError('')
          setFiles([])
          setPage(undefined)
          setQuery(search.trim())
          setRefresh((value) => value + 1)
        }}
      >
        <input
          aria-label="搜索群文件"
          placeholder="搜索文件名称"
          maxLength={240}
          value={search}
          onChange={(event) => setSearch(event.target.value)}
        />
        <button className="icon-button" aria-label="搜索文件" type="submit">
          <Search size={16} />
        </button>
      </form>
      <p className="group-library-help">群里分享的文件会自动收录。单个文件最多 16 MB。</p>
      {page && (
        <p className="group-library-help">
          全部群组已用 {sizeLabel(page.used_bytes)} / {sizeLabel(page.max_bytes)}
        </p>
      )}
      {notice && (
        <p role="status" className="group-library-help">
          {notice}
        </p>
      )}
      {error && (
        <p role="alert" className="form-error">
          {error}
        </p>
      )}
      {actionError && (
        <p role="alert" className="form-error">
          {actionError}
        </p>
      )}
      {remaining.length > 0 && (
        <button
          className="secondary-button"
          disabled={uploading}
          onClick={() => void upload(remaining)}
        >
          重试上传（{remaining.length}）
        </button>
      )}
      {loading && <p role="status">正在读取资料库…</p>}
      {!loading && !error && files.length === 0 && (
        <p role="status" className="cat-panel-empty">
          {query ? '没有找到匹配的文件。' : '资料库还是空的。上传文件，或在群聊中分享附件。'}
        </p>
      )}
      <ul className="group-library-files">
        {files.map((file) => (
          <li key={file.id}>
            <button
              className="group-library-file"
              onClick={() => setOperation({ kind: 'preview', file })}
              aria-label={`预览 ${file.name}`}
            >
              {file.media_type.startsWith('image/') ? <Image size={22} /> : <FileText size={22} />}
              <span>
                <strong title={file.name}>{file.name}</strong>
                <small>
                  {sizeLabel(file.size)} · {new Date(file.created_at).toLocaleDateString('zh-CN')}
                </small>
              </span>
            </button>
            <div className="group-library-actions">
              <button
                className="icon-button"
                aria-label={`下载 ${file.name}`}
                onClick={() => void download(file)}
              >
                <Download size={14} />
              </button>
              <button
                className="icon-button"
                aria-label={`重命名 ${file.name}`}
                onClick={() => setOperation({ kind: 'rename', file })}
              >
                <Pencil size={14} />
              </button>
              <button
                className="icon-button"
                aria-label={`删除 ${file.name}`}
                onClick={() => setOperation({ kind: 'delete', file })}
              >
                <Trash2 size={14} />
              </button>
            </div>
          </li>
        ))}
      </ul>
      {(error || page?.next) && (
        <button
          className="secondary-button"
          disabled={loading}
          onClick={() => void load(error ? failedCursor : page?.next)}
        >
          {error ? '重试' : '加载更多'}
        </button>
      )}
      {operation && (
        <FileDialog
          key={`${operation.kind}:${operation.file.id}`}
          groupId={groupId}
          operation={operation}
          onClose={() => setOperation(undefined)}
          onChanged={() => {
            setOperation(undefined)
            setRefresh((value) => value + 1)
          }}
        />
      )}
    </section>
  )
}
function FileDialog({
  groupId,
  operation,
  onClose,
  onChanged,
}: {
  groupId: string
  operation: { kind: 'preview' | 'rename' | 'delete'; file: GroupFile }
  onClose: () => void
  onChanged: () => void
}) {
  const { file, kind } = operation
  const [name, setName] = useState(file.name)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [preview, setPreview] = useState<GroupFilePreview>()
  const [imageFailed, setImageFailed] = useState(false)
  const [attempt, setAttempt] = useState(0)
  const [notice, setNotice] = useState('')
  useEffect(() => {
    if (kind !== 'preview') return
    let cancelled = false
    void getBackend()
      .previewGroupFile(groupId, file.id)
      .then((result) => {
        if (!cancelled) {
          setPreview(result)
          setError('')
        }
      })
      .catch((failure) => {
        if (!cancelled) setError(errorText(failure))
      })
    return () => {
      cancelled = true
    }
  }, [groupId, file.id, kind, attempt])
  async function save() {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      if (kind === 'rename')
        await getBackend().renameGroupFile(groupId, file.id, file.revision, name.trim())
      else await getBackend().deleteGroupFile(groupId, file.id, file.revision)
      onChanged()
    } catch (failure) {
      setError(errorText(failure))
    } finally {
      setBusy(false)
    }
  }
  async function download() {
    setBusy(true)
    setError('')
    try {
      if (await getBackend().downloadGroupFile(groupId, file.id)) setNotice('文件已保存。')
    } catch (failure) {
      setError(errorText(failure))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      title={kind === 'preview' ? '文件预览' : kind === 'rename' ? '重命名文件' : '删除文件'}
      onClose={onClose}
      isDismissible={!busy}
    >
      <p className="group-file-name">{preview?.file.name ?? file.name}</p>
      {kind === 'preview' ? (
        <>
          {!preview && !error && <p role="status">正在读取文件…</p>}
          {preview?.image && !imageFailed && (
            <img
              className="group-file-preview-image"
              src={preview.image}
              alt={preview.file.name}
              onError={() => {
                setImageFailed(true)
                setError('图片内容无法预览，请下载后检查文件。')
              }}
            />
          )}
          {preview?.text !== undefined && (
            <pre className="group-file-preview-text">{preview.text || '（空文件）'}</pre>
          )}
          {preview?.truncated && <p>仅预览前 64 KB，请下载查看完整内容。</p>}
          {preview && preview.text === undefined && !preview.image && (
            <p>此格式暂不支持预览，可以下载后打开。</p>
          )}
          {error && !preview && (
            <button
              className="secondary-button"
              onClick={() => {
                setError('')
                setAttempt((value) => value + 1)
              }}
            >
              重试预览
            </button>
          )}
          <button className="primary-button" disabled={busy} onClick={() => void download()}>
            <Download size={16} />
            下载文件
          </button>
        </>
      ) : (
        <form
          onSubmit={(event) => {
            event.preventDefault()
            void save()
          }}
        >
          {kind === 'rename' ? (
            <label className="form-field">
              文件名称
              <input
                aria-label="文件名称"
                value={name}
                maxLength={240}
                required
                onChange={(event) => setName(event.target.value)}
              />
            </label>
          ) : (
            <p>删除后将无法下载或预览，历史聊天中的文件记录仍会保留。</p>
          )}
          <div className="dialog-actions">
            <button className="secondary-button" type="button" disabled={busy} onClick={onClose}>
              取消
            </button>
            <button
              className="primary-button"
              type="submit"
              disabled={busy || (kind === 'rename' && !name.trim())}
            >
              {busy ? '正在保存…' : kind === 'rename' ? '保存名称' : '确认删除'}
            </button>
          </div>
        </form>
      )}
      {notice && <p role="status">{notice}</p>}
      {error && (
        <p role="alert" className="form-error">
          {error}
        </p>
      )}
    </Dialog>
  )
}
