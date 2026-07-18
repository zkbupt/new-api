package service

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var twfSeq atomic.Int64

func twfUser(t *testing.T, quota int) int {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(&model.WalletTransaction{}))
	require.NoError(t, model.DB.AutoMigrate(&model.User{}))
	seq := twfSeq.Add(1)
	u := &model.User{
		Username: fmt.Sprintf("twf_u_%d", seq),
		AffCode:  fmt.Sprintf("twf%d", seq),
		Quota:    quota,
		Status:   1,
	}
	require.NoError(t, model.DB.Create(u).Error)
	return u.Id
}

func twfQuota(t *testing.T, uid int) int {
	t.Helper()
	var u model.User
	require.NoError(t, model.DB.Select("quota").Where("id = ?", uid).First(&u).Error)
	return u.Quota
}

func TestTransactionalWalletFunding_ReserveSettleRefundOverConsume(t *testing.T) {
	// 实际 < 预留：多退少补退还
	uid := twfUser(t, 1000)
	rid := fmt.Sprintf("twf-%d", twfSeq.Add(1))
	f := &TransactionalWalletFunding{requestId: rid, userId: uid, tokenId: 7}
	require.NoError(t, f.PreConsume(300))
	assert.Equal(t, 700, twfQuota(t, uid), "预留 300 后余额")
	assert.Equal(t, 300, f.reserved)
	// Settle(delta=-130) → actual = 300-130 = 170，退还 130
	require.NoError(t, f.Settle(-130))
	assert.Equal(t, 830, twfQuota(t, uid), "结算 170：700+退130")
	txn, err := model.GetWalletTransaction(rid)
	require.NoError(t, err)
	assert.Equal(t, model.WalletStatusSettled, txn.Status)
	assert.Equal(t, 170, txn.SettledQuota)
}

func TestTransactionalWalletFunding_SettleChargeMore(t *testing.T) {
	// 实际 > 预留：补扣
	uid := twfUser(t, 1000)
	rid := fmt.Sprintf("twf-%d", twfSeq.Add(1))
	f := &TransactionalWalletFunding{requestId: rid, userId: uid, tokenId: 0}
	require.NoError(t, f.PreConsume(300))
	require.NoError(t, f.Settle(100)) // actual = 400
	assert.Equal(t, 600, twfQuota(t, uid), "结算 400：1000-300-100")
}

func TestTransactionalWalletFunding_Refund(t *testing.T) {
	uid := twfUser(t, 1000)
	rid := fmt.Sprintf("twf-%d", twfSeq.Add(1))
	f := &TransactionalWalletFunding{requestId: rid, userId: uid, tokenId: 0}
	require.NoError(t, f.PreConsume(300))
	require.NoError(t, f.Refund())
	assert.Equal(t, 1000, twfQuota(t, uid), "退款释放全部预留")
	txn, err := model.GetWalletTransaction(rid)
	require.NoError(t, err)
	assert.Equal(t, model.WalletStatusRolledBack, txn.Status)
}

func TestTopUpWalletReservation(t *testing.T) {
	uid := twfUser(t, 1000)
	rid := fmt.Sprintf("twf-%d", twfSeq.Add(1))
	_, err := model.ReserveWallet(rid, uid, 0, 300, 0, "")
	require.NoError(t, err)
	assert.Equal(t, 700, twfQuota(t, uid))
	// 追加 200
	txn, err := model.TopUpWalletReservation(rid, 200)
	require.NoError(t, err)
	assert.Equal(t, 500, twfQuota(t, uid), "追加 200 后余额")
	assert.Equal(t, 500, txn.ReservedQuota)
	// 回退 100
	txn, err = model.TopUpWalletReservation(rid, -100)
	require.NoError(t, err)
	assert.Equal(t, 600, twfQuota(t, uid), "回退 100 后余额")
	assert.Equal(t, 400, txn.ReservedQuota)
}

func TestTopUpWalletReservation_InsufficientQuota(t *testing.T) {
	uid := twfUser(t, 100)
	rid := fmt.Sprintf("twf-%d", twfSeq.Add(1))
	_, err := model.ReserveWallet(rid, uid, 0, 100, 0, "")
	require.NoError(t, err) // 余额 0
	_, err = model.TopUpWalletReservation(rid, 50)
	require.ErrorIs(t, err, model.ErrWalletInsufficientQuota, "余额不足追加应失败")
	assert.Equal(t, 0, twfQuota(t, uid), "失败不改余额")
}
