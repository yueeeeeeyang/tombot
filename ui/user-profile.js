// @ts-check
import { cropRectangle, loadAvatar } from './avatar-crop.js'
import { saveProfile } from './profile-save.js'

/** @template {Element} T
 * @param {string} selector
 * @param {{ new(): T }} kind
 * 资料模块只在个人信息页加载，缺少模板控件时明确报错。
 */
function control(selector, kind) {
  const element = document.querySelector(selector)
  if (!(element instanceof kind)) throw new Error('个人信息页面控件缺失')
  return element
}

const form = control('#profile-form', HTMLFormElement)
const csrf = control('#profile-form input[name="csrf"]', HTMLInputElement).value
const nickname = control('#nickname-input', HTMLInputElement)
const nicknameDisplay = control('#nickname-display', HTMLHeadingElement)
const nicknameEditor = control('#nickname-editor', HTMLDivElement)
const nicknameEdit = control('#nickname-edit', HTMLButtonElement)
const nicknameCancel = control('#nickname-cancel', HTMLButtonElement)
const nicknameError = control('#nickname-error', HTMLParagraphElement)
const status = control('#profile-edit-status', HTMLParagraphElement)
let savedNickname = nickname.value
let isEditingNickname = false
let isSavingNickname = false
let isComposing = false

function showStatus(/** @type {string} */ message) {
  status.textContent = message
  status.hidden = !message
}
function finishNickname(/** @type {boolean} */ focus) {
  isEditingNickname = false
  nickname.hidden = true
  nicknameDisplay.hidden = false
  nicknameEdit.hidden = false
  nicknameCancel.hidden = true
  nickname.value = savedNickname
  nickname.setCustomValidity('')
  nickname.removeAttribute('aria-invalid')
  nicknameError.hidden = true
  if (focus) nicknameEdit.focus()
}
function editNickname() {
  isEditingNickname = true
  nickname.hidden = false
  nickname.focus()
  nickname.select()
  nicknameDisplay.hidden = true
  nicknameEdit.hidden = true
  nicknameCancel.hidden = false
}

async function saveNickname() {
  if (!isEditingNickname || isSavingNickname || isComposing) return
  const next = nickname.value.trim()
  if (!next || !nickname.checkValidity()) {
    nicknameError.textContent = '昵称须为 1–120 个字符，不能为空。'
    nicknameError.hidden = false
    nickname.setAttribute('aria-invalid', 'true')
    return
  }
  if (next === savedNickname) {
    finishNickname(document.activeElement === nickname)
    return
  }
  isSavingNickname = true
  nickname.readOnly = true
  nicknameCancel.disabled = true
  nicknameError.hidden = true
  showStatus('正在保存昵称…')
  try {
    const result = await saveProfile(form.action, csrf, { displayName: next })
    savedNickname = result.displayName
    nicknameDisplay.textContent = savedNickname
    const identity = control('.account-identity strong', HTMLElement)
    identity.textContent = savedNickname
    identity.title = savedNickname
    control('.account-trigger', HTMLElement).setAttribute(
      'aria-label',
      '账户菜单：' + savedNickname + '（' + form.dataset.username + '）',
    )
    finishNickname(document.activeElement === nickname)
    showStatus('昵称已保存。')
  } catch (error) {
    nicknameError.textContent = error instanceof Error ? error.message : '昵称保存失败，请重试。'
    nicknameError.hidden = false
    showStatus('')
  } finally {
    isSavingNickname = false
    nickname.readOnly = false
    nicknameCancel.disabled = false
  }
}

nicknameEdit.addEventListener('click', editNickname)
nicknameCancel.addEventListener('click', () => finishNickname(true))
nickname.addEventListener('input', () => {
  nickname.setCustomValidity(nickname.value.trim() ? '' : '昵称不能为空。')
  nickname.removeAttribute('aria-invalid')
  nicknameError.hidden = true
})
nickname.addEventListener('compositionstart', () => {
  isComposing = true
})
nickname.addEventListener('compositionend', () => {
  isComposing = false
  if (!nicknameEditor.contains(document.activeElement)) void saveNickname()
})
nicknameEditor.addEventListener('focusout', (event) => {
  if (event.relatedTarget instanceof Node && nicknameEditor.contains(event.relatedTarget)) return
  void saveNickname()
})
nickname.addEventListener('keydown', (event) => {
  if (event.isComposing || isComposing) return
  if (event.key === 'Escape' && !isSavingNickname) {
    event.preventDefault()
    finishNickname(true)
  } else if (event.key === 'Enter') {
    event.preventDefault()
    void saveNickname()
  }
})
form.addEventListener('submit', (event) => {
  event.preventDefault()
  void saveNickname()
})

const avatarEdit = control('#avatar-edit', HTMLButtonElement)
const source = control('#avatar-source', HTMLInputElement)
const currentAvatar = control('#current-avatar', HTMLImageElement)
const dialog = control('#avatar-crop-dialog', HTMLDialogElement)
const canvas = control('#crop-canvas', HTMLCanvasElement)
const zoom = control('#crop-zoom', HTMLInputElement)
const confirm = control('#crop-confirm', HTMLButtonElement)
const cancel = control('#crop-cancel', HTMLButtonElement)
const cropStatus = control('#crop-status', HTMLParagraphElement)
const cropError = control('#crop-error', HTMLParagraphElement)
const cropContext = canvas.getContext('2d')
if (!cropContext) throw new Error('浏览器不支持头像裁剪')

/** @type {ImageBitmap | undefined} */
let picture
let centerX = 0
let centerY = 0
let generation = 0
let isEncoding = false
let isSavingAvatar = false
/** @type {{ id: number, x: number, y: number } | undefined} */
let drag

