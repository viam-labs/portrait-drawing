// Machine connection and the polled state the dashboard renders.
import { CameraClient, createRobotClient, type RobotClient } from '@viamrobotics/sdk'
import { GenericServiceClient } from '@viamrobotics/sdk'
import type { MachineTarget } from './machine'

export interface DrawerStatus {
  state: string
  drawing: boolean
  percent?: number
  polylines_done?: number
  polylines_total?: number
  points_total?: number
  elapsed_sec?: number
  last_error?: string
}

export async function connect(target: MachineTarget): Promise<RobotClient> {
  return createRobotClient({
    host: target.host,
    signalingAddress: 'https://app.viam.com:443',
    credentials: {
      type: 'api-key',
      authEntity: target.apiKeyId,
      payload: target.apiKeySecret,
    },
  })
}

/** Latest frame from a camera as an object URL, or null when it holds nothing. */
export async function frameURL(client: RobotClient, name: string): Promise<string | null> {
  const camera = new CameraClient(client, name)
  // frame-buffer serves an error until something is latched, which is a normal
  // state — before the first capture, and after any module restart.
  const { images } = await camera.getImages()
  const picture = images.find((i) => i.mimeType !== 'image/vnd.viam.dep')
  if (!picture) return null
  return URL.createObjectURL(new Blob([picture.image as BlobPart], { type: picture.mimeType }))
}

export async function drawerStatus(client: RobotClient, name: string): Promise<DrawerStatus> {
  const generic = new GenericServiceClient(client, name)
  return (await generic.doCommand({ status: {} })) as unknown as DrawerStatus
}
