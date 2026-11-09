import { memo, useId, type ReactNode } from 'react'
import { CAT_CONTOUR, CAT_STICKER_RIM, getCatPalette, type CatPalette } from './palette'
import type { CatAvatarId } from './presets'

type EarShape = 'medium' | 'small' | 'tall' | 'tiny'
type FaceShape = 'round' | 'wide' | 'wedge' | 'cloud'
type MouthShape = 'smile' | 'open' | 'serious'
type Marking = 'none' | 'tabby' | 'ragdoll' | 'siamese' | 'calico'

type BreedLayout = {
  face: FaceShape
  ears: EarShape
  eyeOffset: number
  eyeCy: number
  eyeRx: number
  eyeRy: number
  mouth: MouthShape
  noseY: number
  markings: Marking
  hasFluff: boolean
  hasMuzzle: boolean
}

const EARS: Record<
  EarShape,
  { left: string; leftInner: string; right: string; rightInner: string }
> = {
  medium: {
    left: 'M24 38 C22 28 15 14 13 10 C12 5 22 6 26 18 C30 28 34 36 34 42 Z',
    leftInner: 'M24 36 C23 28 18 16 17 13 C18 12 23 14 25 24 C27 30 30 36 30 40 Z',
    right: 'M56 38 C58 28 65 14 67 10 C68 5 58 6 54 18 C50 28 46 36 46 42 Z',
    rightInner: 'M56 36 C57 28 62 16 63 13 C62 12 57 14 55 24 C53 30 50 36 50 40 Z',
  },
  small: {
    left: 'M18 40 C16 32 17 24 22 20 C27 16 33 21 33 31 C33 36 29 41 25 42 Z',
    leftInner: 'M22 38 C21 32 22 27 25 25 C28 23 30 27 30 33 C30 36 28 38 26 39 Z',
    right: 'M62 40 C64 32 63 24 58 20 C53 16 47 21 47 31 C47 36 51 41 55 42 Z',
    rightInner: 'M58 38 C59 32 58 27 55 25 C52 23 50 27 50 33 C50 36 52 38 54 39 Z',
  },
  tall: {
    left: 'M27 42 C25 26 16 10 14 5 C13 1 25 2 30 16 C34 28 36 36 36 44 Z',
    leftInner: 'M27 40 C26 28 20 14 19 9 C20 8 26 11 28 22 C30 30 32 36 32 42 Z',
    right: 'M53 42 C55 26 64 10 66 5 C67 1 55 2 50 16 C46 28 44 36 44 44 Z',
    rightInner: 'M53 40 C54 28 60 14 61 9 C60 8 54 11 52 22 C50 30 48 36 48 42 Z',
  },
  tiny: {
    left: 'M30 32 C29 24 31 18 35 17 C39 16 41 22 39 32 Z',
    leftInner: 'M33 30 C33 25 34 22 36 22 C38 22 38 26 37 30 Z',
    right: 'M50 32 C51 24 49 18 45 17 C41 16 39 22 41 32 Z',
    rightInner: 'M47 30 C47 25 46 22 44 22 C42 22 42 26 43 30 Z',
  },
}

const FACES: Record<FaceShape, string> = {
  round: 'M40 28 C22 28 10 42 10 56 C10 70 23 78 40 78 C57 78 70 70 70 56 C70 42 58 28 40 28 Z',
  wide: 'M40 34 C18 32 6 46 6 58 C6 72 20 78 40 78 C60 78 74 72 74 58 C74 46 62 32 40 34 Z',
  wedge: 'M40 32 C24 32 13 40 12 52 C10 66 24 78 40 78 C56 78 70 66 68 52 C67 40 56 32 40 32 Z',
  cloud:
    'M40 26 C30 18 18 22 14 32 C8 30 5 40 9 52 C5 60 11 72 22 76 C28 80 34 74 40 78 C46 74 52 80 58 76 C69 72 75 60 71 52 C75 40 72 30 66 32 C62 22 50 18 40 26 Z',
}

/**
 * 品种只改轮廓和色块，眼睛、眼皮、耳朵的类名保持一致，
 * 这样一套 CSS 就能驱动八只猫，而不必为每只猫做精灵图。
 */
