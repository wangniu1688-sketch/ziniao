package shop_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"ziniao/platform/store"
	"ziniao/platform/types"
	"ziniao/shop"
)

func TestProxyProbeHistoryIsTenantScopedAndLimitedTo100(t *testing.T) {
	s := newProbeSuite(t)
	ctx := context.Background()
	proxyID := s.seedProxy(t, "历史测试", tunnelProxy(t), "history-secret", "US")
	tenantCtx := store.WithTenant(ctx, s.actor.TenantID)
	base := time.Now().UTC().Add(-2 * time.Minute)

	for i := 0; i < 105; i++ {
		status := types.ProxyCheckOK
		if i%2 == 0 {
			status = types.ProxyCheckFail
		}
		err := s.st.Proxies().AppendProbeHistory(tenantCtx, &types.ProxyProbeHistory{
			ID: fmt.Sprintf("probe-%03d", i), ProxyID: proxyID,
			CheckStatus: status, StatusCode: 200, LatencyMS: i,
			Error: "safe summary", CheckedAt: base.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("append history %d: %v", i, err)
		}
	}

	items, err := s.st.Proxies().ListProbeHistory(tenantCtx, proxyID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 100 {
		t.Fatalf("expected capped 100 rows, got %d", len(items))
	}
	if items[0].ID != "probe-104" || items[len(items)-1].ID != "probe-005" {
		t.Fatalf("history must be newest-first and capped: first=%s last=%s", items[0].ID, items[len(items)-1].ID)
	}

	otherTenant := store.WithTenant(ctx, "another-tenant")
	foreign, err := s.st.Proxies().ListProbeHistory(otherTenant, proxyID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 0 {
		t.Fatalf("history leaked across tenants: %+v", foreign)
	}
}


func TestProxyHistoryServiceReturnsLiveProbeRecord(t *testing.T) {
	s := newProbeSuite(t)
	proxyID := s.seedProxy(t, "历史落库", tunnelProxy(t), "", "US")
	res, err := s.proxies.Test(context.Background(), shop.ProxyTestInput{
		ProxyID: proxyID, Target: ipEchoTarget(t), TimeoutMS: 3000, Actor: s.actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.StatusCode != 200 {
		t.Fatalf("expected successful HTTP 200 probe, got %+v", res)
	}
	history, err := s.proxies.History(context.Background(), s.actor, proxyID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].CheckStatus != types.ProxyCheckOK || history[0].StatusCode != 200 {
		t.Fatalf("unexpected history result: %+v", history)
	}
}

func TestProxyHistoryServiceChecksProxyOwnership(t *testing.T) {
	s := newProbeSuite(t)
	_, err := s.proxies.History(context.Background(), s.actor, "not-owned-by-tenant", 100)
	if err == nil {
		t.Fatal("history lookup for missing proxy must fail")
	}
}
