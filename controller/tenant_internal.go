package controller

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
)

// 租户供给内部接口（/api/internal/tenant/*）。仅供 tenant-plane 经内部 M2M 鉴权
// （InternalWalletAuth，与钱包接口同 shared secret）调用，为组织计费用户「代建/改」
// 项目 token（携 ModelLimits/RemainQuota/Group/AllowIps）。
//
// 存在理由：new-api 原生 token API 全在 UserAuth 下、只作用于当前登录用户
// （controller.AddToken 用 c.GetInt("id")），无 admin 跨用户建 token 能力；企业多租户
// 的「组织=一个计费用户、项目=其名下 token」映射需要此内部端点。走 model 层保证
// token 缓存安全，更新路径强制 token 归属 user_id（租户隔离）。见企业多租户设计 §6/§7/§8。

type provisionTokenRequest struct {
	UserId             int    `json:"user_id"`
	TokenId            int    `json:"token_id"` // 0=新建；>0=更新（必须属于 user_id）
	Name               string `json:"name"`
	ExpiredTime        int64  `json:"expired_time"` // 0/缺省=永不过期(-1)
	RemainQuota        int    `json:"remain_quota"`
	UnlimitedQuota     bool   `json:"unlimited_quota"`
	ModelLimitsEnabled bool   `json:"model_limits_enabled"`
	ModelLimits        string `json:"model_limits"`
	AllowIps           string `json:"allow_ips"`
	Group              string `json:"group"`
}

func tenantError(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"success": false, "message": msg})
}

func tenantOK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// ProvisionTenantToken 为组织计费用户建/改一个项目 token。token_id=0 新建并返回明文 key
// （tenant-plane 一次性展示）；token_id>0 更新既有 token（key 不变、不回显），且该 token
// 必须属于 user_id，否则 404——这是租户隔离的强制点。
func ProvisionTenantToken(c *gin.Context) {
	var req provisionTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		tenantError(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if req.UserId <= 0 {
		tenantError(c, http.StatusBadRequest, "user_id is required")
		return
	}
	if len(req.Name) > 50 {
		tenantError(c, http.StatusBadRequest, "token name too long (max 50)")
		return
	}
	if !req.UnlimitedQuota {
		if req.RemainQuota < 0 {
			tenantError(c, http.StatusBadRequest, "remain_quota must be >= 0")
			return
		}
		maxQuotaValue := int(1000000000 * common.QuotaPerUnit)
		if req.RemainQuota > maxQuotaValue {
			tenantError(c, http.StatusBadRequest, "remain_quota exceeds max")
			return
		}
	}
	// 校验组织计费用户存在，避免生成孤儿 token。
	if _, err := model.GetUserById(req.UserId, false); err != nil {
		tenantError(c, http.StatusNotFound, "billing user not found")
		return
	}

	expiredTime := req.ExpiredTime
	if expiredTime == 0 {
		expiredTime = -1 // 永不过期
	}
	var allowIps *string
	if strings.TrimSpace(req.AllowIps) != "" {
		v := req.AllowIps
		allowIps = &v
	}

	// 更新既有项目 token：GetTokenByIds 强制归属 user_id（隔离），key 不变、不回显。
	if req.TokenId > 0 {
		token, err := model.GetTokenByIds(req.TokenId, req.UserId)
		if err != nil || token == nil {
			tenantError(c, http.StatusNotFound, "token not found for user")
			return
		}
		token.Name = req.Name
		token.ExpiredTime = expiredTime
		token.RemainQuota = req.RemainQuota
		token.UnlimitedQuota = req.UnlimitedQuota
		token.ModelLimitsEnabled = req.ModelLimitsEnabled
		token.ModelLimits = req.ModelLimits
		token.AllowIps = allowIps
		token.Group = req.Group
		if err := token.Update(); err != nil {
			tenantError(c, http.StatusInternalServerError, err.Error())
			return
		}
		tenantOK(c, gin.H{"token_id": token.Id, "user_id": token.UserId, "key": ""})
		return
	}

	// 新建项目 token：尊重 per-user token 上限（不绕过 new-api 限制）。
	maxTokens := operation_setting.GetMaxUserTokens()
	count, err := model.CountUserTokens(req.UserId)
	if err != nil {
		tenantError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if int(count) >= maxTokens {
		tenantError(c, http.StatusConflict, "user reached max token count")
		return
	}
	key, err := common.GenerateKey()
	if err != nil {
		tenantError(c, http.StatusInternalServerError, "failed to generate key")
		return
	}
	token := model.Token{
		UserId:             req.UserId,
		Name:               req.Name,
		Key:                key,
		CreatedTime:        common.GetTimestamp(),
		AccessedTime:       common.GetTimestamp(),
		ExpiredTime:        expiredTime,
		RemainQuota:        req.RemainQuota,
		UnlimitedQuota:     req.UnlimitedQuota,
		ModelLimitsEnabled: req.ModelLimitsEnabled,
		ModelLimits:        req.ModelLimits,
		AllowIps:           allowIps,
		Group:              req.Group,
	}
	if err := token.Insert(); err != nil {
		tenantError(c, http.StatusInternalServerError, err.Error())
		return
	}
	tenantOK(c, gin.H{"token_id": token.Id, "user_id": token.UserId, "key": key})
}
