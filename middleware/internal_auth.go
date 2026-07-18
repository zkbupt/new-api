package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
)

// 内部机器对机器（M2M）鉴权。用于 /api/internal/* 钱包接口，只在内部网络监听，
// 使用独立 shared secret；生产阶段升级为 mTLS（见实施计划 v2 §2.0/2.2）。
//
// 签名串覆盖 timestamp、nonce、method、path、body hash，使用常量时间比较、
// 时钟偏差检查及 Redis nonce 防重放；私网 CIDR 作为第二层限制。
const (
	internalTimestampHeader = "X-Wallet-Timestamp"
	internalNonceHeader     = "X-Wallet-Nonce"
	internalSignatureHeader = "X-Wallet-Signature"

	internalMaxClockSkew = 300 * time.Second
	internalNoncePrefix  = "wallet:nonce:"
)

// InternalWalletAuth 返回钱包内部接口的鉴权中间件。
func InternalWalletAuth() gin.HandlerFunc {
	secret := strings.TrimSpace(os.Getenv("WALLET_INTERNAL_SHARED_SECRET"))
	allowPublic := os.Getenv("WALLET_INTERNAL_ALLOW_PUBLIC") == "true"

	return func(c *gin.Context) {
		// 失败关闭：未配置密钥或仍是占位值则拒绝启动服务级鉴权。
		if secret == "" || strings.HasPrefix(secret, "change-me") {
			abortInternal(c, http.StatusServiceUnavailable, "wallet internal auth secret not configured")
			return
		}
		// 第二层：非私网来源默认拒绝（生产可 mTLS 后移除）。
		if !allowPublic && !isPrivateClient(c.ClientIP()) {
			abortInternal(c, http.StatusForbidden, "wallet internal api only accepts private-network clients")
			return
		}

		ts := c.GetHeader(internalTimestampHeader)
		nonce := c.GetHeader(internalNonceHeader)
		sig := c.GetHeader(internalSignatureHeader)
		if ts == "" || nonce == "" || sig == "" {
			abortInternal(c, http.StatusUnauthorized, "missing internal auth headers")
			return
		}

		tsInt, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			abortInternal(c, http.StatusUnauthorized, "invalid timestamp")
			return
		}
		skew := time.Duration(common.GetTimestamp()-tsInt) * time.Second
		if skew < 0 {
			skew = -skew
		}
		if skew > internalMaxClockSkew {
			abortInternal(c, http.StatusUnauthorized, "timestamp outside allowed skew")
			return
		}

		// 读取并复原 body，用于计算 body hash 且不影响后续 handler 绑定。
		var body []byte
		if c.Request.Body != nil {
			body, _ = io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
		}
		bodyHash := sha256.Sum256(body)
		signingString := strings.Join([]string{
			ts, nonce, c.Request.Method, c.Request.URL.Path, hex.EncodeToString(bodyHash[:]),
		}, "\n")

		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(signingString))
		expected := hex.EncodeToString(mac.Sum(nil))

		if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
			abortInternal(c, http.StatusUnauthorized, "signature mismatch")
			return
		}

		// nonce 防重放：SetNX 成功=首次；失败=重放。Redis 未启用时退化为仅时间窗口保护。
		if common.RedisEnabled {
			ok, err := common.RedisSetNX(internalNoncePrefix+nonce, "1", 2*internalMaxClockSkew)
			if err != nil {
				abortInternal(c, http.StatusServiceUnavailable, "nonce store unavailable")
				return
			}
			if !ok {
				abortInternal(c, http.StatusUnauthorized, "nonce already used (replay)")
				return
			}
		}

		c.Next()
	}
}

func abortInternal(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"success": false, "message": msg})
	c.Abort()
}

// isPrivateClient 判断来源是否为回环或 RFC1918/唯一本地地址（容器内网亦属私网）。
func isPrivateClient(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	return parsed.IsLoopback() || parsed.IsPrivate()
}

// InternalWalletSign 生成内部请求签名，供 Wallet Bridge / Settlement Worker 及测试复用。
func InternalWalletSign(secret, method, path string, body []byte, timestamp int64, nonce string) string {
	bodyHash := sha256.Sum256(body)
	signingString := fmt.Sprintf("%d\n%s\n%s\n%s\n%s", timestamp, nonce, method, path, hex.EncodeToString(bodyHash[:]))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingString))
	return hex.EncodeToString(mac.Sum(nil))
}
