// Port of server/src/routes/vip-benefits.ts.
//
// app.ts registers this plugin with NO prefix (`await fastify.register(
// vipBenefitRoutes)`), so every path is absolute below /api. All seven routes
// authenticate in onRequest; the four admin routes stack requireAdmin after it, and
// — as in Fastify — the body parser runs only after the last auth layer, so a
// non-admin gets 403 ADMIN_REQUIRED even with a malformed body.
//
// The route file performs no validation itself: every input is normalized by
// services/vip-benefits.ts, whose VipBenefitError carries the status code
// (400 default, 404 REWARD_NOT_FOUND, 403 VIP_LEVEL_REQUIRED, 409 claim conflicts)
// and the `{error, code}` envelope. Reward ids use the strict
// `/^[1-9]\d*$/` parser. An id above 2^31-1 cannot address a Prisma Int column
// (P2020) and the Node route does not catch it, so it answers 500 like the other
// ports of this codebase.
//
// The inline `config.rateLimit` entries in the TypeScript (60/100/30/20/10 per
// minute or 3 minutes) are dead code in Node — @fastify/rate-limit's onRoute hook is
// registered on the plugin context before app.ts adds the app-level hook — and the
// generated Go rule table has no rule for these paths either, so both backends are
// limited by the global 300/min only.
package routes

import (
	"errors"
	"math"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// vipBenefitsInvalidRewardIDCode mirrors the hand-written INVALID_REWARD_ID body.
const vipBenefitsInvalidRewardIDCode = "INVALID_REWARD_ID"

// vipBenefitsWriteError mirrors sendVipBenefitError: a VipBenefitError is rendered
// verbatim, anything else is an internal error (the Node version rethrows into the
// generic handler).
func vipBenefitsWriteError(c *gin.Context, err error) {
	var benefitErr *services.VipBenefitError
	if errors.As(err, &benefitErr) {
		c.JSON(benefitErr.StatusCode, gin.H{"error": benefitErr.Message, "code": benefitErr.Code})
		return
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// vipBenefitsParseRewardID mirrors parsePositiveRouteId (canonical positive decimal,
// JavaScript safe integer). ok=false is the route's own 400 INVALID_REWARD_ID.
func vipBenefitsParseRewardID(value string) (int32, bool, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok {
		return 0, false, false
	}
	if parsed > math.MaxInt32 {
		// The id parses but cannot address a Prisma Int column (P2020). The Node
		// route does not catch that error, so it surfaces as a 500.
		return 0, true, true
	}
	return int32(parsed), true, false
}

// RegisterVipBenefits mounts the VIP benefit hall routes (no prefix).
func RegisterVipBenefits(r *gin.Engine) {
	r.GET("/api/vip-benefits/me", middleware.Authenticate(), func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		overview, err := services.VipGetBenefitOverview(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, overview)
	})

	r.POST("/api/vip-benefits/:rewardId/claim", middleware.Authenticate(), func(c *gin.Context) {
		rewardID, ok, outOfRange := vipBenefitsParseRewardID(c.Param("rewardId"))
		if outOfRange {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Invalid VIP benefit reward ID",
				"code":  vipBenefitsInvalidRewardIDCode,
			})
			return
		}
		user := middleware.CurrentUser(c)
		result, err := services.VipClaimBenefitReward(c.Request.Context(), user.ID, rewardID)
		if err != nil {
			vipBenefitsWriteError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})

	r.POST("/api/vip-benefits/claim-all", middleware.Authenticate(), func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		result, err := services.VipClaimAllAvailableBenefitRewards(c.Request.Context(), user.ID)
		if err != nil {
			vipBenefitsWriteError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})

	r.GET("/api/admin/vip-benefits/rewards", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		rewards, err := services.VipListAdminBenefitRewards(c.Request.Context())
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"rewards": rewards})
	})

	r.POST("/api/admin/vip-benefits/rewards", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		body, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			middleware.AbortFastifyBody(c, status, message)
			return
		}
		input := services.VipBenefitRewardInputFrom(body.Value)
		reward, err := services.VipCreateBenefitReward(c.Request.Context(), input)
		if err != nil {
			vipBenefitsWriteError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"reward": reward})
	})

	r.PUT("/api/admin/vip-benefits/rewards/:id", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		rewardID, ok, outOfRange := vipBenefitsParseRewardID(c.Param("id"))
		if outOfRange {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Invalid VIP benefit reward ID",
				"code":  vipBenefitsInvalidRewardIDCode,
			})
			return
		}
		body, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			middleware.AbortFastifyBody(c, status, message)
			return
		}
		input := services.VipBenefitRewardInputFrom(body.Value)
		reward, err := services.VipUpdateBenefitReward(c.Request.Context(), rewardID, input)
		if err != nil {
			vipBenefitsWriteError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"reward": reward})
	})

	r.DELETE("/api/admin/vip-benefits/rewards/:id", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		rewardID, ok, outOfRange := vipBenefitsParseRewardID(c.Param("id"))
		if outOfRange {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Invalid VIP benefit reward ID",
				"code":  vipBenefitsInvalidRewardIDCode,
			})
			return
		}
		if err := services.VipDeleteBenefitReward(c.Request.Context(), rewardID); err != nil {
			vipBenefitsWriteError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})
}

func init() {
	Register(RegisterVipBenefits)
}