const BREEDS: Record<CatAvatarId, BreedLayout> = {
  bombay: {
    face: 'round',
    ears: 'medium',
    eyeOffset: 12,
    eyeCy: 50,
    eyeRx: 8.2,
    eyeRy: 9,
    mouth: 'smile',
    noseY: 60,
    markings: 'none',
    hasFluff: false,
    hasMuzzle: true,
  },
  'orange-tabby': {
    face: 'round',
    ears: 'medium',
    eyeOffset: 12,
    eyeCy: 49,
    eyeRx: 8.2,
    eyeRy: 9,
    mouth: 'open',
    noseY: 59,
    markings: 'tabby',
    hasFluff: false,
    hasMuzzle: true,
  },
  'british-shorthair': {
    face: 'wide',
    ears: 'small',
    eyeOffset: 14,
    eyeCy: 52,
    eyeRx: 8,
    eyeRy: 7.5,
    mouth: 'serious',
    noseY: 62,
    markings: 'none',
    hasFluff: false,
    hasMuzzle: true,
  },
  ragdoll: {
    face: 'round',
    ears: 'medium',
    eyeOffset: 11,
    eyeCy: 50,
    eyeRx: 8,
    eyeRy: 8.6,
    mouth: 'smile',
    noseY: 60,
    markings: 'ragdoll',
    hasFluff: true,
    hasMuzzle: false,
  },
  siamese: {
    face: 'wedge',
    ears: 'tall',
    eyeOffset: 11.5,
    eyeCy: 50,
    eyeRx: 8.5,
    eyeRy: 6.3,
    mouth: 'smile',
    noseY: 62,
    markings: 'siamese',
    hasFluff: false,
    hasMuzzle: false,
  },
  'american-shorthair': {
    face: 'round',
    ears: 'medium',
    eyeOffset: 12,
    eyeCy: 49,
    eyeRx: 8,
    eyeRy: 8.8,
    mouth: 'smile',
    noseY: 59,
    markings: 'tabby',
    hasFluff: false,
    hasMuzzle: true,
  },
  persian: {
    face: 'cloud',
    ears: 'tiny',
    eyeOffset: 7.5,
    eyeCy: 48,
    eyeRx: 7.1,
    eyeRy: 7.6,
    mouth: 'smile',
    noseY: 55,
    markings: 'none',
    hasFluff: true,
    hasMuzzle: false,
  },
  calico: {
    face: 'round',
    ears: 'medium',
    eyeOffset: 12,
    eyeCy: 49,
    eyeRx: 8.2,
    eyeRy: 9,
    mouth: 'open',
    noseY: 59,
    markings: 'calico',
    hasFluff: false,
    hasMuzzle: true,
  },
}

/** 矢量贴纸头像。部位拆开是为了让眼睛和耳朵能独立动，毛色仍来自固定调色板。 */
export const CatGraphic = memo(function CatGraphic({ preset }: { preset: CatAvatarId }) {
  const palette = getCatPalette(preset)
  const layout = BREEDS[preset]
  const gradientId = `cat${useId().replace(/:/g, '')}`
  const ears = EARS[layout.ears]
  return (
    <svg className="cat-graphic" viewBox="0 0 80 80" data-fur={palette.fur} aria-hidden="true">
      <defs>
        <radialGradient id={`${gradientId}-fur`} cx="42%" cy="36%" r="72%">
          <stop offset="0%" stopColor={palette.furLight} />
          <stop offset="100%" stopColor={palette.fur} />
        </radialGradient>
        <radialGradient id={`${gradientId}-iris`} cx="36%" cy="32%" r="72%">
          <stop offset="0%" stopColor={palette.irisLight} />
          <stop offset="58%" stopColor={palette.iris} />
          <stop offset="100%" stopColor={palette.irisDeep} />
        </radialGradient>
      </defs>
      <g className="cat-rig">
        <Ear
          side="left"
          outline={ears.left}
          inner={ears.leftInner}
          fill={palette.ear}
          innerFill={palette.earInner}
        />
        <Ear
          side="right"
          outline={ears.right}
          inner={ears.rightInner}
          fill={palette.earRight}
          innerFill={palette.earInner}
        />
        <g className="cat-face">
          <Sticker d={FACES[layout.face]} fill={`url(#${gradientId}-fur)`} />
          <ellipse cx="40" cy="67" rx="18" ry="8" fill={palette.shade} opacity="0.22" />
          <ellipse cx="40" cy="38" rx="12" ry="7" fill={palette.furLight} opacity="0.28" />
          <Markings kind={layout.markings} palette={palette} />
          {layout.hasMuzzle && <ellipse cx="40" cy="64" rx="16" ry="11" fill={palette.muzzle} />}
          {layout.hasFluff && <Fluff color={palette.shade} />}
        </g>
        <Eye
          cx={40 - layout.eyeOffset}
          cy={layout.eyeCy}
          rx={layout.eyeRx}
          ry={layout.eyeRy}
          lid={palette.lid}
          iris={`url(#${gradientId}-iris)`}
        />
        <Eye
          cx={40 + layout.eyeOffset}
          cy={layout.eyeCy}
          rx={layout.eyeRx}
          ry={layout.eyeRy}
          lid={palette.lidRight}
          iris={`url(#${gradientId}-iris)`}
        />
        <Nose fill={palette.nose} y={layout.noseY} />
        <Mouth shape={layout.mouth} color={palette.mouth} tongue={palette.tongue} />
        <Whiskers color={palette.whisker} y={layout.noseY + 2} />
      </g>
    </svg>
  )
})

