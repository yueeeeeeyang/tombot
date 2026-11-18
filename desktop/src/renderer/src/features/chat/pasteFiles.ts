import type { ClipboardEvent } from 'react'
import { getBackend } from '../desktop/backend'

/** 图片和文件先加入本地草稿；Finder 的文件引用通过受限剪贴板接口读取。 */
export function pasteFiles(
  event: ClipboardEvent,
  stageFiles: (files: File[]) => Promise<void>,
  onError: (message: string) => void,
): void {
  const files = [...event.clipboardData.files]
  if (!files.length) {
    for (const item of Array.from(event.clipboardData.items ?? [])) {
      const file = item.kind === 'file' ? item.getAsFile() : null
      if (file) files.push(file)
    }
  }
  if (files.length) {
    event.preventDefault()
    void stageFiles(files).catch((failure: Error) => onError(failure.message))
    return
  }
  const types = event.clipboardData.types
  if (
    types.includes('Files') ||
    event.clipboardData.getData('text/uri-list').startsWith('file:') ||
    types.length === 0
  )
    event.preventDefault()
  const read = getBackend().readClipboardFiles
  if (!read) return
  void read()
    .then((items) => {
      if (items.length)
        return stageFiles(
          items.map(
            (item) => new File([new Uint8Array(item.bytes)], item.name, { type: item.type }),
          ),
        )
    })
    .catch((failure: Error) => onError(failure.message))
}
