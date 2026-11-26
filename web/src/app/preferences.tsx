import * as React from 'react'
import { defaultPreferences, readPreferences, writePreferences, type Preferences } from '../lib/storage'
import { en, vi, type MessageKey } from '../i18n/strings'

interface PreferencesContextValue {
  preferences: Preferences
  setTheme: (theme: Preferences['theme']) => void
  language: 'vi' | 'en'
  setLanguage: (language: 'vi' | 'en') => void
  setDefaultModel: (model: Preferences['model']) => void
  t: (key: MessageKey, vars?: Record<string, string | number>) => string
  locale: string
}

const PreferencesContext = React.createContext<PreferencesContextValue | null>(null)

function resolveTheme(theme: Preferences['theme']): 'light' | 'dark' {
  if (theme !== 'system') return theme
  return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

function browserLanguage(): 'vi' | 'en' {
  const preferred = navigator.languages?.length ? navigator.languages : [navigator.language]
  for (const tag of preferred) {
    const language = tag?.toLowerCase().split('-')[0]
    if (language === 'vi' || language === 'en') return language
  }
  return 'en'
}

export function PreferencesProvider({ children }: { children: React.ReactNode }) {
  const [preferences, setPreferences] = React.useState<Preferences>(() => readPreferences())

  const [automaticLanguage, setAutomaticLanguage] = React.useState(browserLanguage)
  const language = preferences.language === 'auto' ? automaticLanguage : preferences.language

  React.useEffect(() => {
    const updateLanguage = () => setAutomaticLanguage(browserLanguage())
    window.addEventListener('languagechange', updateLanguage)
    return () => window.removeEventListener('languagechange', updateLanguage)
  }, [])

  React.useEffect(() => {
    document.documentElement.lang = language
    document.title = (language === 'vi' ? vi : en)['app.title']
  }, [language])

  React.useEffect(() => {
    writePreferences(preferences)
    const resolved = resolveTheme(preferences.theme)
    document.documentElement.dataset.theme = resolved
    document.documentElement.style.colorScheme = resolved
  }, [preferences])

  React.useEffect(() => {
    if (preferences.theme !== 'system' || !window.matchMedia) return
    const media = window.matchMedia('(prefers-color-scheme: dark)')
    const onChange = () => {
      const resolved = media.matches ? 'dark' : 'light'
      document.documentElement.dataset.theme = resolved
      document.documentElement.style.colorScheme = resolved
    }
    media.addEventListener('change', onChange)
    return () => media.removeEventListener('change', onChange)
  }, [preferences.theme])

  const setTheme = React.useCallback((theme: Preferences['theme']) => {
    setPreferences((current) => ({ ...current, theme }))
  }, [])
  const setLanguage = React.useCallback((language: 'vi' | 'en') => {
    setPreferences((current) => ({ ...current, language }))
  }, [])
  const setDefaultModel = React.useCallback((model: Preferences['model']) => {
    setPreferences((current) => ({ ...current, model }))
  }, [])

  const t = React.useCallback(
    (key: MessageKey, vars?: Record<string, string | number>) => {
      const dictionary = language === 'vi' ? vi : en
      let message = dictionary[key] ?? en[key] ?? key
      if (vars) {
        for (const [name, value] of Object.entries(vars)) {
          message = message.replaceAll(`{${name}}`, String(value))
        }
      }
      return message
    },
    [language],
  )

  const value = React.useMemo<PreferencesContextValue>(
    () => ({
      preferences,
      setTheme,
      language,
      setLanguage,
      setDefaultModel,
      t,
      locale: language === 'vi' ? 'vi-VN' : 'en-US',
    }),
    [preferences, setTheme, setLanguage, setDefaultModel, t, language],
  )

  return <PreferencesContext.Provider value={value}>{children}</PreferencesContext.Provider>
}

export function usePreferences(): PreferencesContextValue {
  const context = React.useContext(PreferencesContext)
  if (!context) throw new Error('usePreferences must be used inside PreferencesProvider')
  return context
}

export { defaultPreferences }
