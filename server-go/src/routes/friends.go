// Port of server/src/routes/friends.ts.
//
// Every route authenticates with `fastify.authenticateUser` (regular users only;
// administrators get 403 USER_ONLY). Route ids go through the strict
// ^[1-9]\d*$ parser (lib.ParsePositiveRouteID), never a bare strconv.Atoi.
package routes

import (
	"errors"
	"math"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

const friendsLogModule = "friend"

// friendHistoryFilters mirrors the `FriendHistoryFilter` union used by
// parseFriendHistoryFilter.
var friendHistoryFilters = map[string]bool{
	"accepted": true,
	"rejected": true,
	"removed":  true,
	"all":      true,
}

// parseFriendHistoryFilter mirrors parseFriendHistoryFilter: an absent/empty value
// is "no filter", a supported value is accepted, anything else is invalid.
func parseFriendHistoryFilter(raw string, present bool) (string, bool, bool) {
	if !present || raw == "" {
		return "", false, true
	}
	if friendHistoryFilters[raw] {
		return raw, true, true
	}
	return "", false, false
}

// respondFriendsInternal mirrors the Node catch-all in every friends handler:
// `reply.code(500).send(apiError(ErrorCode.INTERNAL_ERROR, message))`.
// apiError drops `details` for internal errors, so the body is the fixed envelope
// with the INTERNAL_ERROR code.
func respondFriendsInternal(c *gin.Context, details string) {
	lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, details)
}

// sendFriendNotification fires the notifier and swallows the error, mirroring
// `.catch(err => console.error(...))` in the TypeScript routes.
func sendFriendNotification(ctx *gin.Context, userID int32, eventType lib.EventType, data lib.EventData) {
	lib.SendNotification(ctx.Request.Context(), int64(userID), eventType, data)
}

