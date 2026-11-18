import RFB from '@novnc/novnc'
import { useEffect, useRef, useState } from 'react'
import type { ConsoleBackend } from '../../../../shared/console'
import { ConsoleChannel } from './consoleChannel'

/** 观看与接管复用同一 RFB 会话，分辨率由 Bot 的桌面固定，显示按容器缩放。 */
export function ComputerScreen({
  backend,
  consoleId,
  name,
  isControlling,
  onConnected,
  onError,
}: {
  backend: ConsoleBackend
  consoleId: string
  name: string
  isControlling: boolean
  onConnected: () => void
  onError: (message: string) => void
}) {
  const host = useRef<HTMLDivElement>(null)
  const client = useRef<RFB>(undefined)
  const [text, setText] = useState('')
  const [connected, setConnected] = useState(false)
  const controlling = useRef(isControlling)
  const errorHandler = useRef(onError)
  useEffect(() => {
    errorHandler.current = onError
  }, [onError])
  useEffect(() => {
    controlling.current = isControlling
    if (client.current) client.current.viewOnly = !isControlling
  }, [isControlling])
  useEffect(() => {
    if (!host.current) return
    const channel = new ConsoleChannel(backend, consoleId)
    const rfb = new RFB(host.current, channel, { shared: true })
    client.current = rfb
    rfb.viewOnly = !controlling.current
    rfb.scaleViewport = true
    rfb.resizeSession = false
    rfb.background = '#151515'
    rfb.showDotCursor = true
    const ready = () => {
      setConnected(true)
      onConnected()
    }
    const disconnected = () => {
      setConnected(false)
      errorHandler.current('电脑画面连接已结束，请重新连接。')
    }
    rfb.addEventListener('connect', ready)
    rfb.addEventListener('disconnect', disconnected)
    rfb.addEventListener('securityfailure', disconnected)
    return () => {
      rfb.removeEventListener('connect', ready)
      rfb.removeEventListener('disconnect', disconnected)
      rfb.removeEventListener('securityfailure', disconnected)
      rfb.disconnect()
      channel.close()
      client.current = undefined
    }
  }, [backend, consoleId, onConnected])
  return (
    <div className="computer-session">
      <div ref={host} className="computer-screen" aria-label={`${name}的电脑画面`} />
      {!connected && <p role="status">正在连接电脑画面…</p>}
      {isControlling && (
        <form
          className="computer-text"
          onSubmit={(event) => {
            event.preventDefault()
            if (!client.current || !connected || !controlling.current) return
            for (const character of Array.from(text)) {
              const code = character.codePointAt(0)!
              client.current.sendKey(code <= 0xff ? code : 0x01000000 | code)
            }
            setText('')
            client.current.focus()
          }}
        >
          <label>
            <span>输入到电脑</span>
            <input
              aria-label="输入到电脑"
              value={text}
              maxLength={256}
              autoComplete="off"
              onChange={(event) => setText(event.target.value)}
              placeholder="支持中文输入"
            />
          </label>
          <button type="submit" disabled={!connected || !text}>
            输入
          </button>
        </form>
      )}
    </div>
  )
}
