/**
 * 宿主机 WebSocket 反向隧道多路复用轻量二进制帧协议
 *
 * 帧格式（5 字节头部 + 变长 Payload）：
 * +---------------+-------------------+-----------------------+
 * | Type (1 字节) | Stream ID (4 字节) | Payload (0 ~ N 字节)  |
 * +---------------+-------------------+-----------------------+
 */

export enum TunnelFrameType {
  OPEN = 0x01,   // 新建连接流（Server -> Agent），Payload 为 JSON { targetHost, targetPort }
  DATA = 0x02,   // 传输数据（双向），Payload 为数据片段（最大 64KB）
  CLOSE = 0x03,  // 正常关闭流（双向，发送端结束发送 / EOF）
  RESET = 0x04,  // 异常重置流（双向，连接中断或错误）
  CONFIG = 0x05, // 控制指令（Server -> Agent），StreamID = 0，Payload 为 JSON 配置
  LOG_CTL = 0x06,  // 日志流控制（Server -> Agent），StreamID = 0，Payload 为 JSON { action: 'start' | 'stop', lines?: number }
  LOG_DATA = 0x07, // 日志流数据（Agent -> Server），StreamID = 0，Payload 为一行 UTF-8 文本；空 Payload 表示流结束
}

export interface DecodedFrame {
  type: TunnelFrameType
  streamId: number
  payload: Buffer
}

/**
 * 编码隧道帧
 */
export function encodeFrame(
  type: TunnelFrameType,
  streamId: number,
  payload?: Buffer | Uint8Array | null
): Buffer {
  const payloadLength = payload ? payload.length : 0
  const buf = Buffer.allocUnsafe(5 + payloadLength)
  buf.writeUInt8(type, 0)
  buf.writeUInt32BE(streamId >>> 0, 1)
  if (payload && payloadLength > 0) {
    if (Buffer.isBuffer(payload)) {
      payload.copy(buf, 5)
    } else {
      buf.set(payload, 5)
    }
  }
  return buf
}

/**
 * 解码隧道帧
 */
export function decodeFrame(buf: Buffer): DecodedFrame {
  if (buf.length < 5) {
    throw new Error(`Frame too short: expected at least 5 bytes, got ${buf.length}`)
  }
  const type = buf.readUInt8(0) as TunnelFrameType
  const streamId = buf.readUInt32BE(1)
  const payload = buf.subarray(5)
  return { type, streamId, payload }
}
