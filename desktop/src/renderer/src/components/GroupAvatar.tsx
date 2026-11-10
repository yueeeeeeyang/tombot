import { catPortraitSource } from '../features/avatars/frames'
import type { CatAvatarId } from '../features/avatars/presets'
import { CUSTOM_AVATAR_PATTERN } from '../features/avatars/customAvatar'

/** 使用建群时保存的头像快照，成员增减不会使群图标来回变化。 */
export function GroupAvatar({
  presets,
  size = 'medium',
}: {
  presets: string[]
  size?: 'small' | 'medium' | 'large'
}) {
  return (
    <span
      className={`group-avatar group-avatar--${size}`}
      data-count={presets.length}
      aria-label="群头像"
    >
      {presets.slice(0, 4).map((preset, index) => (
        <img
          key={index}
          src={
            CUSTOM_AVATAR_PATTERN.test(preset)
              ? preset
              : catPortraitSource(preset as CatAvatarId, 'rest')
          }
          alt=""
        />
      ))}
    </span>
  )
}
