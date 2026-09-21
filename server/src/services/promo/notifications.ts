import { createInboxMessage } from '../../db/inbox.js'

export interface PromoBindingExhaustedParams {
  userId: number
  instanceId: number
  instanceName: string
  promoCode: string
  totalCycles: number
}

export interface PromoUnboundByAdminParams {
  userId: number
  instanceId: number
  instanceName: string
  promoCode: string
}

export interface PromoCodeDeletedParams {
  userId: number
  instanceId: number
  instanceName: string
  promoCode: string
}

/**
 * 优惠期数自然耗尽通知
 */
export async function sendPromoBindingExhaustedNotification(
  params: PromoBindingExhaustedParams
): Promise<void> {
  try {
    await createInboxMessage({
      userId: params.userId,
      eventType: 'promo_binding_exhausted',
      title: '【优惠期满】您的实例优惠折扣已结束',
      content: `尊敬的用户：\n您的实例 **${params.instanceName}**（ID: #${params.instanceId}）所绑定的优惠码 **${params.promoCode}** 设定的优惠期数（共 ${params.totalCycles} 期）已全部使用完毕。\n从下一个计费周期开始，该实例将恢复为标准资费续费。您也可以在实例管理页面随时绑定新的有效优惠码。`,
      data: {
        instanceId: params.instanceId,
        instanceName: params.instanceName,
        promoCode: params.promoCode,
        totalCycles: params.totalCycles
      }
    })
  } catch (err) {
    console.error('[Promo Notification] Failed to send promo_binding_exhausted notification:', err)
  }
}

/**
 * 管理员解除绑定通知
 */
export async function sendPromoUnboundByAdminNotification(
  params: PromoUnboundByAdminParams
): Promise<void> {
  try {
    await createInboxMessage({
      userId: params.userId,
      eventType: 'promo_binding_unbound_by_admin',
      title: '【优惠变更】您的实例已解除优惠码绑定',
      content: `尊敬的用户：\n您的实例 **${params.instanceName}**（ID: #${params.instanceId}）绑定的优惠码 **${params.promoCode}** 已由系统管理员解除绑定。\n即日起该实例将不再享受此优惠码的续费折扣，后续续费将按标准资费计费。如有疑问，请通过工单系统联系客服支持。`,
      data: {
        instanceId: params.instanceId,
        instanceName: params.instanceName,
        promoCode: params.promoCode
      }
    })
  } catch (err) {
    console.error('[Promo Notification] Failed to send promo_binding_unbound_by_admin notification:', err)
  }
}

/**
 * 优惠码删除下线通知
 */
export async function sendPromoCodeDeletedNotification(
  params: PromoCodeDeletedParams
): Promise<void> {
  try {
    await createInboxMessage({
      userId: params.userId,
      eventType: 'promo_code_deleted',
      title: '【优惠调整】您使用的优惠码已下线终止',
      content: `尊敬的用户：\n您在实例 **${params.instanceName}**（ID: #${params.instanceId}）上所享受的优惠码 **${params.promoCode}** 已由平台官方正式下线并终止优惠策略。\n该实例后续续费将恢复为标准官方价格。感谢您的理解与支持。`,
      data: {
        instanceId: params.instanceId,
        instanceName: params.instanceName,
        promoCode: params.promoCode
      }
    })
  } catch (err) {
    console.error('[Promo Notification] Failed to send promo_code_deleted notification:', err)
  }
}
