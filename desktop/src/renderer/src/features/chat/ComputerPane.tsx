import { Monitor } from 'lucide-react'
import { useId, useState, type FormEvent } from 'react'
import { isComputerUrl } from '../workspace/model'

/** 详情中先展示电脑摘要，展开后连接或查看；清空地址只断开画面，不改变服务端工作区。 */
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
  const controlsId = useId()
  const statusId = useId()
  const [draft, setDraft] = useState(computerUrl)
  const [error, setError] = useState('')
  const [isOpen, setOpen] = useState(Boolean(computerUrl))
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

  return (
    <div className={`computer-pane ${isExpanded ? 'is-expanded' : ''}`}>
      <button
        type="button"
        className="computer-card"
        aria-label={`${agentName}的电脑`}
        aria-describedby={statusId}
        aria-expanded={isOpen}
        aria-controls={controlsId}
        onClick={() => setOpen((current) => !current)}
      >
        <span className="computer-card-copy">
          <strong>{agentName}的电脑</strong>
          <span id={statusId} role="status">
            {computerUrl ? '已连接' : '未连接'}
          </span>
        </span>
        <span
          className={`computer-preview ${computerUrl ? 'is-connected' : ''}`}
          aria-hidden="true"
        >
          {computerUrl ? (
            <Monitor size={24} strokeWidth={1.5} />
          ) : (
            <>
              <span className="computer-sleep-face" />
              <span className="computer-sleep-marks">
                z<span>z</span>
              </span>
            </>
          )}
        </span>
      </button>
      <div id={controlsId} className="computer-controls" hidden={!isOpen}>
        {!computerUrl ? (
          <form onSubmit={submit}>
            <div className="computer-actions">
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
            </div>
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
            <p className="computer-note">连接后查看这只猫咪正在使用的电脑，也可以填写其他地址。</p>
          </form>
        ) : isOpen ? (
          <>
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
          </>
        ) : null}
      </div>
    </div>
  )
}