function renderCrop() {
  if (!picture || !cropContext) return
  const crop = cropRectangle(picture.width, picture.height, Number(zoom.value), centerX, centerY)
  centerX = crop.centerX
  centerY = crop.centerY
  cropContext.clearRect(0, 0, canvas.width, canvas.height)
  cropContext.drawImage(
    picture,
    crop.x,
    crop.y,
    crop.size,
    crop.size,
    0,
    0,
    canvas.width,
    canvas.height,
  )
}

function releaseCrop() {
  generation++
  picture?.close()
  picture = undefined
  drag = undefined
  isEncoding = false
  isSavingAvatar = false
  cancel.disabled = false
  confirm.textContent = '确认更换'
  confirm.disabled = true
  zoom.disabled = true
}

function cropFailed(/** @type {string} */ message) {
  cropError.textContent = message
  cropError.hidden = false
  cropStatus.textContent = ''
}

avatarEdit.addEventListener('click', () => source.click())
source.addEventListener('change', async () => {
  const file = source.files?.[0]
  source.value = ''
  if (!file) return
  releaseCrop()
  const current = generation
  cropContext.clearRect(0, 0, canvas.width, canvas.height)
  cropError.hidden = true
  cropStatus.textContent = '正在读取图片…'
  zoom.value = '1'
  dialog.showModal()
  try {
    const loaded = await loadAvatar(file)
    // 取消期间完成的解码不能重新打开弹窗或覆盖另一张头像。
    if (generation !== current) {
      loaded.close()
      return
    }
    picture = loaded
    centerX = loaded.width / 2
    centerY = loaded.height / 2
    renderCrop()
    cropStatus.textContent = '点击“确认更换”立即保存头像。'
    confirm.disabled = false
    zoom.disabled = false
    canvas.focus()
  } catch (error) {
    if (generation === current)
      cropFailed(error instanceof Error ? error.message : '图片读取失败，请重新选择。')
  }
})
cancel.addEventListener('click', () => {
  if (isSavingAvatar) return
  releaseCrop()
  dialog.close()
})
dialog.addEventListener('cancel', (event) => {
  if (isSavingAvatar) event.preventDefault()
  else releaseCrop()
})
dialog.addEventListener('close', () => {
  releaseCrop()
  avatarEdit.focus()
})
window.addEventListener('pagehide', releaseCrop)
zoom.addEventListener('input', renderCrop)

canvas.addEventListener('pointerdown', (event) => {
  if (!picture || isEncoding || drag || event.button !== 0) return
  event.preventDefault()
  canvas.focus()
  drag = { id: event.pointerId, x: event.clientX, y: event.clientY }
  canvas.setPointerCapture(event.pointerId)
})
canvas.addEventListener('pointermove', (event) => {
  if (!picture || isEncoding || drag?.id !== event.pointerId) return
  const bounds = canvas.getBoundingClientRect()
  const size = Math.min(picture.width, picture.height) / Number(zoom.value)
  centerX -= ((event.clientX - drag.x) * size) / bounds.width
  centerY -= ((event.clientY - drag.y) * size) / bounds.height
  drag = { id: event.pointerId, x: event.clientX, y: event.clientY }
  renderCrop()
})
function endDrag(/** @type {PointerEvent} */ event) {
  if (drag?.id === event.pointerId) drag = undefined
}
canvas.addEventListener('pointerup', endDrag)
canvas.addEventListener('pointercancel', endDrag)
canvas.addEventListener('lostpointercapture', endDrag)
canvas.addEventListener('keydown', (event) => {
  if (
    !picture ||
    isEncoding ||
    !['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown'].includes(event.key)
  )
    return
  event.preventDefault()
  const step = Math.min(picture.width, picture.height) / Number(zoom.value) / 20
  if (event.key === 'ArrowLeft') centerX += step
  if (event.key === 'ArrowRight') centerX -= step
  if (event.key === 'ArrowUp') centerY += step
  if (event.key === 'ArrowDown') centerY -= step
  renderCrop()
})

confirm.addEventListener('click', async () => {
  if (!picture || isEncoding) return
  isEncoding = true
  confirm.disabled = true
  zoom.disabled = true
  cropError.hidden = true
  cropStatus.textContent = '正在生成头像…'
  const current = generation
  try {
    const blob = await new Promise((resolve) => canvas.toBlob(resolve, 'image/png'))
    if (current !== generation) return
    if (!(blob instanceof Blob) || blob.size > 2 * 1024 * 1024)
      throw new Error('头像生成失败，请重新裁剪。')
    isSavingAvatar = true
    cancel.disabled = true
    confirm.textContent = '正在更换…'
    cropStatus.textContent = '正在保存头像…'
    await saveProfile(form.action, csrf, { avatar: blob })
    if (current !== generation) return
    const avatarURL = '/api/v2/web/auth/avatar?v=' + crypto.randomUUID()
    currentAvatar.src = avatarURL
    control('.account-trigger img', HTMLImageElement).src = avatarURL
    showStatus('头像已更换。')
    dialog.close()
  } catch (error) {
    if (current !== generation) return
    cropFailed(error instanceof Error ? error.message : '头像更换失败，请重试。')
  } finally {
    // 旧图片的编码完成不能解除新一次上传的交互锁。
    if (current === generation) {
      isSavingAvatar = false
      cancel.disabled = false
      confirm.textContent = '确认更换'
      isEncoding = false
      confirm.disabled = false
      zoom.disabled = false
    }
  }
})

currentAvatar.addEventListener('error', () => {
  showStatus('头像无法显示，请刷新页面重试。')
})
