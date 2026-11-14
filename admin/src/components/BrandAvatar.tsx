import catAvatar from '../../../desktop/src/renderer/src/assets/cats/frames/bombay.png'

/** 管理平台与客户端登录页共用同一张孟买猫原图，不引入桌面动画与活动状态依赖。 */
export function BrandAvatar({ size = 64 }: { size?: number }) {
  return (
    <img
      src={catAvatar}
      alt="TomBot 猫咪头像"
      className="brand-avatar"
      width={size}
      height={size}
    />
  )
}
