import { ArrowDown } from 'lucide-react'

/** 不占据消息或输入框高度，键盘也可直接返回最新消息。 */
export function ScrollToBottom({ onClick }: { onClick: () => void }) {
  return (
    <button
      type="button"
      className="scroll-to-bottom round-button"
      aria-label="回到最新消息"
      title="回到最新消息"
      onClick={onClick}
    >
      <ArrowDown size={18} aria-hidden="true" />
    </button>
  )
}
