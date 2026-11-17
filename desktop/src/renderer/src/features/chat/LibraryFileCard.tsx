import { useId, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import {
  Download,
  Ellipsis,
  Eye,
  File,
  FileArchive,
  FileCode,
  FileImage,
  FileMusic,
  FileSpreadsheet,
  FileText,
  FileVideo,
  Pencil,
  Trash2,
} from 'lucide-react'
import type { ChatFile } from '../../../../shared/chatFiles'

const fileIcons = {
  image: FileImage,
  audio: FileMusic,
  video: FileVideo,
  archive: FileArchive,
  spreadsheet: FileSpreadsheet,
  code: FileCode,
  text: FileText,
  file: File,
}

/** 文件按类型共用 Lucide 线性图标，颜色随主题变化，不依赖远程资源。 */
export function fileIconKind(file: Pick<ChatFile, 'name' | 'media_type'>) {
  const extension = file.name.split('.').at(-1)?.toLowerCase() ?? ''
  if (file.media_type.startsWith('image/')) return 'image' as const
  if (file.media_type.startsWith('audio/')) return 'audio' as const
  if (file.media_type.startsWith('video/')) return 'video' as const
  if (['zip', 'jar', 'tar', 'gz', '7z', 'rar'].includes(extension)) return 'archive' as const
  if (['csv', 'tsv', 'xls', 'xlsx', 'ods'].includes(extension)) return 'spreadsheet' as const
  if (
    ['js', 'ts', 'tsx', 'jsx', 'py', 'go', 'java', 'json', 'html', 'css', 'sql', 'sh'].includes(
      extension,
    )
  )
    return 'code' as const
  if (file.media_type.startsWith('text/') || ['pdf', 'doc', 'docx', 'md'].includes(extension))
    return 'text' as const
  return 'file' as const
}

/** 悬浮资料与操作菜单使用独立浮层，避免被资料栏的滚动区域裁切。 */
export function LibraryFileCard({
  file,
  size,
  onAction,
}: {
  file: ChatFile
  size: string
  onAction: (kind: 'preview' | 'download' | 'rename' | 'delete') => void
}) {
  const row = useRef<HTMLLIElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)
  const popup = useRef<HTMLDivElement>(null)
  const [hovered, setHovered] = useState(false)
  const [isMenuOpen, setMenuOpen] = useState(false)
  const tooltipId = useId()
  const Icon = fileIcons[fileIconKind(file)]
  const [now, setNow] = useState(() => Date.now())
  const days = Math.max(0, Math.floor((now - Date.parse(file.created_at)) / 86400000))
  const age = days === 0 ? '今天' : `${days} 天`
  useLayoutEffect(() => {
    const element = popup.current
    const anchor = isMenuOpen ? trigger.current : row.current
    if (!element || !anchor) return
    const position = () => {
      const bounds = anchor.getBoundingClientRect()
      const box = element.getBoundingClientRect()
      const preferredTop = isMenuOpen ? bounds.bottom + 6 : bounds.top - box.height - 8
      element.style.left = `${Math.max(8, Math.min(isMenuOpen ? bounds.right - box.width : bounds.left, innerWidth - box.width - 8))}px`
      element.style.top = `${Math.max(8, Math.min(preferredTop < 8 ? bounds.bottom + 6 : preferredTop, innerHeight - box.height - 8))}px`
    }
    position()
    if (isMenuOpen) element.querySelector('button')?.focus({ preventScroll: true })
    const dismiss = (event: PointerEvent) => {
      if (
        event.target instanceof Node &&
        !element.contains(event.target) &&
        !row.current?.contains(event.target)
      )
        setMenuOpen(false)
    }
    window.addEventListener('resize', position)
    document.addEventListener('scroll', position, true)
    document.addEventListener('pointerdown', dismiss, true)
    return () => {
      window.removeEventListener('resize', position)
      document.removeEventListener('scroll', position, true)
      document.removeEventListener('pointerdown', dismiss, true)
    }
  }, [hovered, isMenuOpen])
  return (
    <li
      ref={row}
      className={`library-file-card${isMenuOpen ? ' is-menu-open' : ''}`}
      onMouseEnter={() => {
        setNow(Date.now())
        setHovered(true)
      }}
      onMouseLeave={() => setHovered(false)}
      onFocus={() => setHovered(true)}
      onBlur={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget)) setHovered(false)
      }}
    >
      <button
        type="button"
        className="chat-library-file"
        aria-label={`预览 ${file.name}`}
        aria-describedby={hovered && !isMenuOpen ? tooltipId : undefined}
        onClick={() => onAction('preview')}
      >
        <span className="library-file-icon">
          <Icon size={24} strokeWidth={1.5} aria-hidden="true" />
        </span>
        <strong>{file.name}</strong>
      </button>
      <span className="library-file-age" aria-hidden="true">
        {age}
      </span>
      <button
        ref={trigger}
        type="button"
        className="icon-button library-file-more"
        aria-label={`更多操作 ${file.name}`}
        aria-haspopup="menu"
        aria-expanded={isMenuOpen}
        onClick={() => setMenuOpen((value) => !value)}
      >
        <Ellipsis size={18} />
      </button>
      {(hovered || isMenuOpen) &&
        createPortal(
          isMenuOpen ? (
            <div
              ref={popup}
              className="library-file-menu"
              role="menu"
              aria-label={`${file.name}的操作`}
              onKeyDown={(event) => {
                const buttons = [...event.currentTarget.querySelectorAll('button')]
                const index = buttons.indexOf(document.activeElement as HTMLButtonElement)
                if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
                  event.preventDefault()
                  const next =
                    event.key === 'Home'
                      ? 0
                      : event.key === 'End'
                        ? buttons.length - 1
                        : (index + (event.key === 'ArrowDown' ? 1 : buttons.length - 1)) %
                          buttons.length
                  buttons[next]?.focus()
                }
                if (event.key === 'Escape' || event.key === 'Tab') {
                  if (event.key === 'Escape') event.preventDefault()
                  setMenuOpen(false)
                  trigger.current?.focus({ preventScroll: true })
                }
              }}
            >
              {(
                [
                  { kind: 'preview', label: '预览', icon: Eye },
                  { kind: 'download', label: '下载', icon: Download },
                  { kind: 'rename', label: '重命名', icon: Pencil },
                  { kind: 'delete', label: '删除', icon: Trash2 },
                ] as const
              ).map(({ kind, label, icon: ActionIcon }) => (
                <button
                  key={kind}
                  type="button"
                  role="menuitem"
                  aria-label={`${label} ${file.name}`}
                  onClick={() => {
                    setMenuOpen(false)
                    onAction(kind)
                  }}
                >
                  <ActionIcon size={15} />
                  {label}
                </button>
              ))}
            </div>
          ) : (
            <div ref={popup} id={tooltipId} className="library-file-tooltip" role="tooltip">
              <span>文件</span>
              <strong>{file.name}</strong>
              <small>
                {size} ·{' '}
                {new Date(file.created_at).toLocaleString('zh-CN', {
                  month: 'long',
                  day: 'numeric',
                  weekday: 'short',
                  hour: '2-digit',
                  minute: '2-digit',
                })}
              </small>
            </div>
          ),
          document.body,
        )}
    </li>
  )
}
