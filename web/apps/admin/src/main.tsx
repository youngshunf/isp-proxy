import '@isp/ui/fonts'
import '@isp/ui/styles.css'
import './styles/admin.css'

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { createDefaultApi } from './app/api'
import { createApp } from './app/createApp'

const { api, now } = createDefaultApi()
const { Root } = createApp({ api, now })

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
)
