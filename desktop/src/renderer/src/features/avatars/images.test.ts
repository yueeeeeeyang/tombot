import { describe, expect, it } from 'vitest'
import { CAT_AVATAR_IDS, type CatAvatarId } from './presets'
import { CAT_AVATAR_IMAGES, getCatAvatarImage } from './images'

describe('猫咪原图', () => {
  it('八款原图地址互不相同', () => {
    const sources = CAT_AVATAR_IDS.map((preset) => getCatAvatarImage(preset))
    expect(sources.every((source) => source.length > 0)).toBe(true)
    expect(new Set(sources).size).toBe(CAT_AVATAR_IDS.length)
    expect(CAT_AVATAR_IMAGES.bombay).toBe(getCatAvatarImage('bombay'))
  })

  it('未知预设不能换成另一张图', () => {
    expect(() => getCatAvatarImage('unknown' as CatAvatarId)).toThrow('未知的猫咪预设')
  })
})
