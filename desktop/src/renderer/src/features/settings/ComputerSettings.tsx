import { useEffect, useRef, useState } from 'react'
import type { ComputerStatus } from '../../../../shared/computer'
import { getBackend } from '../desktop/backend'

const labels: Record<ComputerStatus['state'], string> = {
  running: '运行中',
  stopped: '已停止',
  restarting: '正在重启…',
  failed: '重启未完成',
}

export function ComputerSettings({ onRunning }: { onRunning?: () => void }) {
  const [status, setStatus] = useState<ComputerStatus>()
  const [error, setError] = useState('')
  const [confirm, setConfirm] = useState(false)
  const [pending, setPending] = useState(false)
  const [refresh, setRefresh] = useState(0)
  const version = useRef(0)
  useEffect(() => {
    const generation = ++version.current
    let active = true
    let timer: ReturnType<typeof setTimeout>
    const load = async () => {
      try {
        const next = await getBackend().computerStatus()
        if (!active || generation !== version.current) return
        setStatus(next)
        setError('')
        if (next.state === 'restarting') timer = setTimeout(() => void load(), 1500)
        if (next.state === 'running') onRunning?.()
      } catch (cause) {
        if (active && generation === version.current) setError((cause as Error).message)
      }
    }
    void load()
    return () => {
      active = false
      clearTimeout(timer)
    }
  }, [refresh, onRunning])
  async function restart() {
    if (!status || pending) return
    setConfirm(false)
    setPending(true)
    const generation = ++version.current
    try {
      const next = await getBackend().restartComputer(status.version)
      if (generation === version.current) {
        setStatus(next)
        setRefresh((value) => value + 1)
      }
    } catch (cause) {
      if (generation === version.current)
        setError((cause as Error).message + ' 请刷新以确认重启状态。')
    } finally {
      if (generation === version.current) setPending(false)
    }
  }
  return (
    <>
      <h2 id="settings-section-title">电脑</h2>
      <section className="settings-section" aria-label="Bot 的电脑">
        <h3>Bot 的电脑</h3>
        <div className="settings-card">
          <div className="setting-row">
            <span>共用电脑状态</span>
            <strong role="status">{status ? labels[status.state] : '正在读取…'}</strong>
          </div>
          <div className="setting-row">
            <div>
              <strong>重启电脑</strong>
              <p>所有 Bot 共用这台电脑。重启会中断正在执行的操作，已保存的文件会保留。</p>
            </div>
            <button
              type="button"
              className="settings-sign-out"
              disabled={pending || !!error || !status || status.state === 'restarting' || confirm}
              onClick={() => setConfirm(true)}
            >
              重启电脑
            </button>
          </div>
          {confirm && (
            <div className="setting-row">
              <span>现在重启所有 Bot 共用的电脑？</span>
              <button onClick={() => setConfirm(false)}>取消</button>
              <button className="settings-sign-out" onClick={() => void restart()}>
                确认重启
              </button>
            </div>
          )}
        </div>
        {error && <p role="alert">{error}</p>}
        {(error || status?.state === 'failed') && (
          <button onClick={() => setRefresh((value) => value + 1)}>刷新状态</button>
        )}
        {status?.state === 'failed' && (
          <p>请检查电脑节点是否在线。若再次重启仍失败，请联系管理员。</p>
        )}
      </section>
    </>
  )
}
