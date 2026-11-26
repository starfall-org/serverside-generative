import React from 'react'
import { createRoot } from 'react-dom/client'
import { PreferencesProvider } from './app/preferences'
import { App } from './app/App'
import { ConnectionProvider } from './features/connection/ConnectionProvider'
import { TooltipProvider } from './components/ui/tooltip'
import './styles/app.css'

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <PreferencesProvider>
      <TooltipProvider delayDuration={350}>
        <ConnectionProvider>
          <App />
        </ConnectionProvider>
      </TooltipProvider>
    </PreferencesProvider>
  </React.StrictMode>,
)
