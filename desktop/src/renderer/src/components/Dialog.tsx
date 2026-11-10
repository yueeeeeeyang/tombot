import { useLayoutEffect, useRef, type ReactNode } from 'react'
import { X } from 'lucide-react'

/** 使用原生模态框约束焦点，在卸载前关闭以可靠恢复触发按钮焦点。 */
export function Dialog({
  title,
  children,
  onClose,
  variant = 'default',
}: {
  title: string
  children: ReactNode
  onClose: () => void
  variant?: 'default' | 'search' | 'settings' | 'recruit' | 'dm'
}) {
  const dialog = useRef<HTMLDialogElement>(null)
  useLayoutEffect(() => {
    const element = dialog.current
    const trigger = document.activeElement
    element?.showModal()
    return () => {
      element?.close()
      if (trigger instanceof HTMLElement && trigger.isConnected)
        trigger.focus({ preventScroll: true })
    }
  }, [])

  return (
    <dialog
      ref={dialog}
      className={`dialog dialog--${variant}`}
      aria-label={title}
      onCancel={(event) => {
        event.preventDefault()
        onClose()
      }}
      onClick={(event) => {
        if (event.target === event.currentTarget) onClose()
      }}
    >
      <div className="dialog-content">
        {(variant === 'default' || variant === 'recruit' || variant === 'dm') && (
          <div className="dialog-heading">
            <h2>{title}</h2>
            <button className="icon-button" aria-label="关闭弹窗" onClick={onClose}>
              <X size={17} />
            </button>
          </div>
        )}
        {children}
      </div>
    </dialog>
  )
}
