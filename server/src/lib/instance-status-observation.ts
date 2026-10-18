/** Single-backend observation window: a VM reboot can briefly report Error. */
export class InstanceStatusObservations {
  private errors = new Map<number, { first: number; last: number }>()

  shouldApply(id: number, current: string, observed: string, now = Date.now()): boolean {
    if (!['running', 'stopped', 'error'].includes(current) ||
        !['running', 'stopped', 'error'].includes(observed)) {
      this.errors.delete(id)
      return false
    }
    if (observed !== 'error' || current === 'error') {
      this.errors.delete(id)
      return true
    }
    // Expire old observations and bound memory even if instances disappear.
    for (const [key, value] of this.errors) {
      if (now - value.last > 15 * 60_000) this.errors.delete(key)
    }
    const previous = this.errors.get(id)
    if (!previous) {
      if (this.errors.size >= 10_000) this.errors.delete(this.errors.keys().next().value!)
      this.errors.set(id, { first: now, last: now })
      return false
    }
    previous.last = now
    return now - previous.first >= 30_000
  }

  forget(id: number): void {
    this.errors.delete(id)
  }
}

export const instanceStatusObservations = new InstanceStatusObservations()