function Sticker({ d, fill }: { d: string; fill: string }) {
  return (
    <g>
      <path
        d={d}
        fill={CAT_STICKER_RIM}
        stroke={CAT_STICKER_RIM}
        strokeWidth={4.2}
        strokeLinejoin="round"
      />
      <path d={d} fill={fill} stroke={CAT_CONTOUR} strokeWidth={1.15} strokeLinejoin="round" />
    </g>
  )
}

function Ear({
  side,
  outline,
  inner,
  fill,
  innerFill,
}: {
  side: 'left' | 'right'
  outline: string
  inner: string
  fill: string
  innerFill: string
}) {
  return (
    <g className={`cat-ear cat-ear-${side}`}>
      <Sticker d={outline} fill={fill} />
      <path d={inner} fill={innerFill} />
    </g>
  )
}

function Eye({
  cx,
  cy,
  rx,
  ry,
  lid,
  iris,
}: {
  cx: number
  cy: number
  rx: number
  ry: number
  lid: string
  iris: string
}) {
  return (
    <g className="cat-eye" transform={`translate(${cx} ${cy})`}>
      <ellipse className="cat-sclera" rx={rx} ry={ry} fill="#fffdf8" />
      <ellipse rx={rx * 0.78} ry={ry * 0.82} fill={iris} />
      <g className="cat-pupil">
        <ellipse cy={0.4} rx={rx * 0.34} ry={ry * 0.4} fill="#1c140e" />
        <circle cx={-rx * 0.16} cy={-ry * 0.2} r={Math.max(1.15, rx * 0.16)} fill="#fff" />
      </g>
      <path
        d={`M${-rx} ${-ry * 0.05} Q0 ${-ry} ${rx} ${-ry * 0.05}`}
        fill="none"
        stroke="#2a211c"
        strokeWidth="1.25"
        strokeLinecap="round"
      />
      <ellipse className="cat-lid" rx={rx + 0.7} ry={ry + 0.55} fill={lid} />
    </g>
  )
}

