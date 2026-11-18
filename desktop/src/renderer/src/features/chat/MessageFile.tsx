import { Download, Image as ImageIcon, LoaderCircle, Play, X } from 'lucide-react'
import { useEffect, useEffectEvent, useRef, useState } from 'react'
import { attachmentMediaType } from '../../../../shared/attachmentMedia'
import { Dialog } from '../../components/Dialog'
import { getBackend } from '../desktop/backend'
import type { MessageAttachment } from '../workspace/model'
import { AttachmentIcon } from './AttachmentIcon'
import { useChatImageCache } from './ChatImagePreviewProvider'

/** 已保存的附件由主进程按会话读取，渲染进程无需知道服务端地址或凭据。 */
export async function previewAttachment(file: MessageAttachment): Promise<string> {
  const preview = getBackend().previewAttachment
  if (!preview) throw new Error('文件预览服务不可用。')
  return preview(file.path)
}

export function attachmentCaption(content: string, files?: readonly MessageAttachment[]) {
  // 现有协议用文件名拼接作为无正文消息的摘要。保留摘要供侧栏和引用使用。
  return files?.length && content === files.map((item) => item.name).join('、') ? '' : content
}

export function MessageFile({
  attachment,
  onPreview,
  onDownload,
}: {
  attachment: MessageAttachment
  onPreview?: (file: MessageAttachment) => Promise<string>
  onDownload?: (file: MessageAttachment) => void
}) {
  const media = attachmentMediaType(attachment.name, attachment.mediaType)?.split('/')[0]
  const isImage = media === 'image'
  const canPreview = Boolean(onPreview && media)
  const { path, sha256 } = attachment
  const cache = useChatImageCache()
  const root = useRef<HTMLDivElement>(null)
  const [requested, setRequested] = useState(isImage && typeof IntersectionObserver === 'undefined')
  const [attempt, setAttempt] = useState(0)
  const [url, setUrl] = useState(() =>
    isImage && canPreview ? (cache?.get(attachment) ?? '') : '',
  )
  const [failed, setFailed] = useState(false)
  const [expanded, setExpanded] = useState(false)
  const read = useEffectEvent(async () => {
    const result = await onPreview?.(attachment)
    if (!result) throw new Error('文件缺少预览内容。')
    return result
  })
  useEffect(() => {
    if (!isImage || url || requested || !canPreview || !root.current) return
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) {
          setRequested(true)
          observer.disconnect()
        }
      },
      { rootMargin: '200px' },
    )
    observer.observe(root.current)
    return () => observer.disconnect()
  }, [isImage, url, requested, canPreview])
  useEffect(() => {
    if (!requested || !canPreview) return
    let active = true
    const source = isImage && cache ? cache.load({ path, sha256 }, () => read()) : read()
    void source.then(
      (source) => {
        if (active) setUrl(source)
      },
      () => {
        if (active) setFailed(true)
      },
    )
    return () => {
      active = false
    }
  }, [requested, canPreview, attempt, isImage, cache, path, sha256])
  function retry() {
    setUrl('')
    setFailed(false)
    setRequested(true)
    setAttempt((value) => value + 1)
  }
  const download = onDownload && (
    <button
      type="button"
      className="media-download"
      aria-label={`下载 ${attachment.name}`}
      title="下载文件"
      onClick={() => onDownload(attachment)}
    >
      <Download size={16} />
    </button>
  )
  if (!canPreview)
    return (
      <button
        type="button"
        className="message-document"
        disabled={!onDownload}
        aria-label={`下载 ${attachment.name}`}
        onClick={() => onDownload?.(attachment)}
      >
        <AttachmentIcon name={attachment.name} mediaType={attachment.mediaType} />
        <span>{attachment.name}</span>
      </button>
    )
  return (
    <div ref={root} className={isImage && !failed ? 'message-photo' : 'message-media'}>
      {failed ? (
        <>
          <span className="media-error" role="status">
            {attachment.name} 无法预览
          </span>
          <button type="button" className="text-button" onClick={retry}>
            重试预览
          </button>
          {download}
        </>
      ) : isImage ? (
        <>
          <button
            type="button"
            className="photo-open"
            aria-label={`查看原图 ${attachment.name}`}
            disabled={!url}
            onClick={() => setExpanded(true)}
          >
            {url ? (
              <img
                className="message-image"
                src={url}
                alt={attachment.name}
                loading="lazy"
                onError={() => {
                  cache?.discard(attachment, url)
                  setFailed(true)
                }}
              />
            ) : (
              <ImageIcon size={24} aria-label="正在读取图片" />
            )}
          </button>
          {expanded && (
            <Dialog variant="media" title={attachment.name} onClose={() => setExpanded(false)}>
              <header className="photo-heading">
                <span>{attachment.name}</span>
                {download}
                <button
                  type="button"
                  className="icon-button"
                  aria-label="关闭图片预览"
                  onClick={() => setExpanded(false)}
                >
                  <X size={18} />
                </button>
              </header>
              <img className="full-photo" src={url} alt={attachment.name} />
            </Dialog>
          )}
        </>
      ) : (
        <>
          <div className="media-heading">
            {url ? (
              <AttachmentIcon name={attachment.name} mediaType={attachment.mediaType} />
            ) : (
              <button
                type="button"
                className="media-play"
                aria-label={`播放 ${attachment.name}`}
                disabled={requested}
                onClick={() => setRequested(true)}
              >
                {requested ? <LoaderCircle size={18} aria-label="正在读取" /> : <Play size={18} />}
              </button>
            )}
            <span className="media-title">
              <strong>{attachment.name}</strong>
              <small>{media === 'audio' ? '音频' : '视频'}</small>
            </span>
            {download}
          </div>
          {url &&
            (media === 'audio' ? (
              <audio
                src={url}
                controls
                autoPlay
                aria-label={attachment.name}
                onError={() => setFailed(true)}
              />
            ) : (
              <video
                src={url}
                playsInline
                controls
                autoPlay
                aria-label={attachment.name}
                onError={() => setFailed(true)}
              />
            ))}
        </>
      )}
    </div>
  )
}
