// Port of server/src/routes/admin-tasks.ts.
//
// Three endpoints an administrator uses to watch and cancel long-running node
// operations (instance create/destroy, node recovery):
//
//	GET  /api/admin/tasks            — paged list, with live canStop/stopping
//	POST /api/admin/tasks/:id/stop   — request cancellation
//	GET  /api/admin/tasks/:id/logs/stream — Server-Sent Events log tail
//
// The SSE endpoint is the delicate one. It is a SHORT-LIVED connection on purpose
// (60-second deadline): the client re-authenticates by reconnecting with its last
// cursor, which is why the query parameter is called `after`. Three behaviours
// matter and are reproduced exactly:
//
//  1. Every 200-row page is drained before the stream ends, so a finished task's
//     history is never truncated.
//  2. A slow consumer is disconnected once the socket buffer passes 256 KiB,
//     rather than growing memory; it then resumes from its cursor.
//  3. The terminal event is `reconnect` while the task is still running and `end`
//     once it has finished — the client treats them differently.
package routes

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// adminTasksPageSize is the fixed `take: 20` in the list query.
	adminTasksPageSize = 20
	// adminTasksMaxPage mirrors the schema's `maximum: 100000`.
	adminTasksMaxPage = 100000
	// adminTasksSearchMaxLength mirrors the schema's `maxLength: 100`.
	adminTasksSearchMaxLength = 100
	// adminTasksStreamPageSize mirrors the `take: 200` on log pages.
	adminTasksStreamPageSize = 200
	// adminTasksStreamDeadline mirrors the 60-second short-connection window.
	adminTasksStreamDeadline = 60 * time.Second
	// adminTasksMaxWritableBytes mirrors the 256 KiB buffer guard.
	adminTasksMaxWritableBytes = 256 * 1024
	// adminTasksMaxPerUser / adminTasksMaxTotal mirror the connection caps.
	adminTasksMaxPerUser = 5
	adminTasksMaxTotal   = 100
)

// adminTasksStatuses mirrors the `status` enum.
var adminTasksStatuses = map[string]bool{
	"running":     true,
	"success":     true,
	"failed":      true,
	"cancelled":   true,
	"interrupted": true,
}

// adminTasksKinds mirrors the `kind` enum.
var adminTasksKinds = map[string]bool{
	"create":       true,
	"destroy":      true,
	"node_recover": true,
}

// adminTasksConnections mirrors the per-user SSE connection counter. Process-local
// by design (single-instance invariant in AGENTS.md).
var (
	adminTasksConnectionsMu sync.Mutex
	adminTasksConnections   = map[int32]int{}
)

func adminTasksOpenConnection(userID int32) bool {
	adminTasksConnectionsMu.Lock()
	defer adminTasksConnectionsMu.Unlock()

	if adminTasksConnections[userID] >= adminTasksMaxPerUser {
		return false
	}
	total := 0
	for _, count := range adminTasksConnections {
		total += count
	}
	if total >= adminTasksMaxTotal {
		return false
	}
	adminTasksConnections[userID]++
	return true
}

func adminTasksCloseConnection(userID int32) {
	adminTasksConnectionsMu.Lock()
	defer adminTasksConnectionsMu.Unlock()

	if count := adminTasksConnections[userID] - 1; count > 0 {
		adminTasksConnections[userID] = count
	} else {
		delete(adminTasksConnections, userID)
	}
}

// adminTasksTaskView mirrors `{ ...item, ...operationTaskControl(id), hostName }`.
// Field order in the JSON follows the TypeScript object spread, and the Go struct
// keeps the Prisma camelCase names because the route returns the row verbatim.
type adminTasksTaskView struct {
	ID           int32   `json:"id"`
	HostID       int32   `json:"hostId"`
	InstanceName string  `json:"instanceName"`
	Kind         string  `json:"kind"`
	Status       string  `json:"status"`
	Error        *string `json:"error"`
	CreatedAt    string  `json:"createdAt"`
	FinishedAt   *string `json:"finishedAt"`
	CanStop      bool    `json:"canStop"`
	Stopping     bool    `json:"stopping"`
	HostName     *string `json:"hostName"`
}

