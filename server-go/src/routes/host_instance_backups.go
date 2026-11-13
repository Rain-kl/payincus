package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

const hostBackupEndpoint = "/1.0/agent/instance-backups"
const hostBackupExtension = "incus_instance_backup_v1"

var hostBackupID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func hostInstanceBackupsHandler(c *gin.Context) {
	nodeSecurityNoStore(c)
	hostID, ok := nodeSecurityParseHostID(c)
	if !ok {
		return
	}
	var input struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
	}
	if c.Request.Method == http.MethodPost {
		decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || !hostBackupID.MatchString(input.ID) || len(input.Directory) < 2 || len(input.Directory) > 4096 || !strings.HasPrefix(input.Directory, "/") || strings.ContainsAny(input.Directory, "\x00\r\n") {
			c.JSON(400, gin.H{"error": "请输入有效的任务 ID 和宿主机绝对目录"})
			return
		}
	}
	ctx := c.Request.Context()
	client, err := nodeSecurityClient(ctx, hostID)
	if err != nil {
		c.JSON(409, gin.H{"error": err.Error()})
		return
	}
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/agent", nil)
	if err != nil {
		status := 502
		var apiError *lib.IncusApiError
		if errors.As(err, &apiError) {
			switch apiError.StatusCode {
			case 400, 403, 404, 409, 503:
				status = apiError.StatusCode
			}
		}
		c.JSON(status, gin.H{"error": lib.NodeErrorDetail(err, "节点备份请求失败", nil)})
		return
	}
	var info struct {
		Extensions []string `json:"api_extensions"`
	}
	if json.Unmarshal(raw, &info) != nil || !nodeSecurityHasExtension(info.Extensions, hostBackupExtension) {
		c.JSON(409, gin.H{"error": "节点尚不支持实例备份，请升级节点集成包"})
		return
	}
	var body any
	if c.Request.Method == http.MethodPost {
		body = input
		userID := int64(middleware.CurrentUser(c).ID)
		if err := db.CreateNodeSecurityLogStrict(ctx, userID, "host", "host.backup.start", fmt.Sprintf("节点 #%d 管理员提交实例备份任务 %s", hostID, input.ID), db.LogResultSuccess); err != nil {
			c.JSON(503, gin.H{"error": "审计记录未能保存，未提交备份任务"})
			return
		}
	}
	raw, err = client.Request(ctx, c.Request.Method, hostBackupEndpoint, body)
	if err != nil {
		status := 502
		var apiError *lib.IncusApiError
		if errors.As(err, &apiError) {
			switch apiError.StatusCode {
			case 400, 403, 404, 409, 503:
				status = apiError.StatusCode
			}
		}
		c.JSON(status, gin.H{"error": lib.NodeErrorDetail(err, "节点备份请求失败", nil)})
		return
	}
	c.Data(200, "application/json", raw)
}

func RegisterHostInstanceBackups(r *gin.Engine) {
	group := r.Group("/api/admin/hosts/:hostId/instance-backups", middleware.AuthenticateAdmin())
	group.GET("", hostInstanceBackupsHandler)
	group.POST("", hostInstanceBackupsHandler)
}

func init() { Register(RegisterHostInstanceBackups) }
