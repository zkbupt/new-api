package model

import (
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// WalletTransaction 是数据库权威的钱包交易台账。
//
// 它为 A/B 两条数据面提供统一的原子钱包写入口：reserve 冻结、settle 结算、
// rollback 回滚，全部在数据库事务内完成，request_id 唯一键负责跨重试幂等，
// 用户余额条件扣减与本台账在同一事务提交。DB 是余额真相源，Redis 仅为缓存。
//
// 见 AI 中转站技术实施计划 v2 §2.0-2.3。
type WalletTransaction struct {
	Id            int    `json:"id"`
	RequestId     string `json:"request_id" gorm:"type:varchar(64);uniqueIndex"`
	UserId        int    `json:"user_id" gorm:"index"`
	TokenId       int    `json:"token_id" gorm:"index"`
	Status        string `json:"status" gorm:"type:varchar(32);index"` // reserved/settled/rolled_back/expired
	ReservedQuota int    `json:"reserved_quota" gorm:"type:int;not null;default:0"`
	SettledQuota  int    `json:"settled_quota" gorm:"type:int;not null;default:0"`
	CreatedAt     int64  `json:"created_at" gorm:"bigint;index"`
	ExpiresAt     int64  `json:"expires_at" gorm:"bigint;index"`
	SettledAt     int64  `json:"settled_at" gorm:"bigint"`
	Version       int    `json:"version" gorm:"not null;default:0"`
	UsageMeta     string `json:"usage_meta" gorm:"type:text"` // JSON，经 common.Marshal 写入
}

const (
	WalletStatusReserved   = "reserved"
	WalletStatusSettled    = "settled"
	WalletStatusRolledBack = "rolled_back"
	WalletStatusExpired    = "expired"
)

// ErrWalletInsufficientQuota 表示用户余额不足以完成预留。
var ErrWalletInsufficientQuota = errors.New("wallet: insufficient quota")

// ErrWalletAlreadySettled 表示交易已结算，不能再回滚。
var ErrWalletAlreadySettled = errors.New("wallet: transaction already settled")

// ErrWalletNotFound 表示按 request_id 找不到交易。
var ErrWalletNotFound = errors.New("wallet: transaction not found")

func (w *WalletTransaction) BeforeCreate(tx *gorm.DB) error {
	if w.CreatedAt == 0 {
		w.CreatedAt = common.GetTimestamp()
	}
	return nil
}

// ValidateWalletBalance 只读校验：用户当前 DB 余额是否 >= amount。
func ValidateWalletBalance(userId int, amount int) (bool, int, error) {
	if userId <= 0 {
		return false, 0, errors.New("wallet: invalid userId")
	}
	var user User
	if err := DB.Select("quota").Where("id = ?", userId).First(&user).Error; err != nil {
		return false, 0, err
	}
	return user.Quota >= amount, user.Quota, nil
}

// ReserveWallet 原子预留：条件检查余额并冻结额度，写入 reserved 台账。
// 重复 request_id 幂等返回首次交易，不重复扣减。
func ReserveWallet(requestId string, userId int, tokenId int, amount int, expiresAt int64, usageMeta string) (*WalletTransaction, error) {
	if strings.TrimSpace(requestId) == "" {
		return nil, errors.New("wallet: requestId is empty")
	}
	if userId <= 0 {
		return nil, errors.New("wallet: invalid userId")
	}
	if amount <= 0 {
		return nil, errors.New("wallet: amount must be > 0")
	}

	result := &WalletTransaction{}
	err := DB.Transaction(func(tx *gorm.DB) error {
		var existing WalletTransaction
		q := tx.Where("request_id = ?", requestId).Limit(1).Find(&existing)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected > 0 {
			// 幂等重入：请求重试命中已提交的交易，返回首次结果，不重复扣减。
			*result = existing
			return nil
		}

		var user User
		if err := lockForUpdate(tx).Where("id = ?", userId).First(&user).Error; err != nil {
			return err
		}
		if user.Quota < amount {
			return ErrWalletInsufficientQuota
		}

		// 先写台账、后扣余额：uniqueIndex 冲突（并发同 request_id）时 Create 失败，
		// 整个事务回滚，余额未动、无副作用；调用方重试会命中上面的幂等分支。
		// 顺序反过来会在冲突时留下已扣减的余额（双扣），故必须 Create 在前。
		record := &WalletTransaction{
			RequestId:     requestId,
			UserId:        userId,
			TokenId:       tokenId,
			Status:        WalletStatusReserved,
			ReservedQuota: amount,
			ExpiresAt:     expiresAt,
			UsageMeta:     usageMeta,
			Version:       1,
		}
		if err := tx.Create(record).Error; err != nil {
			return err
		}
		if err := tx.Model(&User{}).Where("id = ?", userId).
			Update("quota", gorm.Expr("quota - ?", amount)).Error; err != nil {
			return err
		}
		*result = *record
		return nil
	})
	if err != nil {
		return nil, err
	}
	invalidateWalletUserCache(userId)
	return result, nil
}

// SettleWallet 结算：按实际 quota 多退少补，幂等。重复结算返回首次结果。
func SettleWallet(requestId string, actualQuota int) (*WalletTransaction, error) {
	if strings.TrimSpace(requestId) == "" {
		return nil, errors.New("wallet: requestId is empty")
	}
	if actualQuota < 0 {
		return nil, errors.New("wallet: actualQuota must be >= 0")
	}

	result := &WalletTransaction{}
	var affectedUser int
	var delta int
	err := DB.Transaction(func(tx *gorm.DB) error {
		var record WalletTransaction
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrWalletNotFound
			}
			return err
		}
		if record.Status == WalletStatusSettled {
			*result = record // 幂等：已结算返回首次结果，不重复扣款。
			return nil
		}
		if record.Status == WalletStatusRolledBack || record.Status == WalletStatusExpired {
			return errors.New("wallet: cannot settle a rolled_back/expired transaction")
		}

		// delta = 实际 - 已冻结；>0 补扣，<0 退还。
		delta = actualQuota - record.ReservedQuota
		if delta > 0 {
			if err := tx.Model(&User{}).Where("id = ?", record.UserId).
				Update("quota", gorm.Expr("quota - ?", delta)).Error; err != nil {
				return err
			}
		} else if delta < 0 {
			if err := tx.Model(&User{}).Where("id = ?", record.UserId).
				Update("quota", gorm.Expr("quota + ?", -delta)).Error; err != nil {
				return err
			}
		}

		record.Status = WalletStatusSettled
		record.SettledQuota = actualQuota
		record.SettledAt = common.GetTimestamp()
		record.Version++
		if err := tx.Save(&record).Error; err != nil {
			return err
		}
		affectedUser = record.UserId
		*result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	if affectedUser > 0 && delta != 0 {
		invalidateWalletUserCache(affectedUser)
	}
	return result, nil
}

// RollbackWallet 回滚：只释放未结算的预留，已结算不可回滚。幂等。
func RollbackWallet(requestId string) (*WalletTransaction, error) {
	if strings.TrimSpace(requestId) == "" {
		return nil, errors.New("wallet: requestId is empty")
	}

	result := &WalletTransaction{}
	var refundedUser int
	err := DB.Transaction(func(tx *gorm.DB) error {
		var record WalletTransaction
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrWalletNotFound
			}
			return err
		}
		if record.Status == WalletStatusRolledBack || record.Status == WalletStatusExpired {
			*result = record // 幂等：已回滚/过期直接返回。
			return nil
		}
		if record.Status == WalletStatusSettled {
			return ErrWalletAlreadySettled
		}

		if record.ReservedQuota > 0 {
			if err := tx.Model(&User{}).Where("id = ?", record.UserId).
				Update("quota", gorm.Expr("quota + ?", record.ReservedQuota)).Error; err != nil {
				return err
			}
			refundedUser = record.UserId
		}
		record.Status = WalletStatusRolledBack
		record.Version++
		if err := tx.Save(&record).Error; err != nil {
			return err
		}
		*result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	if refundedUser > 0 {
		invalidateWalletUserCache(refundedUser)
	}
	return result, nil
}

// TopUpWalletReservation 原子调整未结算预留：delta>0 追加冻结（需余额充足），
// delta<0 释放部分预留（返还余额）。仅对 reserved 交易有效。用于 A 线请求中途
// 超出初始预留时的追加/回退（对应 BillingSession.reserveFunding/rollbackFundingReserve）。
func TopUpWalletReservation(requestId string, delta int) (*WalletTransaction, error) {
	if strings.TrimSpace(requestId) == "" {
		return nil, errors.New("wallet: requestId is empty")
	}
	if delta == 0 {
		return GetWalletTransaction(requestId)
	}

	result := &WalletTransaction{}
	var affectedUser int
	err := DB.Transaction(func(tx *gorm.DB) error {
		var record WalletTransaction
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrWalletNotFound
			}
			return err
		}
		if record.Status != WalletStatusReserved {
			return errors.New("wallet: can only top up a reserved transaction")
		}
		if delta > 0 {
			var user User
			if err := lockForUpdate(tx).Where("id = ?", record.UserId).First(&user).Error; err != nil {
				return err
			}
			if user.Quota < delta {
				return ErrWalletInsufficientQuota
			}
			if err := tx.Model(&User{}).Where("id = ?", record.UserId).
				Update("quota", gorm.Expr("quota - ?", delta)).Error; err != nil {
				return err
			}
			record.ReservedQuota += delta
		} else {
			refund := -delta
			if refund > record.ReservedQuota {
				refund = record.ReservedQuota // 不超还
			}
			if err := tx.Model(&User{}).Where("id = ?", record.UserId).
				Update("quota", gorm.Expr("quota + ?", refund)).Error; err != nil {
				return err
			}
			record.ReservedQuota -= refund
		}
		record.Version++
		if err := tx.Save(&record).Error; err != nil {
			return err
		}
		affectedUser = record.UserId
		*result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	if affectedUser > 0 {
		invalidateWalletUserCache(affectedUser)
	}
	return result, nil
}

// ListStaleReservedTransactions 返回 created_at < beforeUnix 且仍为 reserved 的交易，
// 供 Settlement Worker 对账 sweep：确认无 usage 后回滚过期预留（见实施计划 §2.2/§3.3）。
func ListStaleReservedTransactions(beforeUnix int64, limit int) ([]WalletTransaction, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	var rows []WalletTransaction
	err := DB.Where("status = ? AND created_at < ?", WalletStatusReserved, beforeUnix).
		Order("created_at asc").Limit(limit).Find(&rows).Error
	return rows, err
}

// GetWalletTransaction 按 request_id 读取交易。
func GetWalletTransaction(requestId string) (*WalletTransaction, error) {
	if strings.TrimSpace(requestId) == "" {
		return nil, errors.New("wallet: requestId is empty")
	}
	var record WalletTransaction
	if err := DB.Where("request_id = ?", requestId).First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWalletNotFound
		}
		return nil, err
	}
	return &record, nil
}

// invalidateWalletUserCache 在钱包事务提交后失效用户缓存，让 A 线优先读 Redis
// 的实时余额路径重新从 DB 加载。DB 已是权威，失效失败只记录不阻断。
func invalidateWalletUserCache(userId int) {
	if err := InvalidateUserCache(userId); err != nil {
		common.SysLog("wallet: failed to invalidate user cache: " + err.Error())
	}
}
