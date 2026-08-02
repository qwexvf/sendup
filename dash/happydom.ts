import { GlobalRegistrator } from '@happy-dom/global-registrator'

GlobalRegistrator.register({ url: 'https://dash.example.com/' })
;(globalThis as any).__SENDUP_BASE__ = 'example.com'
// lets React's act() flush effects without warning
;(globalThis as any).IS_REACT_ACT_ENVIRONMENT = true
