import { useLayoutEffect, useRef, useState, type KeyboardEvent } from 'react'

/** 原位名称输入：挂载时全选原文，Enter/失焦提交，Esc 取消，失败时保留编辑。 */
export function InlineNameInput({
  initialName,
  label,
  onCommit,
  onCancel,
  className = '',
  maxLength = 24,
}: {
  initialName: string
  label: string
  onCommit: (name: string) => boolean
  onCancel: () => void
  className?: string
  maxLength?: number
}) {
  const [name, setName] = useState(initialName)
  const input = useRef<HTMLInputElement>(null)
  const isFinishing = useRef(false)

  useLayoutEffect(() => {
    const element = input.current
    if (!element) return
    element.focus({ preventScroll: true })
    element.select()
    element.scrollIntoView({ block: 'nearest' })
  }, [])

  function finish(shouldCommit: boolean): void {
    if (isFinishing.current) return
    if (!shouldCommit) {
      isFinishing.current = true
      onCancel()
      return
    }
    isFinishing.current = true
    if (!onCommit(name)) {
      isFinishing.current = false
      input.current?.focus({ preventScroll: true })
    }
  }

  return (
    <input
      ref={input}
      className={`inline-name-input ${className}`.trim()}
      aria-label={label}
      value={name}
      maxLength={maxLength}
      autoComplete="off"
      spellCheck={false}
      onClick={(event) => event.stopPropagation()}
      onChange={(event) => setName(event.target.value)}
      onBlur={() => finish(true)}
      onKeyDown={(event: KeyboardEvent<HTMLInputElement>) => {
        if (event.nativeEvent.isComposing || event.keyCode === 229) return
        if (event.key === 'Enter') {
          event.preventDefault()
          finish(true)
        } else if (event.key === 'Escape') {
          event.preventDefault()
          finish(false)
        }
      }}
    />
  )
}
