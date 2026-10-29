// Port of server/src/routes/announcements.ts.
package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

var announcementTypes = map[string]bool{
	string(models.AnnouncementTypeSystemBroadcast): true,
	string(models.AnnouncementTypeHostBroadcast):   true,
	string(models.AnnouncementTypeAdminMessage):    true,
	string(models.AnnouncementTypeHostMessage):     true,
}

// parseAnnouncementType returns (value, present) mirroring the Node tri-state.
func parseAnnouncementType(raw string, present bool) (string, bool, bool) {
	if !present {
		return "", false, true
	}
	if announcementTypes[raw] {
		return raw, true, true
	}
	return "", false, false
}

func parseOptionalPositiveID(raw string, present bool) (int64, bool, bool) {
	if !present {
		return 0, false, true
	}
	parsed, ok := lib.ParsePositiveRouteID(raw)
	if !ok {
		return 0, false, false
	}
	return parsed, true, true
}

// RegisterAnnouncements mounts /api/announcements. Every route requires auth,
// mirroring the plugin-level onRequest hook.
func RegisterAnnouncements(r *gin.Engine) {
	group := r.Group("/api/announcements", middleware.Authenticate())

	group.GET("", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		if user.Role != "admin" {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		query := c.Request.URL.Query()
		rawType, typePresent := query["type"]
		typeValue, hasType, valid := parseAnnouncementType(firstOrEmpty(rawType), typePresent)
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid announcement type")
			return
		}

		options := db.AnnouncementListOptions{
			Page:     lib.ParsePositiveIntegerQuery(query.Get("page"), 1, 100000),
			PageSize: lib.ParsePositiveIntegerQuery(query.Get("pageSize"), 20, 100),
		}
		if hasType {
			options.Type = typeValue
		}

		result, err := db.GetAnnouncementList(c.Request.Context(), options)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, result)
	})

	group.GET("/my", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		rawHostID, hostPresent := query["hostId"]
		hostID, hasHostID, valid := parseOptionalPositiveID(firstOrEmpty(rawHostID), hostPresent)
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		options := db.AnnouncementListOptions{
			Page:     lib.ParsePositiveIntegerQuery(query.Get("page"), 1, 100000),
			PageSize: lib.ParsePositiveIntegerQuery(query.Get("pageSize"), 20, 100),
		}
		if hasHostID {
			options.HostID = hostID
		}

		result, err := db.GetHostOwnerAnnouncementList(c.Request.Context(), int64(user.ID), options)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, result)
	})

	group.GET("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		announcementID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		announcement, err := db.GetAnnouncementByID(c.Request.Context(), announcementID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if announcement == nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error":   "ANNOUNCEMENT_NOT_FOUND",
				"message": "Announcement not found",
			})
			return
		}

		if user.Role != "admin" && announcement.SenderID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		c.JSON(http.StatusOK, announcement)
	})
}

func firstOrEmpty(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func init() {
	Register(RegisterAnnouncements)
}
