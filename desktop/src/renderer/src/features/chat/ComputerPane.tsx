import { useState, type FormEvent } from 'react'
import { isComputerUrl } from '../workspace/model'

/**
 * 远端电脑只有一个接入点：这只猫咪自己保存的地址。
 * 应用不创建、不探测电脑；地址为空就是未连接。
 */
export function ComputerPane({
  agentName,
  computerUrl,
  onSave,
}: {
  agentName: string
  computerUrl: string
  onSave: (computerUrl: string) => void
}) {
  const [draft, setDraft] = useState(computerUrl)
  const [error, setError] = useState('')
  const [isExpanded, setExpanded] = useState(false)

  function submit(event: FormEvent): void {
    event.preventDefault()
    const trimmed = draft.trim()
    if (trimmed && !isComputerUrl(trimmed)) {
      setError('请输入不含登录凭据的 HTTP 或 HTTPS 地址。')
      return
    }
    setError('')
    onSave(trimmed)
  }

  if (!computerUrl) {
    return (
      <form className="computer-pane" onSubmit={submit}>
        <p role="status">未连接</p>
        <label>
          远端电脑地址
          <input
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            placeholder="https://"
            inputMode="url"
            autoComplete="off"
            spellCheck={false}
          />
        </label>
        {error && <p role="alert">{error}</p>}
        <button type="submit">保存地址</button>
        <p className="computer-note">保存后会在这里嵌出画面。应用不会自己创建电脑。</p>
      </form>
    )
  }

  return (
    <div className={`computer-pane ${isExpanded ? 'is-expanded' : ''}`}>
      <p role="status">运行中</p>
      <iframe
        title={`${agentName}的远端电脑`}
        src={computerUrl}
        sandbox="allow-scripts allow-forms"
        referrerPolicy="no-referrer"
      />
      <div className="computer-actions">
        <button type="button" onClick={() => setExpanded((current) => !current)}>
          {isExpanded ? '还原' : '全屏查看'}
        </button>
        <button
          type="button"
          onClick={() => {
            setExpanded(false)
            onSave('')
          }}
        >
          断开
        </button>
      </div>
    </div>
  )
}
