import { Settings, X } from 'lucide-react'
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
  return (
    <Dialog title="设置" variant="settings" onClose={onClose}>
      <div className="settings-layout">
        <nav className="settings-navigation" aria-label="设置分类">
          <button type="button" className="is-active" aria-current="page">
            <Settings size={17} strokeWidth={1.7} />
            通用
          </button>
        </nav>
        <section className="settings-pane" aria-labelledby="settings-section-title">
          <h2 id="settings-section-title">通用</h2>
          <section className="settings-section" aria-labelledby="settings-account-title">
            <h3 id="settings-account-title">账号</h3>
            <div className="settings-card settings-account">
              <div className="settings-account-identity">
                <img src={userProfile?.avatar || workspaceCat} alt="" draggable={false} />
                <div className="settings-account-copy">
                  <strong>{userProfile?.displayName || userProfile?.username || '账号信息'}</strong>
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
            {(profileError || signOutError) && (
              <p className="form-error" role="alert">
                {signOutError || profileError}
              </p>
            )}
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
          {(themeError || languageError) && (
            <p className="form-error" role="alert">
              {themeError || languageError}
            </p>
          )}
        </section>
        <button className="icon-button settings-close" aria-label="关闭设置" onClick={onClose}>
          <X size={18} strokeWidth={1.5} />
        </button>
      </div>
    </Dialog>
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
