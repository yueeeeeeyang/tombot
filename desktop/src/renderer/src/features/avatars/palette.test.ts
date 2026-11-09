import { describe, expect, it } from 'vitest'
import { CAT_AVATAR_IDS, type CatAvatarId } from './presets'
import { CAT_PALETTES, CAT_STICKER_RIM, getCatPalette, listCatPalettes } from './palette'

describe('猫咪毛色', () => {
  it('八款毛色和眨眼节奏都可区分，且不使用主题色', () => {
    const palettes = listCatPalettes()
    expect(palettes.map((item) => item.id)).toEqual([...CAT_AVATAR_IDS])
    expect(new Set(palettes.map((item) => item.palette.fur)).size).toBe(8)
    expect(new Set(palettes.map((item) => item.palette.blinkDelay)).size).toBe(8)
    for (const { palette } of palettes) {
      for (const color of [palette.fur, palette.iris, palette.ear, palette.earRight, palette.lid]) {
        expect(color).toMatch(/^#[0-9a-f]{6}$/)
      }
    }
    expect(CAT_STICKER_RIM).toBe('#f4ead8')
  })

  it('保留每款猫的标志毛色和眼睛', () => {
    expect(getCatPalette('bombay')).toMatchObject({ fur: '#2a2a30', iris: '#f0a01c' })
    expect(getCatPalette('orange-tabby')).toMatchObject({ fur: '#e8832a', marking: '#c45e12' })
    expect(getCatPalette('british-shorthair')).toMatchObject({ fur: '#7d8794', iris: '#e87820' })
    expect(getCatPalette('ragdoll')).toMatchObject({ iris: '#2f8cff', marking: '#8d5e48' })
    expect(getCatPalette('siamese').ear).toBe('#6b3a2a')
    expect(getCatPalette('american-shorthair')).toMatchObject({
      iris: '#3cba5a',
      marking: '#3a4150',
    })
    expect(getCatPalette('persian')).toMatchObject({ fur: '#f7f4ef', iris: '#c6c43a' })
    expect(getCatPalette('calico').ear).not.toBe(getCatPalette('calico').earRight)
    expect(CAT_PALETTES.bombay).toBe(getCatPalette('bombay'))
  })

  it('未知预设不能当成某只猫静默画出来', () => {
    expect(() => getCatPalette('unknown' as CatAvatarId)).toThrow('未知的猫咪预设')
  })
})
