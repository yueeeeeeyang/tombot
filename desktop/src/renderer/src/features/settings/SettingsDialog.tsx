import { useEffect, useState } from 'react'
import { getBackend, type ApplicationAbout } from '../desktop/backend'
import { UpdatePanel } from './ClientUpdates'
import { Download, Settings, X } from 'lucide-react'
import { Dialog } from '../../components/Dialog'
import { Select } from '../../../../../../ui/Select'
import workspaceCat from '../../assets/cats/workspace-cat.png'
import type { UserProfile } from '../desktop/backend'
import { type LanguagePreference, type ThemePreference } from './settings'

interface SettingsDialogProps {
  theme: ThemePreference
  themeError: string
  onThemeChange: (theme: ThemePreference) => void
  language: LanguagePreference
  languageError: string
  onLanguageChange: (language: LanguagePreference) => void
  userProfile?: UserProfile
  profileError: string
  isSigningOut: boolean
  signOutError: string
  onSignOut: () => Promise<void>
  onClose: () => void
}

/** 账号资料来自当前会话；主题即时生效，外观偏好保存在本机。 */
export function SettingsDialog({
  theme,
  themeError,
  languageError,
  onThemeChange,
  language,
  onLanguageChange,
  userProfile,
  profileError,
  isSigningOut,
  signOutError,
  onSignOut,
  onClose,
}: SettingsDialogProps) {
  const [section, setSection] = useState<'general' | 'updates'>('general')
  return (
    <Dialog title="设置" variant="settings" onClose={onClose}>
      <div className="settings-layout">
        <nav className="settings-navigation" aria-label="设置分类">
          <button
            type="button"
            className={section === 'general' ? 'is-active' : ''}
            aria-current={section === 'general' ? 'page' : undefined}
            onClick={() => setSection('general')}
          >
            <Settings size={17} strokeWidth={1.7} />
            通用
          </button>
          <button
            type="button"
            className={section === 'updates' ? 'is-active' : ''}
            aria-current={section === 'updates' ? 'page' : undefined}
            onClick={() => setSection('updates')}
          >
            <Download size={17} strokeWidth={1.7} />
            更新
          </button>
        </nav>
        <section className="settings-pane" aria-labelledby="settings-section-title">
          {section === 'updates' ? (
            <UpdateSettings />
          ) : (
            <>
              <h2 id="settings-section-title">通用</h2>
              {(profileError || signOutError || themeError || languageError) && (
                <div className="settings-errors">
                  {profileError && <SettingsError message={profileError} />}
                  {signOutError && <SettingsError message={signOutError} />}
                  {themeError && <SettingsError message={themeError} />}
                  {languageError && <SettingsError message={languageError} />}
                </div>
              )}
              <section className="settings-section" aria-labelledby="settings-account-title">
                <h3 id="settings-account-title">账号</h3>
                <div className="settings-card settings-account">
                  <div className="settings-account-identity">
                    <img src={userProfile?.avatar || workspaceCat} alt="" draggable={false} />
                    <div className="settings-account-copy">
                      <strong>
                        {userProfile?.displayName || userProfile?.username || '账号信息'}
                      </strong>
                      <span>
                        {userProfile?.username ||
                          (profileError ? '暂时无法读取账号信息' : '正在读取账号信息…')}
                      </span>
                    </div>
                  </div>
                  <button
                    type="button"
                    className="settings-sign-out"
                    disabled={isSigningOut}
                    onClick={() => void onSignOut()}
                  >
                    {isSigningOut ? '正在退出…' : '退出登录'}
                  </button>
                </div>
              </section>
              <GeneralSettings
                language={language}
                onLanguageChange={onLanguageChange}
                theme={theme}
                onThemeChange={onThemeChange}
              />
              <p className="settings-preview-note">
                主题即时生效。语言选择保存在本机，界面仍使用简体中文
              </p>
            </>
          )}
        </section>
        <button className="icon-button settings-close" aria-label="关闭设置" onClick={onClose}>
          <X size={18} strokeWidth={1.5} />
        </button>
      </div>
    </Dialog>
  )
}

/** 设置错误集中置于标题下方；重试入口与对应原因保持在同一条提示内。 */
function SettingsError({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div role="alert" className="form-error settings-error">
      <span>{message}</span>
      {onRetry && (
        <button type="button" aria-label="重试读取版本" onClick={onRetry}>
          重试
        </button>
      )}
    </div>
  )
}

/** 主题通过独立回调应用和保存，语言选项保留为界面预览。 */
function GeneralSettings({
  language,
  onLanguageChange,
  theme,
  onThemeChange,
}: Pick<SettingsDialogProps, 'language' | 'onLanguageChange' | 'theme' | 'onThemeChange'>) {
  return (
    <div className="settings-section">
      <h3>外观</h3>
      <div className="settings-card">
        <div className="setting-row">
          <span>主题</span>
          <Select
            label="主题"
            value={theme}
            onChange={(value) => onThemeChange(value)}
            options={[
              { value: 'system', label: '跟随系统' },
              { value: 'light', label: '浅色' },
              { value: 'dark', label: '深色' },
            ]}
          />
        </div>
        <div className="setting-row">
          <span>语言</span>
          <Select
            label="语言"
            value={language}
            onChange={onLanguageChange}
            options={[
              { value: 'system', label: '跟随系统' },
              { value: 'zh-CN', label: '简体中文' },
              { value: 'en', label: '英语' },
            ]}
          />
        </div>
      </div>
    </div>
  )
}

/** 更新页展示主进程与当前连接的实际版本；失败提供重试，不用占位版本掩盖错误。 */
function UpdateSettings() {
  const [about, setAbout] = useState<ApplicationAbout>()
  const [error, setError] = useState('')
  const [attempt, setAttempt] = useState(0)
  useEffect(() => {
    let cancelled = false
    void getBackend()
      .loadAbout()
      .then(
        (result) => {
          if (!cancelled) {
            setAbout(result)
            setError(result.serverError ?? '')
          }
        },
        (failure: Error) => {
          if (!cancelled) setError(failure.message)
        },
      )
    return () => {
      cancelled = true
    }
  }, [attempt])
  return (
    <>
      <h2 id="settings-section-title">更新</h2>
      {error && (
        <div className="settings-errors">
          <SettingsError
            message={error}
            onRetry={() => {
              setError('')
              setAttempt((value) => value + 1)
            }}
          />
        </div>
      )}
      <div className="settings-card settings-versions">
        <div className="setting-row">
          <span>客户端版本</span>
          <span>{about?.clientVersion ?? '正在读取…'}</span>
        </div>
        <div className="setting-row">
          <span>服务端地址</span>
          <span>{about?.serverUrl || '正在读取…'}</span>
        </div>
        <div className="setting-row">
          <span>服务端版本</span>
          <span>{about?.serverVersion || (error ? '暂时无法获取' : '正在读取…')}</span>
        </div>
      </div>
      <UpdatePanel />
    </>
  )
}
