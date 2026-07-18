package model

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupWalletTest 迁移钱包表并返回一个持有指定余额的新用户 id。
// User 表已由包级 TestMain 迁移；此处只补迁 WalletTransaction。
func setupWalletTest(t *testing.T, quota int) int {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&WalletTransaction{}))
	seq := walletTestSeq.Add(1)
	u := &User{
		Username: fmt.Sprintf("wallet_u_%d", seq),
		AffCode:  fmt.Sprintf("waf%d", seq), // aff_code 是 uniqueIndex，必须唯一
		Quota:    quota,
		Status:   1,
	}
	require.NoError(t, DB.Create(u).Error)
	return u.Id
}

var walletTestSeq atomic.Int64

func userQuota(t *testing.T, userId int) int {
	t.Helper()
	var u User
	require.NoError(t, DB.Select("quota").Where("id = ?", userId).First(&u).Error)
	return u.Quota
}

func TestReserveWallet_DeductsAndRecords(t *testing.T) {
	uid := setupWalletTest(t, 1000)
	rid := fmt.Sprintf("req-%d", walletTestSeq.Add(1))

	txn, err := ReserveWallet(rid, uid, 7, 300, 0, "")
	require.NoError(t, err)
	assert.Equal(t, WalletStatusReserved, txn.Status)
	assert.Equal(t, 300, txn.ReservedQuota)
	assert.Equal(t, 700, userQuota(t, uid), "预留后余额应减少 300")
}

func TestReserveWallet_InsufficientQuota(t *testing.T) {
	uid := setupWalletTest(t, 100)
	rid := fmt.Sprintf("req-%d", walletTestSeq.Add(1))

	_, err := ReserveWallet(rid, uid, 0, 500, 0, "")
	require.ErrorIs(t, err, ErrWalletInsufficientQuota)
	assert.Equal(t, 100, userQuota(t, uid), "余额不足时不得扣减")
	_, gerr := GetWalletTransaction(rid)
	require.ErrorIs(t, gerr, ErrWalletNotFound, "失败的预留不应留下台账")
}

func TestReserveWallet_Idempotent(t *testing.T) {
	uid := setupWalletTest(t, 1000)
	rid := fmt.Sprintf("req-%d", walletTestSeq.Add(1))

	t1, err := ReserveWallet(rid, uid, 0, 300, 0, "")
	require.NoError(t, err)
	t2, err := ReserveWallet(rid, uid, 0, 300, 0, "")
	require.NoError(t, err)
	assert.Equal(t, t1.Id, t2.Id, "同 request_id 应返回同一交易")
	assert.Equal(t, 700, userQuota(t, uid), "重复预留只扣一次")
}

func TestSettleWallet_ChargeMoreThenRefund(t *testing.T) {
	// 实际 > 预留：补扣
	uid := setupWalletTest(t, 1000)
	rid := fmt.Sprintf("req-%d", walletTestSeq.Add(1))
	_, err := ReserveWallet(rid, uid, 0, 300, 0, "")
	require.NoError(t, err)
	s, err := SettleWallet(rid, 400)
	require.NoError(t, err)
	assert.Equal(t, WalletStatusSettled, s.Status)
	assert.Equal(t, 400, s.SettledQuota)
	assert.Equal(t, 600, userQuota(t, uid), "结算 400：1000-300(reserve)-100(补扣)")

	// 实际 < 预留：退还
	uid2 := setupWalletTest(t, 1000)
	rid2 := fmt.Sprintf("req-%d", walletTestSeq.Add(1))
	_, err = ReserveWallet(rid2, uid2, 0, 300, 0, "")
	require.NoError(t, err)
	_, err = SettleWallet(rid2, 120)
	require.NoError(t, err)
	assert.Equal(t, 880, userQuota(t, uid2), "结算 120：1000-300(reserve)+180(退还)")
}

func TestSettleWallet_Idempotent(t *testing.T) {
	uid := setupWalletTest(t, 1000)
	rid := fmt.Sprintf("req-%d", walletTestSeq.Add(1))
	_, err := ReserveWallet(rid, uid, 0, 300, 0, "")
	require.NoError(t, err)
	_, err = SettleWallet(rid, 400)
	require.NoError(t, err)
	s2, err := SettleWallet(rid, 400)
	require.NoError(t, err)
	assert.Equal(t, WalletStatusSettled, s2.Status)
	assert.Equal(t, 600, userQuota(t, uid), "重复结算不得重复扣款")
}

func TestRollbackWallet_ReleasesReservation(t *testing.T) {
	uid := setupWalletTest(t, 1000)
	rid := fmt.Sprintf("req-%d", walletTestSeq.Add(1))
	_, err := ReserveWallet(rid, uid, 0, 300, 0, "")
	require.NoError(t, err)
	r, err := RollbackWallet(rid)
	require.NoError(t, err)
	assert.Equal(t, WalletStatusRolledBack, r.Status)
	assert.Equal(t, 1000, userQuota(t, uid), "回滚应释放全部预留")
}

func TestRollbackWallet_Idempotent(t *testing.T) {
	uid := setupWalletTest(t, 1000)
	rid := fmt.Sprintf("req-%d", walletTestSeq.Add(1))
	_, err := ReserveWallet(rid, uid, 0, 300, 0, "")
	require.NoError(t, err)
	_, err = RollbackWallet(rid)
	require.NoError(t, err)
	_, err = RollbackWallet(rid)
	require.NoError(t, err)
	assert.Equal(t, 1000, userQuota(t, uid), "重复回滚只退一次")
}

func TestRollbackWallet_CannotRollbackSettled(t *testing.T) {
	uid := setupWalletTest(t, 1000)
	rid := fmt.Sprintf("req-%d", walletTestSeq.Add(1))
	_, err := ReserveWallet(rid, uid, 0, 300, 0, "")
	require.NoError(t, err)
	_, err = SettleWallet(rid, 300)
	require.NoError(t, err)
	_, err = RollbackWallet(rid)
	require.ErrorIs(t, err, ErrWalletAlreadySettled, "已结算不可回滚")
	assert.Equal(t, 700, userQuota(t, uid))
}

func TestValidateWalletBalance(t *testing.T) {
	uid := setupWalletTest(t, 500)
	ok, bal, err := ValidateWalletBalance(uid, 300)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 500, bal)
	ok, _, err = ValidateWalletBalance(uid, 600)
	require.NoError(t, err)
	assert.False(t, ok, "余额不足应返回 false")
}

// TestReserveWallet_ConcurrentNoOverdraft 校验并发预留不透支。
// SQLite 内存库单连接下事务串行化，验证的是"不透支"的逻辑不变量；
// 真实并发（100~500）以 PostgreSQL 集成测试为准（计划 §2.3）。
func TestReserveWallet_ConcurrentNoOverdraft(t *testing.T) {
	uid := setupWalletTest(t, 1000)
	const each = 100
	const n = 20 // 20*100 = 2000 需求 > 1000 余额，必有一半失败
	var success int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rid := fmt.Sprintf("conc-%d-%d", uid, i)
			if _, err := ReserveWallet(rid, uid, 0, each, 0, ""); err == nil {
				atomic.AddInt64(&success, 1)
			}
		}(i)
	}
	wg.Wait()
	bal := userQuota(t, uid)
	assert.GreaterOrEqual(t, bal, 0, "余额永不为负（不透支）")
	assert.Equal(t, 1000-int(success)*each, bal, "成功笔数与扣减一致")
	assert.LessOrEqual(t, int(success), 10, "最多 10 笔成功（1000/100）")
}
