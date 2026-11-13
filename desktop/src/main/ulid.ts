import { randomBytes } from 'node:crypto'

// Crockford Base32 字母表，与 ULID 规范一致；不含 I、L、O、U。
const alphabet = '0123456789ABCDEFGHJKMNPQRSTVWXYZ'

/**
 * 生成 26 位 ULID。
 * 服务端用 ULID 作为幂等键与资源标识，客户端不能再用 UUID；
 * 时间戳在前保证同毫秒内创建的消息仍按时间有序。
 */
export function ulid(now: number = Date.now()): string {
  let time = BigInt(now)
  const timePart: string[] = new Array(10).fill('0')
  for (let index = 9; index >= 0; index -= 1) {
    timePart[index] = alphabet[Number(time & 31n)] ?? '0'
    time >>= 5n
  }
  let value = 0n
  for (const byte of randomBytes(10)) value = (value << 8n) | BigInt(byte)
  const randomPart: string[] = new Array(16).fill('0')
  for (let index = 15; index >= 0; index -= 1) {
    randomPart[index] = alphabet[Number(value & 31n)] ?? '0'
    value >>= 5n
  }
  return timePart.join('') + randomPart.join('')
}
