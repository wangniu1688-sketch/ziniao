package shop_test

import (
	"testing"

	"ziniao/shop"
)

// TestLifecycleTransitions 生命周期状态机（表驱动）：只允许定义过的迁移。
func TestLifecycleTransitions(t *testing.T) {
	cases := []struct {
		from    string
		to      string
		wantErr bool
	}{
		{"", shop.StateRegistering, false},
		{shop.StateRegistering, shop.StateNurturing, false},
		{shop.StateRegistering, shop.StateOperating, true}, // 注册中不能直接跳正常运营
		{shop.StateNurturing, shop.StateOperating, false},
		{shop.StateOperating, shop.StateReviewing, false},
		{shop.StateOperating, shop.StateRestricted, false},
		{shop.StateOperating, shop.StateAbandoned, false},
		{shop.StateAbandoned, shop.StateRegistering, false},
		{shop.StateAbandoned, shop.StateOperating, true}, // 已弃用不能直接恢复运营
		{shop.StateOperating, "", true},                  // 目标状态不能为空
		{"未知状态", shop.StateOperating, true},
	}
	for _, tc := range cases {
		err := shop.CanTransition(tc.from, tc.to)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s → %s 期望 err=%v，实际 %v", tc.from, tc.to, tc.wantErr, err)
		}
	}
}

// TestRequiredEnvForState 注册环境必须用全新住宅 IP + 高纯净度。
func TestRequiredEnvForState(t *testing.T) {
	minPurity, residential := shop.RequiredEnvForState(shop.StateRegistering)
	if !residential || minPurity < 80 {
		t.Fatalf("注册环境应要求住宅 IP 且纯净度门槛 ≥80，实际 %d/%v", minPurity, residential)
	}
	minPurity2, residential2 := shop.RequiredEnvForState(shop.StateOperating)
	if residential2 {
		t.Fatal("正常运营不应强制住宅 IP")
	}
	if minPurity2 >= minPurity {
		t.Fatal("正常运营门槛应低于注册环境")
	}
}
