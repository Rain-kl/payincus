/**
 * OCI Drawer 移动端右滑关闭手势监听引擎
 * -------------------------------------------------------------
 * 当视口宽度较小（移动端）且弹窗处于打开状态时，
 * 用户在弹窗面板上向右滑动可实现原生般的平滑跟手滑动并自动关闭。
 */

let isInitialized = false

export function initDrawerGesture(): void {
  if (typeof window === 'undefined' || isInitialized) return
  isInitialized = true

  let activeModalContent: HTMLElement | null = null
  let touchStartX = 0
  let touchStartY = 0
  let touchStartTime = 0
  let isSwiping = false

  function findModalContent(target: EventTarget | null): HTMLElement | null {
    if (!target || !(target instanceof HTMLElement)) return null
    return target.closest('.modal-content') as HTMLElement | null
  }

  function triggerModalClose(modalContent: HTMLElement): void {
    // 1. 优先寻找头部或内部的显式关闭按钮
    const closeBtn = modalContent.querySelector<HTMLButtonElement>(
      'button[aria-label="close"], button[aria-label="Close"], .modal-header button, button[title*="关闭"], button[title*="Close"]'
    )
    if (closeBtn) {
      closeBtn.click()
      return
    }

    // 2. 尝试点击背景遮罩层
    const overlay = modalContent.closest('.modal-overlay') || modalContent.parentElement
    if (overlay) {
      const backdrop = overlay.querySelector<HTMLElement>('.modal-backdrop')
      if (backdrop) {
        backdrop.click()
        return
      }
    }

    // 3. 兜底模拟派发 Escape 按键
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', code: 'Escape', bubbles: true }))
  }

  window.addEventListener(
    'touchstart',
    (e: TouchEvent) => {
      // 仅在宽度较窄的移动端/小屏启用手势（< 768px）
      if (window.innerWidth >= 768) return
      if (e.touches.length !== 1) return

      const target = e.target
      const content = findModalContent(target)
      if (!content) return

      activeModalContent = content
      touchStartX = e.touches[0].clientX
      touchStartY = e.touches[0].clientY
      touchStartTime = Date.now()
      isSwiping = false
    },
    { passive: true }
  )

  window.addEventListener(
    'touchmove',
    (e: TouchEvent) => {
      if (!activeModalContent || e.touches.length !== 1) return

      const currentX = e.touches[0].clientX
      const currentY = e.touches[0].clientY
      const deltaX = currentX - touchStartX
      const deltaY = currentY - touchStartY

      // 仅处理从左向右滑动且横向位移大于纵向滚动的场景
      if (!isSwiping) {
        if (deltaX > 10 && deltaX > Math.abs(deltaY) * 1.2) {
          isSwiping = true
        } else if (Math.abs(deltaY) > 10) {
          // 纵向滚动，取消本次手势拦截
          activeModalContent = null
          return
        }
      }

      if (isSwiping && deltaX > 0) {
        activeModalContent.style.transition = 'none'
        activeModalContent.style.transform = `translateX(${deltaX}px)`
      }
    },
    { passive: true }
  )

  window.addEventListener(
    'touchend',
    (e: TouchEvent) => {
      if (!activeModalContent) return

      const targetModal = activeModalContent
      activeModalContent = null

      if (!isSwiping) return
      isSwiping = false

      const touchEndX = e.changedTouches[0]?.clientX || 0
      const deltaX = touchEndX - touchStartX
      const deltaTime = Math.max(Date.now() - touchStartTime, 1)
      const velocityX = deltaX / deltaTime // px/ms

      // 判断滑动是否达到关闭阈值：位移大于 80px，或滑动速度较快（> 0.35 px/ms 且位移大于 30px）
      const shouldClose = deltaX > 80 || (velocityX > 0.35 && deltaX > 30)

      targetModal.style.transition = 'transform 0.22s cubic-bezier(0.16, 1, 0.3, 1)'

      if (shouldClose) {
        targetModal.style.transform = 'translateX(100%)'
        setTimeout(() => {
          targetModal.style.transform = ''
          targetModal.style.transition = ''
          triggerModalClose(targetModal)
        }, 180)
      } else {
        // 弹性复位
        targetModal.style.transform = 'translateX(0)'
        setTimeout(() => {
          targetModal.style.transform = ''
          targetModal.style.transition = ''
        }, 220)
      }
    },
    { passive: true }
  )

  window.addEventListener('touchcancel', () => {
    if (activeModalContent) {
      activeModalContent.style.transform = ''
      activeModalContent.style.transition = ''
      activeModalContent = null
      isSwiping = false
    }
  })
}
