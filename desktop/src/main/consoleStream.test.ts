import { EventEmitter } from 'node:events'
import type { IpcMain, IpcMainEvent, WebContents } from 'electron'
import type { WebSocket } from 'undici'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ConsoleStreams } from './consoleStream'
const id = '01ARZ3NDEKTSV4RRFFQ69G5FAV'
const streamId = '01ARZ3NDEKTSV4RRFFQ69G5FAW'
function fixture() {
  const ipc = new EventEmitter()
  const owner = Object.assign(new EventEmitter(), {
    mainFrame: {},
    send: vi.fn(),
    isDestroyed: () => false,
  })
  const socket = {
    readyState: 1,
    bufferedAmount: 0,
    close: vi.fn(),
    send: vi.fn(),
    binaryType: '',
    onopen: undefined as (() => void) | undefined,
    onmessage: undefined as ((event: { data: unknown }) => void) | undefined,
  }
  const connect = vi.fn(async () => socket as unknown as WebSocket)
  const streams = new ConsoleStreams(connect)
  streams.register(ipc as unknown as IpcMain)
  const event = {
    sender: owner as unknown as WebContents,
    senderFrame: owner.mainFrame,
  } as IpcMainEvent
  return { ipc, owner, socket, connect, streams, event }
}
afterEach(() => vi.useRealTimers())
describe('受信主进程 RFB 通道', () => {
  it('验证主 frame 与会话标识，字节只发给所属窗口', async () => {
    const f = fixture()
    f.ipc.emit('desktop:console-stream-open', { ...f.event, senderFrame: {} }, id, streamId)
    f.ipc.emit('desktop:console-stream-open', f.event, 'https://foreign.test', streamId)
    expect(f.connect).not.toHaveBeenCalled()
    f.ipc.emit('desktop:console-stream-open', f.event, id, streamId)
    await Promise.resolve()
    f.socket.onopen?.()
    f.socket.onmessage?.({ data: new Uint8Array([1, 2, 3]).buffer })
    expect(f.owner.send).toHaveBeenLastCalledWith('desktop:console-stream', streamId, {
      kind: 'bytes',
      bytes: new Uint8Array([1, 2, 3]),
    })
    f.ipc.emit('desktop:console-stream-ack', f.event, streamId, 3)
    f.ipc.emit('desktop:console-stream-write', f.event, streamId, new Uint8Array([4, 1]))
    expect(f.socket.send).toHaveBeenCalledExactlyOnceWith(new Uint8Array([4, 1]))
    f.ipc.emit(
      'desktop:console-stream-write',
      { ...f.event, senderFrame: {} },
      streamId,
      new Uint8Array([5]),
    )
    expect(f.socket.send).toHaveBeenCalledOnce()
    f.streams.closeAll()
    expect(f.socket.close).toHaveBeenCalledOnce()
    expect(f.owner.listenerCount('destroyed')).toBe(0)
  })
  it('慢渲染器达到缓冲上限时关闭，不积压无限画面', async () => {
    const f = fixture()
    f.ipc.emit('desktop:console-stream-open', f.event, id, streamId)
    await Promise.resolve()
    for (let n = 0; n < 3; n++) f.socket.onmessage?.({ data: new ArrayBuffer(1 << 20) })
    expect(f.socket.close).toHaveBeenCalledOnce()
    expect(f.owner.send).toHaveBeenLastCalledWith('desktop:console-stream', streamId, {
      kind: 'error',
    })
  })
  it('输入过大、无效确认和握手超时立即关闭，不重发输入', async () => {
    for (const mode of ['input', 'ack', 'timeout']) {
      vi.useFakeTimers()
      const f = fixture()
      f.ipc.emit('desktop:console-stream-open', f.event, id, streamId)
      await Promise.resolve()
      if (mode === 'input')
        f.ipc.emit('desktop:console-stream-write', f.event, streamId, new Uint8Array(65537))
      if (mode === 'ack') f.ipc.emit('desktop:console-stream-ack', f.event, streamId, 1)
      if (mode === 'timeout') await vi.advanceTimersByTimeAsync(10_001)
      expect(f.socket.close).toHaveBeenCalledOnce()
      expect(f.socket.send).not.toHaveBeenCalled()
      vi.useRealTimers()
    }
  })
})
