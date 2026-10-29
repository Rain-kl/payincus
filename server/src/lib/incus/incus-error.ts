// A certificate pin authenticates the node; it does not imply that node security is locked.
export class IncusApiError extends Error {
  readonly statusCode: number
  constructor(message: string, statusCode: number, body?: unknown) {
    super(redactNodeText(message, body).slice(0, 4096))
    this.name = 'IncusApiError'
    this.statusCode = statusCode >= 400 && statusCode <= 599 ? statusCode : 502
  }
}

export function nodeErrorDetail(error: unknown, fallback: string, body?: unknown): string {
  const message = error instanceof Error ? error.message : fallback
  return new IncusApiError(message || fallback, 500, body).message
}

// Redact the original request before truncating any error or progress text.
export function redactNodeText(message: string, body?: unknown): string {
  let detail = message
  const redact = (value: unknown, depth: number) => {
    if (!value || typeof value !== 'object' || depth > 5) return
    for (const [name, item] of Object.entries(value)) {
      if (/key|password|secret|token|credential/i.test(name) && typeof item === 'string' && item) {
        for (const candidate of [item, encodeURIComponent(item), JSON.stringify(item).slice(1, -1)]) detail = detail.split(candidate).join('[REDACTED]')
      } else if (item && typeof item === 'object') redact(item, depth + 1)
    }
  }
  redact(body, 0)
  return detail
}

export function operationError(error: unknown, body?: unknown): Error {
  if (error instanceof Error && ['AbortError', 'TimeoutError'].includes(error.name)) {
    return new DOMException(redactNodeText(error.message, body).slice(0, 4096), error.name)
  }
  return new IncusApiError(error instanceof Error ? error.message : '节点操作失败', error instanceof IncusApiError ? error.statusCode : 502, body)
}