// adminTasksParsePage mirrors `page: { type: 'integer', minimum: 1, maximum:
// 100000, default: 1 }`. The message distinguishes the three failure modes
// exactly as AJV does (verified against the live Node server).
func adminTasksParsePage(raw string, present bool) (int, string) {
	if !present || raw == "" {
		return 1, ""
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, "must be integer"
	}
	if parsed < 1 {
		return 0, "must be >= 1"
	}
	if parsed > adminTasksMaxPage {
		return 0, "must be <= 100000"
	}
	return int(parsed), ""
}

// adminTasksParseAfter mirrors `after: { type: 'integer', minimum: 0, maximum:
// 2147483647, default: 0 }`.
func adminTasksParseAfter(raw string, present bool) (int32, string) {
	if !present || raw == "" {
		return 0, ""
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, "must be integer"
	}
	if parsed < 0 {
		return 0, "must be >= 0"
	}
	if parsed > 2147483647 {
		return 0, "must be <= 2147483647"
	}
	return int32(parsed), ""
}

// adminTasksISOTime mirrors Date#toISOString().
func adminTasksISOTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format("2006-01-02T15:04:05.000Z")
	return &formatted
}

// adminTasksTaskView builds the wire shape from a row plus its live control state.
func adminTasksTaskViewFromRow(row models.OperationTask, hostName *string) adminTasksTaskView {
	control := lib.OperationTaskControl(row.ID)
	created := row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	return adminTasksTaskView{
		ID:           row.ID,
		HostID:       row.HostID,
		InstanceName: row.InstanceName,
		Kind:         row.Kind,
		Status:       row.Status,
		Error:        row.Error,
		CreatedAt:    created,
		FinishedAt:   adminTasksISOTime(row.FinishedAt),
		CanStop:      control.CanStop,
		Stopping:     control.Stopping,
		HostName:     hostName,
	}
}

