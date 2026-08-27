// Connection to the machine this application was opened for.
//
// A single_machine application is served with the machine's hostname in the
// query string and credentials in cookies, both set by app.viam.com when the
// viewer picks a machine. Nothing here prompts for or stores credentials
// itself.
import { getCookie } from 'typescript-cookie'

export interface MachineTarget {
  host: string
  id: string
  apiKeyId: string
  apiKeySecret: string
}

/** Reads the machine and credentials Viam supplies, or null when opened outside it. */
export function machineTarget(): MachineTarget | null {
  const host = new URLSearchParams(window.location.search).get('machineId')
    ?? window.location.pathname.split('/').filter(Boolean).pop()
    ?? ''
  if (!host) return null

  const raw = getCookie(host)
  if (!raw) return null
  try {
    const { hostname, id, key, key_id: keyId } = JSON.parse(raw)
    return {
      host: hostname ?? host,
      id: id ?? host,
      apiKeyId: keyId ?? '',
      apiKeySecret: key ?? '',
    }
  } catch {
    return null
  }
}
