// Port of server/src/routes/vip-levels.ts.
//
// vipLevelRoutes is registered with no prefix (app.ts: `await fastify.register(
// vipLevelRoutes)`), so every path below is absolute. All three routes carry an
// inline per-route rate limit in Node (60/30/20 per minute) that the shared Go
// rate-limit config does not yet know about — see the module report.
package routes

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// parseVipRuleType mirrors parseRuleType: only "user" is accepted.
func parseVipRuleType(value string) (services.VipRuleType, bool) {
	if value == "user" {
		return services.VipRuleTypeUser, true
	}
	return "", false
}

// parseUserVipMetric mirrors parseUserVipMetric.
func parseUserVipMetric(value any) (services.UserVipMetric, bool) {
	asString, ok := value.(string)
	if !ok {
		return "", false
	}
	if asString == string(services.UserVipMetricTotalRecharge) || asString == string(services.UserVipMetricTotalConsume) {
		return services.UserVipMetric(asString), true
	}
	return "", false
}

// vipRuleInputsFromBody mirrors `Array.isArray(request.body?.rules) ? ... : []`.
// Non-object entries become zero inputs, which fail level validation exactly like
// the Node path (Number(undefined) -> NaN).
func vipRuleInputsFromBody(raw any) []services.VipRuleInput {
	items, ok := raw.([]any)
	if !ok {
		return []services.VipRuleInput{}
	}
	inputs := make([]services.VipRuleInput, 0, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			inputs = append(inputs, services.VipRuleInput{})
			continue
		}
		inputs = append(inputs, services.VipRuleInput{
			Level:         record["level"],
			Enabled:       record["enabled"],
			ConditionMode: record["conditionMode"],
			UserMetric:    record["userMetric"],
			MinRecharge:   record["minRecharge"],
			MinConsume:    record["minConsume"],
			Benefits:      record["benefits"],
		})
	}
	return inputs
}

// RegisterVipLevels mounts the VIP level routes (no prefix, matching app.ts).
func RegisterVipLevels(r *gin.Engine) {
	r.GET("/api/vip-levels/me", middleware.Authenticate(), func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		overview, err := services.GetVipOverview(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, overview)
	})

	r.GET("/api/admin/vip-levels/:type", middleware.AuthenticateAdmin(), func(c *gin.Context) {
		ruleType, ok := parseVipRuleType(c.Param("type"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid VIP rule type"})
			return
		}

		ctx := c.Request.Context()
		response := gin.H{
			"type":     ruleType,
			"maxLevel": services.MaxVipLevel,
		}
		if ruleType == services.VipRuleTypeUser {
			metric, err := services.GetUserVipMetric(ctx)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
				return
			}
			response["userMetric"] = metric
		}

		rules, err := services.GetVipRules(ctx, ruleType)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		response["rules"] = rules

		c.JSON(http.StatusOK, response)
	})

	r.PUT("/api/admin/vip-levels/:type", middleware.AuthenticateAdmin(), func(c *gin.Context) {
		ruleType, ok := parseVipRuleType(c.Param("type"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid VIP rule type"})
			return
		}

		var parsedBody any
		raw, err := c.GetRawData()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &parsedBody); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}

		// `request.body?.rules` / `request.body?.userMetric`: property access on a
		// non-object body yields undefined, exactly like JS optional chaining.
		var rulesRaw, userMetricRaw any
		if record, ok := parsedBody.(map[string]any); ok {
			rulesRaw = record["rules"]
			userMetricRaw = record["userMetric"]
		}

		inputs := vipRuleInputsFromBody(rulesRaw)

		var userMetric *services.UserVipMetric
		if ruleType == services.VipRuleTypeUser {
			parsed, ok := parseUserVipMetric(userMetricRaw)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user VIP metric"})
				return
			}
			userMetric = &parsed
		}

		savedRules, err := services.ReplaceVipRules(c.Request.Context(), ruleType, inputs, userMetric)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		response := gin.H{
			"type":     ruleType,
			"maxLevel": services.MaxVipLevel,
			"rules":    savedRules,
		}
		if userMetric != nil {
			response["userMetric"] = *userMetric
		}

		c.JSON(http.StatusOK, response)
	})
}

func init() {
	Register(RegisterVipLevels)
}
