import { Star } from 'lucide-react'
import { CAT_AVATAR_IMAGES } from '../features/avatars/images'
import type { CatAvatarId } from '../features/avatars/presets'

/** 显示随应用打包的透明猫咪头像；名称由外层控件提供，星标用于默认助手。 */
export function Avatar({
  preset,
  size = 'medium',
  starred = false,
}: {
  preset: CatAvatarId
  size?: 'small' | 'medium' | 'large'
  starred?: boolean
}) {
  return (
    <span className={`avatar avatar--${size}`} data-avatar={preset} aria-hidden="true">
      <img src={CAT_AVATAR_IMAGES[preset]} alt="" draggable={false} decoding="sync" />
      {starred && (
        <span className="avatar-star">
          <Star size={10} fill="currentColor" strokeWidth={1.5} />
        </span>
      )}
    </span>
  )
}
