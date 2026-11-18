import type {
  ConsoleBackend,
  ConsoleConnection,
  ConsoleStreamEvent,
} from '../../../../shared/console'
import { ulid } from '../../../../shared/ulid'

/** 实现 noVNC 的标准 WebSocket 通道接口；只转交 RFB 字节，不定义画面或输入编码。 */
export class ConsoleChannel {
  binaryType = 'arraybuffer'
  protocol = ''
  readyState = 0
  onopen: ((event: Event) => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  private connection?: ConsoleConnection

  constructor(backend: ConsoleBackend, id: string) {
    // noVNC 先安装事件处理器，再接收连接结果；同步测试连接也遵守相同顺序。
    queueMicrotask(() => {
      if (this.readyState === 3) return
      this.connection = backend.connectConsole(id, ulid(), (event) => this.receive(event))
    })
  }

  private receive(event: ConsoleStreamEvent): void {
    if (this.readyState === 3) return
    if (event.kind === 'open') {
      this.readyState = 1
      this.onopen?.(new Event('open'))
    } else if (event.kind === 'bytes') {
      const bytes = Uint8Array.from(event.bytes)
      this.onmessage?.(new MessageEvent('message', { data: bytes.buffer }))
    } else {
      if (event.kind === 'error') this.onerror?.(new Event('error'))
      this.readyState = 3
      this.connection?.close()
      this.onclose?.(new CloseEvent('close', { code: 1006, wasClean: false }))
    }
  }

  send(bytes: Uint8Array): void {
    if (this.readyState !== 1) throw new Error('电脑画面连接已结束。')
    this.connection?.send(Uint8Array.from(bytes))
  }

  close(): void {
    if (this.readyState === 3) return
    this.readyState = 3
    this.connection?.close()
    this.onclose?.(new CloseEvent('close', { code: 1000, wasClean: true }))
  }
}
