package handlers

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
	"hubproxy/utils"
)

// RegisterAdminRoutes 注册管理员接口：访问汇总与明细
func RegisterAdminRoutes(router *gin.Engine) {
	admin := router.Group("/api/admin", adminAuthMiddleware())
	{
		admin.GET("/access/summary", handleAccessSummary)
		admin.GET("/access/logs", handleAccessLogs)
	}
}

// adminAuthMiddleware 管理接口鉴权：
// 配置 token 后必须携带 Bearer token；未配置时仅允许本机回环访问。
func adminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := config.GetConfig()
		token := strings.TrimSpace(cfg.Admin.Token)

		if token != "" {
			provided := strings.TrimSpace(c.Query("token"))
			if auth := c.GetHeader("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				provided = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
			}

			if provided == "" ||
				subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				c.JSON(http.StatusUnauthorized, gin.H{
					"error": "未授权：管理员令牌无效或缺失",
					"code":  "ADMIN_UNAUTHORIZED",
				})
				c.Abort()
				return
			}
			c.Next()
			return
		}

		if !isLoopbackIP(c.ClientIP()) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "管理接口仅允许本机访问，团队共用请在配置中设置 admin.token（或 ADMIN_TOKEN 环境变量）",
				"code":  "ADMIN_FORBIDDEN",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

func isLoopbackIP(ip string) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	return parsed != nil && parsed.IsLoopback()
}

// parseAccessFilter 从查询参数解析访问记录过滤条件
func parseAccessFilter(c *gin.Context) utils.AccessLogFilter {
	filter := utils.AccessLogFilter{
		IP:     strings.TrimSpace(c.Query("ip")),
		Type:   strings.TrimSpace(c.Query("type")),
		Target: strings.TrimSpace(c.Query("target")),
	}

	if status := c.Query("status"); status != "" {
		if code, err := strconv.Atoi(status); err == nil {
			filter.Status = code
		}
	}

	filter.StartTime = parseTimeParam(c.Query("start"))
	filter.EndTime = parseTimeParam(c.Query("end"))

	return filter
}

// parseTimeParam 支持 Unix 秒与 RFC3339 两种时间格式
func parseTimeParam(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(unix, 0)
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	return time.Time{}
}

// handleAccessSummary 访问汇总：GET /api/admin/access/summary?group_by=ip|day|hour|target
func handleAccessSummary(c *gin.Context) {
	if utils.GlobalAccessLogger == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "访问记录未启用"})
		return
	}

	groupBy := strings.TrimSpace(c.DefaultQuery("group_by", "ip"))
	switch groupBy {
	case "ip", "day", "hour", "target":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "group_by 仅支持 ip、day、hour、target"})
		return
	}

	filter := parseAccessFilter(c)
	groups, totals := utils.GlobalAccessLogger.Summary(groupBy, filter)

	c.JSON(http.StatusOK, gin.H{
		"group_by": groupBy,
		"filters":  filter,
		"totals":   totals,
		"groups":   groups,
	})
}

// handleAccessLogs 访问明细：GET /api/admin/access/logs（分页 + 筛选，按时间倒序）
func handleAccessLogs(c *gin.Context) {
	if utils.GlobalAccessLogger == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "访问记录未启用"})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))

	filter := parseAccessFilter(c)
	items, total := utils.GlobalAccessLogger.Query(filter, page, pageSize)

	c.JSON(http.StatusOK, gin.H{
		"filters":   filter,
		"page":      page,
		"page_size": pageSize,
		"total":     total,
		"items":     items,
	})
}
