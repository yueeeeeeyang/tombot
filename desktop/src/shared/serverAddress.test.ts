import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  normalizeServerAddress,
  parseStoredSession,
  selectServerAddress,
  serializeStoredSession,
  storedServerAddress,
} from './serverAddress'

describe('服务地址', () => {
  it('接受远端主机并去掉多余的斜杠', () => {
    expect(normalizeServerAddress('https://tom.example.com')).toBe('https://tom.example.com')
    expect(normalizeServerAddress('https://tom.example.com/')).toBe('https://tom.example.com')
    expect(normalizeServerAddress('http://192.168.1.10:8787')).toBe('http://192.168.1.10:8787')
    expect(normalizeServerAddress(' HTTP://Tom.Example.com:8443 ')).toBe(
      'http://tom.example.com:8443',
    )
    expect(normalizeServerAddress('http://[::1]:8787')).toBe('http://[::1]:8787')
  })

  it('用户明确填写的环回地址可以保存', () => {
    expect(normalizeServerAddress('http://127.0.0.1:8787')).toBe('http://127.0.0.1:8787')
  })

  it('拒绝空地址、缺协议、路径、凭据和其他协议', () => {
    expect(() => normalizeServerAddress('   ')).toThrow('请输入服务地址')
    expect(() => normalizeServerAddress('tom.example.com')).toThrow('http://')
    expect(() => normalizeServerAddress('127.0.0.1:8787')).toThrow('http://')
    expect(() => normalizeServerAddress('https://tom.example.com/tombot')).toThrow('路径')
    expect(() => normalizeServerAddress('https://tom.example.com/?q=1')).toThrow('路径')
    expect(() => normalizeServerAddress('https://account:placeholder@tom.example.com')).toThrow(
      '账号或密码',
    )
    expect(() => normalizeServerAddress('javascript:alert(1)')).toThrow('http')
    expect(() => normalizeServerAddress('file:///tmp/tombot')).toThrow('http')
    expect(() => normalizeServerAddress(`https://${'a'.repeat(3000)}.example`)).toThrow('过长')
  })

  it('没有已保存地址时不会回退到本机', () => {
    expect(selectServerAddress(undefined, undefined)).toEqual({ connect: '', prefill: '' })
    expect(selectServerAddress('', '   ')).toEqual({ connect: '', prefill: '' })
    expect(selectServerAddress(null, '')).toEqual({ connect: '', prefill: '' })
    expect(JSON.stringify(selectServerAddress(undefined, undefined))).not.toContain('127.0.0.1')
    expect(storedServerAddress(undefined)).toBe('')
    expect(storedServerAddress('不是地址')).toBe('')
  })

  it('环境变量只预填，不会变成自动连接的地址', () => {
    expect(selectServerAddress(undefined, 'http://127.0.0.1:8787')).toEqual({
      connect: '',
      prefill: 'http://127.0.0.1:8787',
    })
    expect(selectServerAddress(undefined, 'https://tom.example.com/')).toEqual({
      connect: '',
      prefill: 'https://tom.example.com',
    })
    expect(selectServerAddress(undefined, 'tom.example.com')).toEqual({
      connect: '',
      prefill: 'tom.example.com',
    })
  })

  it('已保存的地址优先于环境变量', () => {
    expect(selectServerAddress('https://saved.example/', 'http://127.0.0.1:8787')).toEqual({
      connect: 'https://saved.example',
      prefill: 'https://saved.example',
    })
  })

  it('旧会话文件没有地址时保持空白，损坏内容不会变成默认主机', () => {
    expect(parseStoredSession('{"deviceToken":"saved-device"}')).toEqual({
      deviceToken: 'saved-device',
      serverUrl: '',
    })
    expect(parseStoredSession('not-json')).toEqual({ deviceToken: '', serverUrl: '' })
    expect(
      parseStoredSession('{"deviceToken":"saved-device","serverUrl":"ftp://example.com"}'),
    ).toEqual({ deviceToken: 'saved-device', serverUrl: '' })
    const roundTrip = serializeStoredSession({
      deviceToken: 'saved-device',
      serverUrl: 'https://tom.example.com',
    })
    expect(parseStoredSession(roundTrip)).toEqual({
      deviceToken: 'saved-device',
      serverUrl: 'https://tom.example.com',
    })
    expect(serializeStoredSession({ deviceToken: '', serverUrl: '' })).not.toContain('127.0.0.1')
  })

  it('主进程不再在缺少地址时回退到本机 8787', () => {
    const source = readFileSync(join(process.cwd(), 'src/main/desktopServer.ts'), 'utf8')
    expect(source).not.toContain('127.0.0.1:8787')
    expect(source).not.toMatch(/TOMBOT_SERVER_URL\?\.trim\(\)\s*\|\|/)
  })
})
