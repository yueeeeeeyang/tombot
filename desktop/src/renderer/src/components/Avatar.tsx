import { Star } from 'lucide-react'
import type { CSSProperties } from 'react'
import { useAgentActivity } from '../features/avatars/activity'
import { getCatAvatarImage } from '../features/avatars/images'
import { resolveAvatarPresentation } from '../features/avatars/pose'
import type { CatAvatarId } from '../features/avatars/presets'
import { useAvatarPointer } from '../features/avatars/useAvatarPointer'
import { useLookTilt } from '../features/avatars/useLookTilt'
import { usePrefersReducedMotion } from '../features/avatars/usePrefersReducedMotion'

/**
 * 显示打包好的猫咪原图。姿态只来自 resolveAvatarPresentation。
 * 跟随指针是侧栏专属：会话标题和选择器不传 tracksPointer，避免满屏一起转头。
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
  useLookTilt(ref, presentation.lookX, presentation.lookY, prefersReducedMotion)
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
        } as CSSProperties
      }
      aria-hidden="true"
    >
      <span className="cat-stage">
        <span className="cat-tilt">
          <span className="cat-motion">
            <img
              className="cat-portrait"
              src={getCatAvatarImage(preset)}
              alt=""
              draggable={false}
              decoding="sync"
            />
          </span>
        </span>
      </span>
      {presentation.pose === 'working' && (
        <span className="cat-work-mark">
          <svg className="cat-work-dots" viewBox="0 0 22 8">
            <circle cx="2.2" cy="4" r="1.6" />
            <circle cx="11" cy="4" r="1.6" />
            <circle cx="19.8" cy="4" r="1.6" />
          </svg>
        </span>
      )}
      {starred && (
        <span className="avatar-star">
          <Star size={10} fill="currentColor" strokeWidth={1.5} />
        </span>
      )}
    </span>
  )
}
