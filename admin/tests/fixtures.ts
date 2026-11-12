import { z } from 'zod'
import { AdminClient } from '../src/api/client'
import { sessionSchema } from '../src/api/schemas'

export const id = (digit: number) => '0'.repeat(25) + digit.toString()
export function session() {
  return {
    principal: {
      user_id: id(1),
      session_id: id(2),
      device_id: id(3),
      role: 'admin',
      scope: 'admin',
    },
    csrf_token: crypto.randomUUID(),
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
    expires_at: new Date(Date.now() + 3_600_000).toISOString(),
  }
}
export function json(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}
export const empty = () => new Response(null, { status: 204 })
export function enrollment() {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  return {
    secret: Array.from(
      crypto.getRandomValues(new Uint8Array(32)),
      (value) => alphabet[value % 32],
    ).join(''),
    period: 30,
    digits: 6,
    algorithm: 'SHA1',
  }
}
export function activationInputs() {
  return {
    token: btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))))
      .replaceAll('+', '-')
      .replaceAll('/', '_')
      .replace(/=+$/, ''),
    password: crypto.randomUUID(),
    code: String(crypto.getRandomValues(new Uint32Array(1))[0]! % 1_000_000).padStart(6, '0'),
  }
}
export const failure = (code: string, message: string, status = 400) =>
  json({ code, message, request_id: id(9) }, status)
export type Route = (path: string, options: RequestInit) => Promise<Response> | Response
export function makeClient(route: Route) {
  const authentication = session()
  const calls: { path: string; options: RequestInit }[] = []
  const fetcher = async (input: RequestInfo | URL, options: RequestInit = {}) => {
    const path = String(input).replace('/api/v2/admin', '')
    calls.push({ path, options })
    if (path === '/auth/session' || path === '/auth/login' || path === '/auth/refresh')
      return json(authentication)
    if (path === '/auth/logout' || path === '/auth/verify') return empty()
    return route(path, options)
  }
  return { client: new AdminClient(fetcher as typeof fetch), calls, authentication }
}
export const operation = (status = 'pending') => ({
  id: id(6),
  user_id: id(4),
  computer_id: id(5),
  status,
  stage: status === 'succeeded' ? 'completed' : 'inspecting',
  attempt: 1,
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
})
export const provider = {
  id: id(4),
  name: '测试供应商',
  protocol: 'openai_compat',
  base_url: 'https://models.example.test',
  is_enabled: true,
  revision: 7,
  created_at: new Date().toISOString(),
}
export const voidSchema = z.undefined()
export { sessionSchema }
