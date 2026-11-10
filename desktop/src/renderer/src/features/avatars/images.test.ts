import { describe, expect, it } from 'vitest'
import { catEyeOverlaySource, catPortraitSource, listCatFrameSources } from './frames'
import { getCatAvatarImage } from './images'
import { CAT_AVATAR_IDS, WORKSPACE_AVATAR, type AvatarPresetId } from './presets'

describe('猫咪原图', () => {
  it('八款原图地址互不相同', () => {
    const sources = CAT_AVATAR_IDS.map((preset) => getCatAvatarImage(preset))
    expect(sources.every((source) => source.length > 0)).toBe(true)
    expect(new Set(sources).size).toBe(CAT_AVATAR_IDS.length)
    expect(sources.every((source) => source.includes('/frames/'))).toBe(true)
  })

  it('工作空间的猫有自己的七帧，并且不占用助手预设', () => {
    expect(CAT_AVATAR_IDS).not.toContain(WORKSPACE_AVATAR.id)
    const portrait = getCatAvatarImage(WORKSPACE_AVATAR.id)
    expect(portrait).toContain(WORKSPACE_AVATAR.id)
    expect(portrait).not.toMatch(/ears-|eyes-/)
    expect(CAT_AVATAR_IDS.map((preset) => getCatAvatarImage(preset))).not.toContain(portrait)
    const perk = catPortraitSource(WORKSPACE_AVATAR.id, 'perk')
    expect(perk).toMatch(/ears-perk(?!-half)/)
    expect(catPortraitSource(WORKSPACE_AVATAR.id, 'twitch-left')).toContain('ear-left-twitch')
    expect(catEyeOverlaySource(WORKSPACE_AVATAR.id, 'half')).toContain('eyes-half-overlay')
    expect(catEyeOverlaySource(WORKSPACE_AVATAR.id, 'closed')).toContain('eyes-closed-overlay')
    expect(catEyeOverlaySource(WORKSPACE_AVATAR.id, 'open')).toBeNull()
    expect(
      listCatFrameSources().filter((source) => source.includes(WORKSPACE_AVATAR.id)),
    ).toHaveLength(7)
  })

  it('未知预设不能换成另一张图', () => {
    expect(() => getCatAvatarImage('unknown' as AvatarPresetId)).toThrow('未知的猫咪预设')
  })
})
