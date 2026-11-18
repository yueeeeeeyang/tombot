import type { MessageAttachment } from '../workspace/model'

type ImageAttachment = Pick<MessageAttachment, 'path' | 'sha256'>
const MAX_PREVIEW_BYTES = 64 * 1024 * 1024
const MAX_PREVIEW_ENTRIES = 128

function imageKey({ path, sha256 }: ImageAttachment): string {
  return `${path}:${sha256}`
}

/** 由登录工作区持有，按成功读取的顺序淘汰图片，限制切换会话后的额外内存占用。 */
export class ChatImageCache {
  private readonly previews = new Map<string, { path: string; source: string }>()
  private readonly pending = new Map<string, { path: string; request: Promise<string> }>()
  private bytes = 0

  constructor(
    private readonly maxBytes = MAX_PREVIEW_BYTES,
    private readonly maxEntries = MAX_PREVIEW_ENTRIES,
  ) {}

  /** 同步返回该路径和摘要的现有预览，使重新挂载的图片首帧就能使用它。 */
  get(attachment: ImageAttachment): string | undefined {
    return this.previews.get(imageKey(attachment))?.source
  }

  /** 合并同一图片正在进行的读取，失败沿用读取错误并允许下次重试。 */
  load(attachment: ImageAttachment, read: () => Promise<string>): Promise<string> {
    const key = imageKey(attachment)
    const cached = this.previews.get(key)
    if (cached) return Promise.resolve(cached.source)
    const pending = this.pending.get(key)
    if (pending) return pending.request

    const { path } = attachment
    const request = Promise.resolve()
      .then(read)
      .then((source) => {
        // 文件变更后，迟到的结果不得覆盖同一资源的新请求或重新进入缓存。
        if (this.pending.get(key)?.request === request) this.store(key, path, source)
        return source
      })
      .finally(() => {
        if (this.pending.get(key)?.request === request) this.pending.delete(key)
      })
    this.pending.set(key, { path, request })
    return request
  }

  /** 失效该文件所有摘要的缓存，并阻止正在读取的旧请求写回。 */
  invalidate(path: string): void {
    for (const [key, preview] of this.previews) {
      if (preview.path === path) this.remove(key)
    }
    for (const [key, pending] of this.pending) {
      if (pending.path === path) this.pending.delete(key)
    }
  }

  /** 浏览器解码失败时，仅移除本次失败的预览，避免误删后续更新。 */
  discard(attachment: ImageAttachment, source: string): void {
    const key = imageKey(attachment)
    if (this.previews.get(key)?.source === source) this.remove(key)
  }

  /** 工作区退出或事件流中断时释放缓存，阻止未结束的读取写回。 */
  clear(): void {
    this.previews.clear()
    this.pending.clear()
    this.bytes = 0
  }

  private store(key: string, path: string, source: string): void {
    // 预览地址是 Base64 字符串，按每个字符两字节保守计算缓存预算。
    const bytes = source.length * 2
    if (!bytes || bytes > this.maxBytes || this.maxEntries < 1) return
    for (const oldest of this.previews.keys()) {
      if (this.bytes + bytes <= this.maxBytes && this.previews.size < this.maxEntries) break
      this.remove(oldest)
    }
    this.previews.set(key, { path, source })
    this.bytes += bytes
  }

  private remove(key: string): void {
    const preview = this.previews.get(key)
    if (!preview) return
    this.bytes -= preview.source.length * 2
    this.previews.delete(key)
  }
}