// RegisterFriends mounts /api/friends.
func RegisterFriends(r *gin.Engine) {
	group := r.Group("/api/friends", middleware.AuthenticateUser())

	// GET /api/friends — 好友列表
	//
	// Fastify registers the plugin root under both "/api/friends" and
	// "/api/friends/" (prefixTrailingSlash defaults to 'both').
	listFriends := func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		friends, err := db.GetFriendList(c.Request.Context(), user.ID)
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"friends": friends})
	}
	group.GET("", listFriends)
	group.GET("/", listFriends)

	// POST /api/friends/request — 发送好友请求
	group.POST("/request", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		body, validationMessage := validateAJVObject(decoded.Value, []string{"username"},
			[]ajvFieldSpec{
				{Name: "username", Kind: ajvKindString, MinLength: 1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
				{Name: "remark", Kind: ajvKindString, MinLength: -1, MaxLength: 200, Minimum: math.NaN(), Maximum: math.NaN()},
			})
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		username, _ := body.stringValue("username")
		remark, hasRemark := body.stringValue("remark")
		var remarkPointer *string
		if hasRemark && remark != "" {
			remarkPointer = &remark
		}

		targetUser, err := db.SearchUserByUsername(ctx, username, user.ID)
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		if targetUser == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
			return
		}

		result, err := db.SendFriendRequest(ctx, user.ID, targetUser.ID, remarkPointer)
		if err != nil {
			switch {
			case errors.Is(err, db.ErrFriendAlreadyFriends):
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrAlreadyFriends, "")
			case errors.Is(err, db.ErrFriendRequestPending):
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrFriendRequestPending, "")
			default:
				respondFriendsInternal(c, err.Error())
			}
			return
		}

		userID := int64(user.ID)
		if result.Status == "accepted" {
			// 自动接受：对方之前发过请求
			db.CreateLog(ctx, &userID, friendsLogModule, "friend.request_auto_accepted",
				`Auto accepted friend request with user "`+targetUser.Username+`"`,
				db.LogResultSuccess, db.CreateLogOptions{})

			sendFriendNotification(c, targetUser.ID, lib.EventFriendAccepted, lib.EventData{
				"toUsername": user.Username,
			})
			sendFriendNotification(c, user.ID, lib.EventFriendRequest, lib.EventData{
				"fromUsername": targetUser.Username,
			})

			c.JSON(http.StatusOK, gin.H{
				"message":    "好友添加成功（对方已向您发送请求）",
				"friendship": result,
			})
			return
		}

		db.CreateLog(ctx, &userID, friendsLogModule, "friend.request_sent",
			`Sent friend request to user "`+targetUser.Username+`"`,
			db.LogResultSuccess, db.CreateLogOptions{})

		sendFriendNotification(c, targetUser.ID, lib.EventFriendRequest, lib.EventData{
			"fromUsername": user.Username,
		})

		c.JSON(http.StatusOK, gin.H{
			"message": "好友请求已发送",
			"request": gin.H{"id": result.ID},
		})
	})

	// GET /api/friends/requests — 收到的待处理请求
	group.GET("/requests", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		requests, err := db.GetPendingFriendRequests(c.Request.Context(), user.ID)
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"requests": requests})
	})

	// GET /api/friends/requests/sent — 已发送的待处理请求
	group.GET("/requests/sent", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		requests, err := db.GetSentFriendRequests(c.Request.Context(), user.ID)
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"requests": requests})
	})

	// DELETE /api/friends/request/:id — 取消已发送的请求
	group.DELETE("/request/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		// DELETE is a bodywith method in Fastify: an empty JSON body with
		// Content-Type: application/json is rejected before the handler runs.
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		requestID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		if _, err := db.CancelFriendRequest(ctx, int32(requestID), user.ID); err != nil {
			switch {
			case errors.Is(err, db.ErrFriendRequestNotFound):
				lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "请求不存在")
			case errors.Is(err, db.ErrFriendForbidden):
				lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "无权取消此请求")
			case errors.Is(err, db.ErrFriendRequestNotPending):
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrFriendRequestNotPending, "")
			default:
				respondFriendsInternal(c, err.Error())
			}
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, friendsLogModule, "friend.request_cancelled",
			"Cancelled friend request #"+c.Param("id"), db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "已取消好友请求"})
	})

	// POST /api/friends/:id/accept
	group.POST("/:id/accept", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		requestID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		friendship, err := db.GetFriendshipById(ctx, int32(requestID))
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		if friendship == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrFriendRequestNotFound, "")
			return
		}

		result, err := db.AcceptFriendRequest(ctx, int32(requestID), user.ID)
		if err != nil {
			switch {
			case errors.Is(err, db.ErrFriendRequestNotFound):
				lib.RespondAPIError(c, http.StatusNotFound, lib.ErrFriendRequestNotFound, "")
			case errors.Is(err, db.ErrFriendForbidden):
				lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "无权处理此请求")
			case errors.Is(err, db.ErrFriendRequestNotPending):
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrFriendRequestNotPending, "")
			default:
				respondFriendsInternal(c, err.Error())
			}
			return
		}

		userID := int64(user.ID)
		senderUsername := ""
		if friendship.User != nil {
			senderUsername = friendship.User.Username
		}
		db.CreateLog(ctx, &userID, friendsLogModule, "friend.request_accepted",
			`Accepted friend request from user "`+senderUsername+`"`,
			db.LogResultSuccess, db.CreateLogOptions{})

		sendFriendNotification(c, friendship.UserID, lib.EventFriendAccepted, lib.EventData{
			"toUsername": user.Username,
		})

		c.JSON(http.StatusOK, gin.H{
			"message":    "已接受好友请求",
			"friendship": result,
		})
	})

	// POST /api/friends/:id/reject
	group.POST("/:id/reject", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		requestID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		friendship, err := db.GetFriendshipById(ctx, int32(requestID))
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		if friendship == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrFriendRequestNotFound, "")
			return
		}

		if _, err := db.RejectFriendRequest(ctx, int32(requestID), user.ID); err != nil {
			switch {
			case errors.Is(err, db.ErrFriendRequestNotFound):
				lib.RespondAPIError(c, http.StatusNotFound, lib.ErrFriendRequestNotFound, "")
			case errors.Is(err, db.ErrFriendForbidden):
				lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "无权处理此请求")
			case errors.Is(err, db.ErrFriendRequestNotPending):
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrFriendRequestNotPending, "")
			default:
				respondFriendsInternal(c, err.Error())
			}
			return
		}

		userID := int64(user.ID)
		senderUsername := ""
		if friendship.User != nil {
			senderUsername = friendship.User.Username
		}
		db.CreateLog(ctx, &userID, friendsLogModule, "friend.request_rejected",
			`Rejected friend request from user "`+senderUsername+`"`,
			db.LogResultSuccess, db.CreateLogOptions{})

		sendFriendNotification(c, friendship.UserID, lib.EventFriendRejected, lib.EventData{
			"toUsername": user.Username,
		})

		c.JSON(http.StatusOK, gin.H{"message": "已拒绝好友请求"})
	})

	// DELETE /api/friends/:id — 删除好友（软删除）
	group.DELETE("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		friendshipID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		friendship, err := db.GetFriendshipById(ctx, int32(friendshipID))
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		if friendship == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrFriendshipNotFound, "")
			return
		}

		// 确定对方信息用于日志和通知
		friendUsername := ""
		friendUserID := friendship.UserID
		if friendship.UserID == user.ID {
			if friendship.Friend != nil {
				friendUsername = friendship.Friend.Username
			}
			friendUserID = friendship.FriendID
		} else if friendship.User != nil {
			friendUsername = friendship.User.Username
			friendUserID = friendship.UserID
		}

		if _, err := db.RemoveFriend(ctx, int32(friendshipID), user.ID); err != nil {
			switch {
			case errors.Is(err, db.ErrFriendshipNotFound):
				lib.RespondAPIError(c, http.StatusNotFound, lib.ErrFriendshipNotFound, "")
			case errors.Is(err, db.ErrFriendForbidden):
				lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "无权删除此好友")
			default:
				respondFriendsInternal(c, err.Error())
			}
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, friendsLogModule, "friend.removed",
			`Removed friend "`+friendUsername+`"`, db.LogResultSuccess, db.CreateLogOptions{})

		sendFriendNotification(c, friendUserID, lib.EventFriendRemoved, lib.EventData{
			"fromUsername": user.Username,
		})

		c.JSON(http.StatusOK, gin.H{"message": "已删除好友"})
	})

	// GET /api/friends/history
	group.GET("/history", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		query := c.Request.URL.Query()
		rawFilter, filterPresent := query["filter"]
		filter, _, valid := parseFriendHistoryFilter(firstOrEmpty(rawFilter), filterPresent)
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid history filter")
			return
		}

		history, err := db.GetRequestHistory(ctx, user.ID, filter)
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"history": history})
	})

	// GET /api/friends/search
	group.GET("/search", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		// Fastify validates the querystring before the handler runs.
		rawUsername, present := c.Request.URL.Query()["username"]
		username := firstOrEmpty(rawUsername)
		if !present {
			respondAJVValidation(c, "username is required")
			return
		}
		if username == "" {
			respondAJVValidation(c, "username is too short")
			return
		}

		targetUser, err := db.SearchUserByUsername(ctx, username, user.ID)
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		if targetUser == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "用户不存在")
			return
		}

		isFriend, err := db.AreFriends(ctx, user.ID, targetUser.ID)
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"user": gin.H{
				"id":          targetUser.ID,
				"username":    targetUser.Username,
				"avatarStyle": targetUser.AvatarStyle,
				"isFriend":    isFriend,
			},
		})
	})

	// GET /api/friends/:id/resources
	group.GET("/:id/resources", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		friendshipID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		friendship, err := db.GetFriendshipById(ctx, int32(friendshipID))
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		if friendship == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "好友关系不存在")
			return
		}
		// 验证权限
		if friendship.UserID != user.ID && friendship.FriendID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		friendUserID := friendship.UserID
		if friendship.UserID == user.ID {
			friendUserID = friendship.FriendID
		}

		hostCount, err := db.CountUserHosts(ctx, friendUserID)
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}
		packageCount, err := db.CountUserPackages(ctx, friendUserID)
		if err != nil {
			respondFriendsInternal(c, err.Error())
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"resources": gin.H{
				"hosts": hostCount,
				// 镜像是节点级别的，不是用户级别
				"images":   0,
				"packages": packageCount,
			},
		})
	})
}

func init() {
	Register(RegisterFriends)
}
