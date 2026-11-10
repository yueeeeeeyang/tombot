import { useState } from 'react'
import { Dialog } from '../../components/Dialog'
import type { Agent } from './model'

/** 删除前展示影响范围；取消不改变任何内容，失败时保留弹窗供重试。 */
export function DeleteCatDialog({
  agent,
  onDelete,
  onClose,
}: {
  agent: Agent
  onDelete: () => void
  onClose: () => void
}) {
  const [error, setError] = useState('')
  return (
    <Dialog title="删除猫咪" onClose={onClose}>
      <p className="dialog-description">
        确定删除「{agent.name}」吗？它的 {agent.messages.length}{' '}
        条消息和输入草稿也会被删除，此操作无法撤销。
      </p>
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
      <div className="cat-dialog-actions">
        <button className="secondary-button" autoFocus onClick={onClose}>
          取消
        </button>
        <button
          className="primary-button danger-button"
          onClick={() => {
            try {
              onDelete()
            } catch (failure) {
              setError((failure as Error).message)
            }
          }}
        >
          删除猫咪
        </button>
      </div>
    </Dialog>
  )
}
