import type { IpcMain, IpcMainEvent, WebContents } from 'electron'
import type { WebSocket } from 'undici'
import { consoleID } from '../shared/console'

type Stream = {
  owner: WebContents
  socket?: WebSocket
  pending: number
  timer: ReturnType<typeof setTimeout>
  closed: boolean
  cleanup: () => void
}

/** IPC 只承载原始 RFB 字节；连接地址、凭据和证书校验始终由主进程掌握。 */
export class ConsoleStreams {
  private streams = new Map<string, Stream>()
  constructor(private readonly connect: (id: string) => Promise<WebSocket>) {}

  closeAll(): void {
    for (const [id, stream] of this.streams) this.close(id, stream)
  }

  private close(id: string, stream: Stream, failed = false): void {
    if (stream.closed) return
    stream.closed = true
    stream.cleanup()
    clearTimeout(stream.timer)
    stream.socket?.close()
    this.streams.delete(id)
    if (!stream.owner.isDestroyed())
      stream.owner.send('desktop:console-stream', id, { kind: failed ? 'error' : 'close' })
  }

  register(ipc: IpcMain): void {
    ipc.on(
      'desktop:console-stream-open',
      (event: IpcMainEvent, consoleId: unknown, streamId: unknown) => {
        if (
          event.senderFrame !== event.sender.mainFrame ||
          !consoleID.safeParse(consoleId).success ||
          !consoleID.safeParse(streamId).success
        )
          return
        const id = streamId as string
        if (this.streams.has(id) || this.streams.size >= 8) {
          event.sender.send('desktop:console-stream', id, { kind: 'error' })
          return
        }
        const stream: Stream = {
          owner: event.sender,
          pending: 0,
          closed: false,
          cleanup: () => event.sender.removeListener('destroyed', destroyed),
          timer: setTimeout(() => this.close(id, stream, true), 10_000),
        }
        this.streams.set(id, stream)
        const destroyed = () => this.close(id, stream)
        event.sender.once('destroyed', destroyed)
        void this.connect(consoleId as string)
          .then((socket) => {
            if (stream.closed) {
              socket.close()
              return
            }
            stream.socket = socket
            socket.binaryType = 'arraybuffer'
            socket.onopen = () => {
              if (stream.closed) return
              clearTimeout(stream.timer)
              if (!stream.owner.isDestroyed())
                stream.owner.send('desktop:console-stream', id, { kind: 'open' })
            }
            socket.onmessage = (message) => {
              if (stream.closed) return
              if (!(message.data instanceof ArrayBuffer) || message.data.byteLength > 1 << 20) {
                this.close(id, stream, true)
                return
              }
              stream.pending += message.data.byteLength
              if (stream.pending > 2 << 20 || stream.owner.isDestroyed()) {
                this.close(id, stream, true)
                return
              }
              stream.owner.send('desktop:console-stream', id, {
                kind: 'bytes',
                bytes: new Uint8Array(message.data),
              })
            }
            socket.onerror = () => this.close(id, stream, true)
            socket.onclose = () => {
              event.sender.removeListener('destroyed', destroyed)
              this.close(id, stream)
            }
          })
          .catch(() => {
            event.sender.removeListener('destroyed', destroyed)
            this.close(id, stream, true)
          })
      },
    )
    ipc.on('desktop:console-stream-write', (event: IpcMainEvent, id: string, bytes: unknown) => {
      const stream = this.streams.get(id)
      if (!stream || stream.owner !== event.sender || event.senderFrame !== event.sender.mainFrame)
        return
      if (
        !(bytes instanceof Uint8Array) ||
        bytes.byteLength === 0 ||
        bytes.byteLength > 64 << 10 ||
        stream.socket?.readyState !== 1 ||
        stream.socket.bufferedAmount > 256 << 10
      ) {
        this.close(id, stream, true)
        return
      }
      stream.socket.send(bytes)
    })
    ipc.on('desktop:console-stream-ack', (event: IpcMainEvent, id: string, count: number) => {
      const stream = this.streams.get(id)
      if (!stream || stream.owner !== event.sender || event.senderFrame !== event.sender.mainFrame)
        return
      if (!Number.isSafeInteger(count) || count < 1 || count > stream.pending) {
        this.close(id, stream, true)
        return
      }
      stream.pending -= count
    })
    ipc.on('desktop:console-stream-close', (event: IpcMainEvent, id: string) => {
      const stream = this.streams.get(id)
      if (stream?.owner === event.sender && event.senderFrame === event.sender.mainFrame)
        this.close(id, stream)
    })
  }
}
