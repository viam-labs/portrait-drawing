// Connection to the machine this application was opened for.
//
// A single_machine application is served with the machine's hostname in the URL
// and credentials in cookies, both set by app.viam.com when the viewer picks a
// machine. Nothing here prompts for or stores credentials itself.
import { getCookie } from 'typescript-cookie'

export interface MachineTarget {
  host: string
  apiKeyId: string
  apiKeySecret: string
  /** True when the credentials came from .env.local rather than from Viam. */
  fromEnv: boolean
}

function machineHost(): string {
  const params = new URLSearchParams(window.location.search)
  return (
    params.get('machineId') ??
    params.get('machine') ??
    window.location.pathname.split('/').filter(Boolean).pop() ??
    ''
  )
}

function fromViam(): MachineTarget | null {
  const host = machineHost()
  if (!host) return null
  const raw = getCookie(host)
  if (!raw) return null
  try {
    const { hostname, key, key_id: keyId } = JSON.parse(raw)
    if (!key || !keyId) return null
    return { host: hostname ?? host, apiKeyId: keyId, apiKeySecret: key, fromEnv: false }
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
  return { host, apiKeyId, apiKeySecret, fromEnv: true }
}

/**
 * The machine to connect to: whichever Viam supplied, or the one named in
 * web/.env.local when developing locally. Null when neither is available.
 */
export function machineTarget(): MachineTarget | null {
  return fromViam() ?? fromEnvFile()
}