// adminTasksLogView mirrors the operationTaskLog row as SSE `data`.
type adminTasksLogView struct {
	ID        int32  `json:"id"`
	TaskID    int32  `json:"taskId"`
	Source    string `json:"source"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	CreatedAt string `json:"createdAt"`
}

// RegisterAdminTasks mounts /api/admin/tasks. Every route is admin-only
// (`onRequest: [authenticate, requireAdmin]`).
func RegisterAdminTasks(r *gin.Engine) {
	group := r.Group("/api/admin/tasks", middleware.Authenticate(), middleware.RequireAdmin())

	// GET /api/admin/tasks — paged list
	listHandler := func(c *gin.Context) {
		query := c.Request.URL.Query()

		page, pageMessage := adminTasksParsePage(firstQueryValue(query, "page"))
		if pageMessage != "" {
			respondAJVValidation(c, pageMessage)
			return
		}

		status := ""
		if raw, present := query["status"]; present {
			candidate := firstOrEmpty(raw)
			if !adminTasksStatuses[candidate] {
				respondAJVValidation(c, "must be equal to one of the allowed values")
				return
			}
			status = candidate
		}

		kind := ""
		if raw, present := query["kind"]; present {
			candidate := firstOrEmpty(raw)
			if !adminTasksKinds[candidate] {
				respondAJVValidation(c, "must be equal to one of the allowed values")
				return
			}
			kind = candidate
		}

		search := ""
		if raw, present := query["search"]; present {
			candidate := firstOrEmpty(raw)
			if jsStringLength(candidate) > adminTasksSearchMaxLength {
				respondAJVValidation(c, "search is too long")
				return
			}
			search = candidate
		}

		where := db.DB.WithContext(c.Request.Context()).Model(&models.OperationTask{})
		if status != "" {
			where = where.Where("status = ?", status)
		}
		if kind != "" {
			where = where.Where("kind = ?", kind)
		}
		if search != "" {
			where = where.Where("instance_name LIKE ?", "%"+search+"%")
		}

		var total int64
		if err := where.Count(&total).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		var rows []models.OperationTask
		if err := where.
			Order("id DESC").
			Limit(adminTasksPageSize).
			Offset((page - 1) * adminTasksPageSize).
			Find(&rows).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		// Resolve host names in one query, like the TS `findMany({ id: { in: … } })`.
		hostNames := map[int32]string{}
		if len(rows) > 0 {
			hostIDs := make([]int32, 0, len(rows))
			seen := map[int32]bool{}
			for _, row := range rows {
				if !seen[row.HostID] {
					seen[row.HostID] = true
					hostIDs = append(hostIDs, row.HostID)
				}
			}
			var hosts []struct {
				ID   int32  `gorm:"column:id"`
				Name string `gorm:"column:name"`
			}
			if err := db.DB.WithContext(c.Request.Context()).
				Table("hosts").
				Select("id, name").
				Where("id IN ?", hostIDs).
				Find(&hosts).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
				return
			}
			for _, host := range hosts {
				hostNames[host.ID] = host.Name
			}
		}

		items := make([]adminTasksTaskView, 0, len(rows))
		for _, row := range rows {
			var name *string
			if resolved, ok := hostNames[row.HostID]; ok {
				value := resolved
				name = &value
			}
			items = append(items, adminTasksTaskViewFromRow(row, name))
		}

		c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
	}
	// Only the bare path is mounted. Measured against Node: GET /api/admin/tasks/
	// returns 404, so no trailing-slash alias is registered.
	group.GET("", listHandler)

	// POST /api/admin/tasks/:id/stop
	group.POST("/:id/stop", func(c *gin.Context) {
		taskID, idMessage := adminTasksParseRouteID(c.Param("id"))
		if idMessage != "" {
			respondAJVValidation(c, idMessage)
			return
		}

		var task models.OperationTask
		err := db.DB.WithContext(c.Request.Context()).
			Where("id = ?", taskID).
			Limit(1).
			Find(&task).Error
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if task.ID == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
			return
		}
		if task.Status != lib.OperationTaskStatusRunning {
			c.JSON(http.StatusConflict, gin.H{"error": "任务已结束"})
			return
		}

		if err := lib.StopOperationTask(task.ID); err != nil {
			// The TS redacts before putting the message on the wire.
			c.JSON(http.StatusConflict, gin.H{"error": lib.RedactTaskLog(err.Error())})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "取消信号已发送，等待节点确认结束及清理完成"})
	})

	// GET /api/admin/tasks/:id/logs/stream — Server-Sent Events
	group.GET("/:id/logs/stream", func(c *gin.Context) {
		taskID, idMessage := adminTasksParseRouteID(c.Param("id"))
		if idMessage != "" {
			respondAJVValidation(c, idMessage)
			return
		}

		query := c.Request.URL.Query()
		cursor, afterMessage := adminTasksParseAfter(firstQueryValue(query, "after"))
		if afterMessage != "" {
			respondAJVValidation(c, afterMessage)
			return
		}

		ctx := c.Request.Context()

		var task models.OperationTask
		if err := db.DB.WithContext(ctx).Where("id = ?", taskID).Limit(1).Find(&task).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if task.ID == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
			return
		}

		user := middleware.CurrentUser(c)
		if !adminTasksOpenConnection(user.ID) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "日志连接过多，请关闭其他日志窗口"})
			return
		}
		defer adminTasksCloseConnection(user.ID)

		writer := c.Writer
		flusher, canFlush := writer.(http.Flusher)

		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-cache, no-transform")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.Header().Set("Connection", "keep-alive")
		writer.WriteHeader(http.StatusOK)
		if canFlush {
			flusher.Flush()
		}

		deadline := time.Now().Add(adminTasksStreamDeadline)
		closed := false

		// send mirrors the TS `send`: it reports false once the socket is gone or
		// too far behind, which tells the loop to stop.
		send := func(event string, data any, id *int32) bool {
			if closed || ctx.Err() != nil {
				return false
			}
			// A slow consumer reconnects from its cursor; never grow the buffer.
			if writer.Size() > adminTasksMaxWritableBytes {
				closed = true
				return false
			}

			var builder strings.Builder
			if id != nil {
				builder.WriteString("id: ")
				builder.WriteString(strconv.FormatInt(int64(*id), 10))
				builder.WriteString("\n")
			}
			builder.WriteString("event: ")
			builder.WriteString(event)
			builder.WriteString("\ndata: ")
			encoded, err := json.Marshal(data)
			if err != nil {
				return false
			}
			builder.Write(encoded)
			builder.WriteString("\n\n")

			if _, err := io.WriteString(writer, builder.String()); err != nil {
				closed = true
				return false
			}
			if canFlush {
				flusher.Flush()
			}
			return true
		}

		for {
			if closed || ctx.Err() != nil {
				return
			}

			var logs []models.OperationTaskLog
			if err := db.DB.WithContext(ctx).
				Where("task_id = ? AND id > ?", taskID, cursor).
				Order("id ASC").
				Limit(adminTasksStreamPageSize).
				Find(&logs).Error; err != nil {
				send("stream-error", gin.H{"message": "读取日志失败，请重连"}, nil)
				return
			}

			for _, row := range logs {
				view := adminTasksLogView{
					ID:        row.ID,
					TaskID:    row.TaskID,
					Source:    row.Source,
					Level:     row.Level,
					Message:   row.Message,
					CreatedAt: row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
				}
				rowID := row.ID
				if !send("log", view, &rowID) {
					return
				}
				cursor = row.ID
			}

			var current models.OperationTask
			if err := db.DB.WithContext(ctx).Where("id = ?", taskID).Limit(1).Find(&current).Error; err != nil {
				send("stream-error", gin.H{"message": "读取日志失败，请重连"}, nil)
				return
			}

			var taskPayload any
			if current.ID != 0 {
				taskPayload = adminTasksTaskViewFromRow(current, nil)
			}
			if !send("task", taskPayload, nil) {
				return
			}

			// Drain all historical pages before completing; never truncate a
			// finished task's logs.
			finished := current.Status != lib.OperationTaskStatusRunning
			if (finished && len(logs) < adminTasksStreamPageSize) || time.Now().After(deadline) {
				event := "end"
				if !finished {
					event = "reconnect"
				}
				send(event, gin.H{}, nil)
				return
			}

			if len(logs) >= adminTasksStreamPageSize {
				continue // next page immediately
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	})
}

// firstQueryValue returns the single value of a query key, mirroring Fastify's
// parser (a repeated key becomes an array and fails the integer coercion).
func firstQueryValue(query map[string][]string, name string) (string, bool) {
	values, present := query[name]
	if !present {
		return "", false
	}
	if len(values) != 1 {
		return "", present
	}
	return values[0], true
}

func init() {
	Register(RegisterAdminTasks)
}

// adminTasksParseRouteID mirrors the Fastify params schema
// `{ type: 'integer', minimum: 1, maximum: 2147483647 }`. It returns the AJV
// message on failure so the route can answer with the exact same text.
func adminTasksParseRouteID(raw string) (int32, string) {
	parsed, ok := lib.ParsePositiveRouteID(raw)
	if !ok {
		// A non-numeric value fails `type: 'integer'`; a numeric-but-zero value
		// fails `minimum: 1`. AJV reports whichever keyword fails first.
		if _, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err != nil {
			return 0, "must be integer"
		}
		return 0, "must be >= 1"
	}
	if parsed > 2147483647 {
		return 0, "must be <= 2147483647"
	}
	return int32(parsed), ""
}
