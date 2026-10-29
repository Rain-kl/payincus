// Port of server/src/routes/images.ts.
package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// systemImageListItem is the user-facing projection of GET /api/images/system.
type systemImageListItem struct {
	ID           int32  `json:"id"`
	Name         string `json:"name"`
	RemoteAlias  string `json:"remoteAlias"`
	OSType       string `json:"osType"`
	Architecture string `json:"architecture"`
	InstanceType string `json:"instanceType"`
	Icon         string `json:"icon"`
}

// systemImageInput mirrors the destructured request body of the admin create /
// update routes. A nil pointer means the key was absent (Prisma skips it);
// `null` decodes to the zero value, exactly like the TypeScript truthiness checks.
type systemImageInput struct {
	Name         *string
	RemoteAlias  *string
	OSType       *string
	Architecture *string
	InstanceType *string
	Icon         *string
	SortOrder    *int32
	Hidden       *bool
}

// parseOptionalImageQuery mirrors parsePositiveInteger() in the route module: an
// absent value yields (nil, true), a malformed one (nil, false) — the caller
// turns the latter into a 400 INVALID_ID.
func parseOptionalImageQuery(query url.Values, key string) (*int, bool) {
	values, present := query[key]
	if !present {
		return nil, true
	}
	raw := ""
	if len(values) > 0 {
		raw = values[0]
	}
	parsed, ok := lib.ParsePositiveRouteID(raw)
	if !ok {
		return nil, false
	}
	value := int(parsed)
	return &value, true
}

// readImageRequestBody reads the raw body, treating an empty one as `{}` so the
// downstream truthiness checks mirror the TypeScript destructuring of a body.
func readImageRequestBody(c *gin.Context) []byte {
	data, err := io.ReadAll(c.Request.Body)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return []byte("{}")
	}
	return data
}

// decodeSystemImageInput mirrors `const { name, remoteAlias, ... } = request.body`:
// unknown keys are dropped and a mistyped known key is a hard failure (Prisma
// would reject it too).
func decodeSystemImageInput(data []byte) (systemImageInput, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return systemImageInput{}, err
	}

	var input systemImageInput
	var err error
	if input.Name, err = decodeOptionalImageString(raw, "name"); err != nil {
		return systemImageInput{}, err
	}
	if input.RemoteAlias, err = decodeOptionalImageString(raw, "remoteAlias"); err != nil {
		return systemImageInput{}, err
	}
	if input.OSType, err = decodeOptionalImageString(raw, "osType"); err != nil {
		return systemImageInput{}, err
	}
	if input.Architecture, err = decodeOptionalImageString(raw, "architecture"); err != nil {
		return systemImageInput{}, err
	}
	if input.InstanceType, err = decodeOptionalImageString(raw, "instanceType"); err != nil {
		return systemImageInput{}, err
	}
	if input.Icon, err = decodeOptionalImageString(raw, "icon"); err != nil {
		return systemImageInput{}, err
	}
	if value, present := raw["sortOrder"]; present {
		var sortOrder int32
		if err := json.Unmarshal(value, &sortOrder); err != nil {
			return systemImageInput{}, fmt.Errorf("images: field %q: %w", "sortOrder", err)
		}
		input.SortOrder = &sortOrder
	}
	if value, present := raw["hidden"]; present {
		var hidden bool
		if err := json.Unmarshal(value, &hidden); err != nil {
			return systemImageInput{}, fmt.Errorf("images: field %q: %w", "hidden", err)
		}
		input.Hidden = &hidden
	}
	return input, nil
}

func decodeOptionalImageString(raw map[string]json.RawMessage, key string) (*string, error) {
	value, present := raw[key]
	if !present {
		return nil, nil
	}
	var out string
	if err := json.Unmarshal(value, &out); err != nil {
		return nil, fmt.Errorf("images: field %q: %w", key, err)
	}
	return &out, nil
}

