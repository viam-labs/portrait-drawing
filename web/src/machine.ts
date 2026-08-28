// Finding the machine this application was opened for.
//
// Shapes follow the RDK's own app template; they are not guessable, and getting
// them wrong looks exactly like a machine being offline. In order of preference:
//
//  1. host / api-key-id / api-key cookies, set by `viam module local-app-testing`
//  2. the per-machine cookie viamapplications.com sets, named by the machine id
//     in the /machine/<id>/... path
//  3. web/.env.local, for development against a real machine
import { getCookie } from 'typescript-cookie'

export type CredentialSource = 'local-app-testing' | 'viam' | 'env'

export interface MachineTarget {
  host: string
  apiKeyId: string
  apiKeySecret: string
  source: CredentialSource
}

export const SIGNALING_ADDRESS = 'https://app.viam.com:443'

function fromTestingCookies(): MachineTarget | null {
  const host = getCookie('host')
  const apiKeyId = getCookie('api-key-id')
  const apiKeySecret = getCookie('api-key')
  if (!host || !apiKeyId || !apiKeySecret) return null
  return { host, apiKeyId, apiKeySecret, source: 'local-app-testing' }
}

function fromViam(): MachineTarget | null {
  // The path is /machine/<id>/..., and the cookie is named by that id.
  const parts = window.location.pathname.split('/')
  if (parts.length < 3 || parts[1] !== 'machine') return null
  const raw = getCookie(parts[2])
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw)
    const host = parsed?.hostname
    const apiKeyId = parsed?.apiKey?.id
    const apiKeySecret = parsed?.apiKey?.key
    if (!host || !apiKeyId || !apiKeySecret) return null
    return { host, apiKeyId, apiKeySecret, source: 'viam' }
  } catch {
    return null
  }
}

// Development only. Vite inlines env vars at build time, so this is guarded by
// import.meta.env.DEV: the branch is dropped from a production bundle entirely,
// and the values are never inlined into one.
function fromEnvFile(): MachineTarget | null {
  if (!import.meta.env.DEV) return null
  const host = import.meta.env.VITE_MACHINE_HOST
  const apiKeyId = import.meta.env.VITE_API_KEY_ID
  const apiKeySecret = import.meta.env.VITE_API_KEY
  if (!host || !apiKeyId || !apiKeySecret) return null
  return { host, apiKeyId, apiKeySecret, source: 'env' }
}

export function machineTarget(): MachineTarget | null {
  return fromTestingCookies() ?? fromViam() ?? fromEnvFile()
}

/** What the page looked for, for when it found nothing. */
export function lookedFor(): string {
  const parts = window.location.pathname.split('/')
  return parts.length >= 3 && parts[1] === 'machine'
    ? `a cookie named "${parts[2]}" for the machine in this URL`
    : `a /machine/<id>/ path (this page is at "${window.location.pathname}")`
}
