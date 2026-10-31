package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"ziniao/platform/types"
)

// AuditHash 计算审计记录哈希：H(prev_hash || 规范化字段)。
// 形成 per-tenant 哈希链，任何一条被篡改都会导致后续全部校验失败（WORM 雏形）。
// TODO(P1)：把链尾哈希定期锚定到对象存储/外部时间戳服务，形成不可否认的取证链。
func AuditHash(prev string, e *types.AuditEntry) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte{0})
	h.Write([]byte(e.TenantID))
	h.Write([]byte{0})
	h.Write([]byte(string(e.Category)))
	h.Write([]byte{0})
	h.Write([]byte(e.ActorUserID))
	h.Write([]byte{0})
	h.Write([]byte(e.Action))
	h.Write([]byte{0})
	h.Write([]byte(e.Result))
	h.Write([]byte{0})
	h.Write([]byte(e.ShopID))
	h.Write([]byte{0})
	h.Write([]byte(e.DeviceID))
	h.Write([]byte{0})
	h.Write([]byte(e.TerminalIP))
	h.Write([]byte{0})
	h.Write([]byte(e.URL))
	h.Write([]byte{0})
	h.Write([]byte(fmt.Sprint(e.OccurredAt.UTC().UnixNano())))
	h.Write([]byte{0})
	h.Write(e.Data)
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyChain 校验同一租户内哈希链完整性。
func VerifyChain(entries []types.AuditEntry) (bool, int) {
	prev := ""
	for i, e := range entries {
		if e.PrevHash != prev {
			return false, i
		}
		if e.Hash != AuditHash(prev, &entries[i]) {
			return false, i
		}
		prev = e.Hash
	}
	return true, -1
}

// RetentionCutoff 按差异化留存策略返回清理截止点（永久留存类别不清理）。
func RetentionCutoff(retentionDays int, permanent []types.AuditCategory, now time.Time) (time.Time, []types.AuditCategory) {
	return now.AddDate(0, 0, -retentionDays), permanent
}
