import { useCallback, useEffect, useRef, useState } from 'react'
import { Download, Search } from 'lucide-react'
import { LibraryFileCard } from './LibraryFileCard'
import { Dialog } from '../../components/Dialog'
import { getBackend } from '../desktop/backend'
import {
  type ChatFile,
  type ChatFilePage,
  type ChatFilePreview,
} from '../../../../shared/chatFiles'

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

/** 切换会话时销毁查询与弹窗状态，旧请求不能污染新会话。 */
export function ChatLibrary({ conversationId }: { conversationId: string }) {
  return <Library key={conversationId} conversationId={conversationId} />
}
function Library({ conversationId }: { conversationId: string }) {
  const [files, setFiles] = useState<ChatFile[]>([])
  const [page, setPage] = useState<ChatFilePage>()
  const [query, setQuery] = useState('')
  const [search, setSearch] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [actionError, setActionError] = useState('')
  const [notice, setNotice] = useState('')
  const [refresh, setRefresh] = useState(0)
  const [failedCursor, setFailedCursor] = useState('')
  const [operation, setOperation] = useState<{
    kind: 'preview' | 'rename' | 'delete'
    file: ChatFile
  }>()
  const sequence = useRef({ value: 0 })
  const load = useCallback(
    async (before = '') => {
      const current = ++sequence.current.value
      setLoading(true)
      try {
        if (!conversationId) throw new Error('尚未获取聊天会话，请刷新工作台后重试。')
        const result = await getBackend().listChatFiles(conversationId, before, query)
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
    [conversationId, query],
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
          (event.kind === 'chat.library.changed' &&
            event.payload.conversation_id === conversationId)
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
  }, [conversationId])
  async function download(file: ChatFile) {
    try {
      if (await getBackend().downloadChatFile(conversationId, file.id))
        setNotice(`已保存 ${file.name}`)
    } catch (failure) {
      setActionError(errorText(failure))
    }
  }
  return (
    <section className="chat-library" aria-label="聊天资料库">
      <form
        className="chat-library-search"
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
          aria-label="搜索聊天文件"
          placeholder="搜索文件名称"
          maxLength={240}
          value={search}
          onChange={(event) => setSearch(event.target.value)}
        />
        <button className="icon-button" aria-label="搜索文件" type="submit">
          <Search size={16} />
        </button>
      </form>
      <p className="chat-library-help">聊天中你和猫咪发出的文件会自动收录。</p>
      {page && (
        <p className="chat-library-help">
          已用 {sizeLabel(page.used_bytes)} / {sizeLabel(page.max_bytes)}
        </p>
      )}
      {notice && (
        <p role="status" className="chat-library-help">
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
      {loading && <p role="status">正在读取资料库…</p>}
      {!loading && !error && files.length === 0 && (
        <p role="status" className="cat-panel-empty">
          {query ? '没有找到匹配的文件。' : '资料库还是空的。在聊天中发送文件后会显示在这里。'}
        </p>
      )}
      <ul className="chat-library-files">
        {files.map((file) => (
          <LibraryFileCard
            key={file.id}
            file={file}
            size={sizeLabel(file.size)}
            onAction={(kind) => {
              if (kind === 'download') void download(file)
              else setOperation({ kind, file })
            }}
          />
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
          conversationId={conversationId}
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
  conversationId,
  operation,
  onClose,
  onChanged,
}: {
  conversationId: string
  operation: { kind: 'preview' | 'rename' | 'delete'; file: ChatFile }
  onClose: () => void
  onChanged: () => void
}) {
  const { file, kind } = operation
  const [name, setName] = useState(file.name)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [preview, setPreview] = useState<ChatFilePreview>()
  const [imageFailed, setImageFailed] = useState(false)
  const [attempt, setAttempt] = useState(0)
  const [notice, setNotice] = useState('')
  useEffect(() => {
    if (kind !== 'preview') return
    let cancelled = false
    void getBackend()
      .previewChatFile(conversationId, file.id)
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
  }, [conversationId, file.id, kind, attempt])
  async function save() {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      if (kind === 'rename')
        await getBackend().renameChatFile(conversationId, file.id, file.revision, name.trim())
      else await getBackend().deleteChatFile(conversationId, file.id, file.revision)
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
      if (await getBackend().downloadChatFile(conversationId, file.id)) setNotice('文件已保存。')
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
      <p className="chat-file-name">{preview?.file.name ?? file.name}</p>
      {kind === 'preview' ? (
        <>
          {!preview && !error && <p role="status">正在读取文件…</p>}
          {preview?.image && !imageFailed && (
            <img
              className="chat-file-preview-image"
              src={preview.image}
              alt={preview.file.name}
              onError={() => {
                setImageFailed(true)
                setError('图片内容无法预览，请下载后检查文件。')
              }}
            />
          )}
          {preview?.text !== undefined && (
            <pre className="chat-file-preview-text">{preview.text || '（空文件）'}</pre>
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
