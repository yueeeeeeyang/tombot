import { useState, type FormEvent } from 'react'
import { isComputerUrl } from '../workspace/model'

/**
 * 未连接时可以让服务端打开这只猫咪正在使用的电脑，也可以改填别的地址。
 * 地址为空就是未连接，断开只清掉画面，不改服务端上的工作区。
 */
export function ComputerPane({
  agentName,
  computerUrl,
  onSave,
  onConnect,
  onPrepare,
}: {
  agentName: string
  computerUrl: string
  onSave: (computerUrl: string) => void
  onConnect?: () => void
  onPrepare?: () => void
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
        {onPrepare && (
          <button type="button" onClick={onPrepare}>
            开始准备
          </button>
        )}
        {onConnect && (
          <button type="button" onClick={onConnect}>
            连接电脑
          </button>
        )}
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
        <p className="computer-note">连接后嵌出这只猫咪正在使用的电脑。也可以填写其他地址。</p>
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