// isImageUniqueViolation mirrors the Prisma `error.code === 'P2002'` branch: a
// PostgreSQL unique-constraint violation (remote_alias is unique).
func isImageUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// RegisterImages mounts /api/images. Every route requires authentication; the
// admin routes additionally require the admin role, mirroring the per-route
// onRequest arrays in the TypeScript module.
func RegisterImages(r *gin.Engine) {
	group := r.Group("/api/images", middleware.Authenticate())

	// 获取系统镜像列表（用户端 - 只返回未隐藏的）
	group.GET("/system", func(c *gin.Context) {
		query := c.Request.URL.Query()
		memory, memoryOK := parseOptionalImageQuery(query, "memory")
		hostID, hostOK := parseOptionalImageQuery(query, "hostId")
		if !memoryOK || !hostOK {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		var instanceType *string
		if raw := query.Get("type"); raw != "" {
			instanceType = &raw
		}

		images, err := db.GetVisibleSystemImages(c.Request.Context(), instanceType, memory, hostID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		items := make([]systemImageListItem, 0, len(images))
		for _, image := range images {
			imageType := image.InstanceType
			if imageType == "" {
				imageType = "both"
			}
			items = append(items, systemImageListItem{
				ID:           image.ID,
				Name:         image.Name,
				RemoteAlias:  image.RemoteAlias,
				OSType:       image.OSType,
				Architecture: image.Architecture,
				InstanceType: imageType,
				Icon:         image.Icon,
			})
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "images": items})
	})

	// 管理员：获取所有镜像（包含隐藏的）
	group.GET("/admin", middleware.RequireAdmin(), func(c *gin.Context) {
		images, err := db.GetAllSystemImages(c.Request.Context())
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "images": images})
	})

	// 管理员：创建镜像
	group.POST("/admin", middleware.RequireAdmin(), func(c *gin.Context) {
		input, err := decodeSystemImageInput(readImageRequestBody(c))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		if input.Name == nil || *input.Name == "" ||
			input.RemoteAlias == nil || *input.RemoteAlias == "" ||
			input.Icon == nil || *input.Icon == "" {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidName, "")
			return
		}

		image, err := db.CreateSystemImage(c.Request.Context(), db.SystemImageCreateData{
			Name:         *input.Name,
			RemoteAlias:  *input.RemoteAlias,
			OSType:       input.OSType,
			Architecture: input.Architecture,
			InstanceType: input.InstanceType,
			Icon:         *input.Icon,
			SortOrder:    input.SortOrder,
			Hidden:       input.Hidden,
		})
		if err != nil {
			if isImageUniqueViolation(err) {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidName, "Remote alias already exists")
				return
			}
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(middleware.CurrentUser(c).ID)
		db.LogSuccess(c.Request.Context(), &userID, "images", "create",
			fmt.Sprintf("Created system image: %s (%s)", *input.Name, *input.RemoteAlias), db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"success": true, "image": image})
	})

	// 管理员：更新镜像
	group.PATCH("/admin/:id", middleware.RequireAdmin(), func(c *gin.Context) {
		input, err := decodeSystemImageInput(readImageRequestBody(c))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		id, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		existing, err := db.GetSystemImageByID(c.Request.Context(), int32(id))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if existing == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
			return
		}

		image, err := db.UpdateSystemImage(c.Request.Context(), int32(id), db.SystemImageUpdateData{
			Name:         input.Name,
			RemoteAlias:  input.RemoteAlias,
			OSType:       input.OSType,
			Architecture: input.Architecture,
			InstanceType: input.InstanceType,
			Icon:         input.Icon,
			SortOrder:    input.SortOrder,
			Hidden:       input.Hidden,
		})
		if err != nil {
			if isImageUniqueViolation(err) {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidName, "Remote alias already exists")
				return
			}
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(middleware.CurrentUser(c).ID)
		db.LogSuccess(c.Request.Context(), &userID, "images", "update",
			fmt.Sprintf("Updated system image: %s", image.Name), db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"success": true, "image": image})
	})

	// 管理员：删除镜像
	group.DELETE("/admin/:id", middleware.RequireAdmin(), func(c *gin.Context) {
		id, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		existing, err := db.GetSystemImageByID(c.Request.Context(), int32(id))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if existing == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
			return
		}

		if _, err := db.DeleteSystemImage(c.Request.Context(), int32(id)); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(middleware.CurrentUser(c).ID)
		db.LogSuccess(c.Request.Context(), &userID, "images", "delete",
			fmt.Sprintf("Deleted system image: %s", existing.Name), db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"success": true})
	})
}

func init() {
	Register(RegisterImages)
}
