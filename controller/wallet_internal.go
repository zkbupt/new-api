package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// 钱包内部接口（/api/internal/wallet/*）。仅供 Wallet Bridge / Settlement Worker
// 经内部 M2M 鉴权调用，A/B 两线共享同一原子钱包写入口。见实施计划 v2 §2.2。

type walletReserveRequest struct {
	RequestId string          `json:"request_id"`
	UserId    int             `json:"user_id"`
	TokenId   int             `json:"token_id"`
	Amount    int             `json:"amount"`
	ExpiresAt int64           `json:"expires_at"`
	UsageMeta json.RawMessage `json:"usage_meta"`
}

type walletSettleRequest struct {
	RequestId   string `json:"request_id"`
	ActualQuota int    `json:"actual_quota"`
}

type walletRollbackRequest struct {
	RequestId string `json:"request_id"`
}

type walletValidateRequest struct {
	UserId int `json:"user_id"`
	Amount int `json:"amount"`
}

func walletError(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"success": false, "message": msg})
}

func walletOK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// mapWalletError 把领域错误映射到 HTTP 状态码，使 Bridge 可据此决策。
func mapWalletError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrWalletInsufficientQuota):
		walletError(c, http.StatusPaymentRequired, err.Error())
	case errors.Is(err, model.ErrWalletNotFound):
		walletError(c, http.StatusNotFound, err.Error())
	case errors.Is(err, model.ErrWalletAlreadySettled):
		walletError(c, http.StatusConflict, err.Error())
	default:
		walletError(c, http.StatusBadRequest, err.Error())
	}
}

func WalletValidate(c *gin.Context) {
	var req walletValidateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		walletError(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	ok, balance, err := model.ValidateWalletBalance(req.UserId, req.Amount)
	if err != nil {
		mapWalletError(c, err)
		return
	}
	walletOK(c, gin.H{"ok": ok, "balance": balance})
}

func WalletReserve(c *gin.Context) {
	var req walletReserveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		walletError(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	usageMeta := ""
	if len(req.UsageMeta) > 0 {
		usageMeta = string(req.UsageMeta)
	}
	txn, err := model.ReserveWallet(req.RequestId, req.UserId, req.TokenId, req.Amount, req.ExpiresAt, usageMeta)
	if err != nil {
		mapWalletError(c, err)
		return
	}
	walletOK(c, txn)
}

func WalletSettle(c *gin.Context) {
	var req walletSettleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		walletError(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	txn, err := model.SettleWallet(req.RequestId, req.ActualQuota)
	if err != nil {
		mapWalletError(c, err)
		return
	}
	walletOK(c, txn)
}

func WalletRollback(c *gin.Context) {
	var req walletRollbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		walletError(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	txn, err := model.RollbackWallet(req.RequestId)
	if err != nil {
		mapWalletError(c, err)
		return
	}
	walletOK(c, txn)
}

func WalletGetTransaction(c *gin.Context) {
	requestId := strings.TrimSpace(c.Param("request_id"))
	if requestId == "" {
		walletError(c, http.StatusBadRequest, "request_id is required")
		return
	}
	txn, err := model.GetWalletTransaction(requestId)
	if err != nil {
		mapWalletError(c, err)
		return
	}
	walletOK(c, txn)
}

type walletStaleReservationsRequest struct {
	BeforeUnix int64 `json:"before_unix"`
	Limit      int   `json:"limit"`
}

// WalletStaleReservations 列出 created_at < before_unix 的 reserved 交易，供 Settlement
// Worker 对账 sweep（确认无 usage 后回滚过期预留）。
func WalletStaleReservations(c *gin.Context) {
	var req walletStaleReservationsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		walletError(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if req.BeforeUnix <= 0 {
		walletError(c, http.StatusBadRequest, "before_unix must be > 0")
		return
	}
	rows, err := model.ListStaleReservedTransactions(req.BeforeUnix, req.Limit)
	if err != nil {
		walletError(c, http.StatusInternalServerError, err.Error())
		return
	}
	walletOK(c, rows)
}

type walletResolveKeyRequest struct {
	APIKey string `json:"api_key"`
}

// WalletResolveKey 把外部 New API Key 解析为 user_id/token_id，供 Wallet Bridge
// 在预留前确定钱包归属。复用 model.ValidateUserToken 的标准校验语义（状态/过期/
// token 配额），与 A 线一致；密钥→用户归属留在 New API。禁止记录明文 key。
func WalletResolveKey(c *gin.Context) {
	var req walletResolveKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		walletError(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	key := normalizeWalletAPIKey(req.APIKey)
	if key == "" {
		walletError(c, http.StatusUnauthorized, "empty api key")
		return
	}
	token, err := model.ValidateUserToken(key)
	if err != nil || token == nil {
		walletError(c, http.StatusUnauthorized, "invalid api key")
		return
	}
	walletOK(c, gin.H{"user_id": token.UserId, "token_id": token.Id})
}

// normalizeWalletAPIKey 与 middleware/auth.go 的 key 规范化保持一致：
// 去 Bearer/sk- 前缀，按 "-" 切分取首段。
func normalizeWalletAPIKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "Bearer ") || strings.HasPrefix(raw, "bearer ") {
		raw = strings.TrimSpace(raw[7:])
	}
	raw = strings.TrimPrefix(raw, "sk-")
	return strings.Split(raw, "-")[0]
}