function Markings({ kind, palette }: { kind: Marking; palette: CatPalette }): ReactNode {
  if (kind === 'tabby') {
    return (
      <g className="cat-markings" fill={palette.marking}>
        <path d="M33 33 L37.2 41 L40 35 L42.8 41 L47 33 L44.7 33 L42.3 38.2 L40 34.2 L37.7 38.2 L35.3 33 Z" />
        <path d="M14 50 C22 46 28 49 32 53 C27 51 20 52 14 55 Z" />
        <path d="M66 50 C58 46 52 49 48 53 C53 51 60 52 66 55 Z" />
        <path d="M16 57 C24 55 30 57 33 60 C27 58 21 59 16 62 Z" />
        <path d="M64 57 C56 55 50 57 47 60 C53 58 59 59 64 62 Z" />
      </g>
    )
  }
  if (kind === 'ragdoll') {
    return (
      <g className="cat-markings">
        <path
          d="M12 48 C16 34 28 32 36 40 C34 54 26 66 16 68 C11 60 10 54 12 48 Z"
          fill={palette.marking}
        />
        <path
          d="M68 48 C64 34 52 32 44 40 C46 54 54 66 64 68 C69 60 70 54 68 48 Z"
          fill={palette.marking}
        />
        <path d="M40 30 L33 44 L36 58 L40 66 L44 58 L47 44 Z" fill={palette.muzzle} />
      </g>
    )
  }
  if (kind === 'siamese') {
    return (
      <path
        className="cat-markings"
        fill={palette.marking}
        fillRule="evenodd"
        d="M16 46 C18 36 28 34 40 37 C52 34 62 36 64 46 C66 60 56 75 40 76 C24 75 14 60 16 46 Z M40 33 L30 48 L40 56 L50 48 Z"
      />
    )
  }
  if (kind === 'calico') {
    return (
      <g className="cat-markings">
        <path d="M10 50 C12 34 24 28 38 34 L34 54 C26 50 16 56 10 50 Z" fill={palette.ear} />
        <path d="M70 48 C68 32 56 26 44 32 L48 50 C56 46 66 50 70 48 Z" fill={palette.earRight} />
        <path d="M14 62 C18 56 24 58 26 64 C22 66 16 66 14 62 Z" fill={palette.earRight} />
      </g>
    )
  }
  return null
}

function Fluff({ color }: { color: string }) {
  return (
    <g
      className="cat-fluff"
      stroke={color}
      strokeWidth="1.6"
      fill="none"
      strokeLinecap="round"
      opacity="0.7"
    >
      <path d="M16 58 Q24 72 34 76" />
      <path d="M22 64 Q32 78 42 78" />
      <path d="M64 58 Q56 72 46 76" />
      <path d="M58 64 Q48 78 38 78" />
    </g>
  )
}

function Nose({ fill, y }: { fill: string; y: number }) {
  return (
    <path
      className="cat-nose"
      fill={fill}
      d={`M40 ${y - 3.2} C37.4 ${y - 3.2} 36 ${y - 1} 36 ${y + 1.1} C36 ${y + 3.4} 38 ${y + 4.8} 40 ${y + 6.2} C42 ${y + 4.8} 44 ${y + 3.4} 44 ${y + 1.1} C44 ${y - 1} 42.6 ${y - 3.2} 40 ${y - 3.2} Z`}
    />
  )
}

function Mouth({ shape, color, tongue }: { shape: MouthShape; color: string; tongue: string }) {
  if (shape === 'open') {
    return (
      <g className="cat-mouth">
        <path d="M32 63.5 Q40 65.2 48 63.5 Q45 74 40 75 Q35 74 32 63.5 Z" fill={color} />
        <ellipse cx="40" cy="70.6" rx="3.3" ry="2.5" fill={tongue} />
      </g>
    )
  }
  const d = shape === 'serious' ? 'M33 67.5 Q40 64.8 47 67.5' : 'M32 66 Q40 72 48 66'
  return (
    <path
      className="cat-mouth"
      d={d}
      fill="none"
      stroke={color}
      strokeWidth="1.8"
      strokeLinecap="round"
    />
  )
}

function Whiskers({ color, y }: { color: string; y: number }) {
  return (
    <g
      className="cat-whiskers"
      stroke={color}
      strokeWidth="1.5"
      strokeLinecap="round"
      fill="none"
      opacity="0.9"
    >
      <path d={`M16 ${y - 4} Q26 ${y - 6} 33 ${y - 1}`} />
      <path d={`M15 ${y} Q26 ${y - 1} 33 ${y + 2}`} />
      <path d={`M17 ${y + 4} Q26 ${y + 3} 33 ${y + 5}`} />
      <path d={`M64 ${y - 4} Q54 ${y - 6} 47 ${y - 1}`} />
      <path d={`M65 ${y} Q54 ${y - 1} 47 ${y + 2}`} />
      <path d={`M63 ${y + 4} Q54 ${y + 3} 47 ${y + 5}`} />
    </g>
  )
}
