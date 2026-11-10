import { describe, expect, it } from 'vitest'
import { quantizeLook, resolveLookOffset, type LookBounds } from './look'

const bounds: LookBounds = { left: 100, top: 200, width: 34, height: 34 }

describe('头像视线', () => {
  it('以头像中心为原点，贴边时到达两端', () => {
    expect(resolveLookOffset(117, 217, bounds)).toEqual({ x: 0, y: 0 })
    expect(resolveLookOffset(100, 217, bounds)).toEqual({ x: -1, y: 0 })
    expect(resolveLookOffset(134, 217, bounds)).toEqual({ x: 1, y: 0 })
    expect(resolveLookOffset(117, 200, bounds)).toEqual({ x: 0, y: -1 })
  })

  it('越出头像后停在边界，不把眼睛推出眼眶', () => {
    expect(resolveLookOffset(-40, 800, bounds)).toEqual({ x: -1, y: 1 })
  })

  it('无效坐标或空尺寸直接失败', () => {
    expect(() => resolveLookOffset(Number.NaN, 217, bounds)).toThrow('指针位置无效')
    expect(() => resolveLookOffset(117, 217, { ...bounds, width: 0 })).toThrow('头像尺寸无效')
    expect(() => resolveLookOffset(117, 217, { ...bounds, height: -4 })).toThrow('头像尺寸无效')
    expect(() => resolveLookOffset(117, 217, undefined as unknown as LookBounds)).toThrow(
      '指针位置无效',
    )
  })

  it('把视线量化到百分位，并拒绝非数字', () => {
    expect(quantizeLook(1 / 3)).toBe(0.33)
    expect(quantizeLook(2)).toBe(1)
    expect(quantizeLook(-2)).toBe(-1)
    expect(() => quantizeLook(Number.POSITIVE_INFINITY)).toThrow('视线偏移无效')
    expect(() => quantizeLook('0.2' as unknown as number)).toThrow('视线偏移无效')
  })
})
