import { Star } from 'lucide-react'
import type { CSSProperties } from 'react'
import { CatGraphic } from '../features/avatars/CatGraphic'
import { useAgentActivity } from '../features/avatars/activity'
import { getCatPalette } from '../features/avatars/palette'
import { resolveAvatarPresentation } from '../features/avatars/pose'
import type { CatAvatarId } from '../features/avatars/presets'
import { useAvatarPointer } from '../features/avatars/useAvatarPointer'
import { usePrefersReducedMotion } from '../features/avatars/usePrefersReducedMotion'

/**
 * 显示一只预设猫。姿态只来自 resolveAvatarPresentation。
 * 跟随指针是侧栏专属：会话标题和选择器不传 tracksPointer，避免满屏眼睛乱看。
 */
export function Avatar({
  preset,
  size = 'medium',
  starred = false,
  agentId,
  tracksPointer = false,
}: {
  preset: CatAvatarId
  size?: 'small' | 'medium' | 'large'
  starred?: boolean
  agentId?: string
  tracksPointer?: boolean
}) {
  const activity = useAgentActivity(agentId)
  const prefersReducedMotion = usePrefersReducedMotion()
  const { ref, pointer } = useAvatarPointer(tracksPointer, agentId)
  const presentation = resolveAvatarPresentation({ activity, pointer, prefersReducedMotion })
  const palette = getCatPalette(preset)
  return (
    <span
      ref={ref}
      className={`avatar avatar--${size}`}
      data-avatar={preset}
      data-pose={presentation.pose}
      data-pressed={presentation.isPressed ? 'true' : 'false'}
      style={
        {
          '--look-x': String(presentation.lookX),
          '--look-y': String(presentation.lookY),
          '--blink-delay': palette.blinkDelay,
        } as CSSProperties
      }
      aria-hidden="true"
    >
      <CatGraphic preset={preset} />
      {starred && (
        <span className="avatar-star">
          <Star size={10} fill="currentColor" strokeWidth={1.5} />
        </span>
      )}
    </span>
  )
}
