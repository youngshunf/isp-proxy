import '@isp/ui/fonts'
import '@isp/ui/styles.css'
import './styles/global.css'

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { createApp } from './app/createApp'

const { Root } = createApp()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
)
