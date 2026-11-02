package eventbus_test

import (
	"context"
	"testing"
	"time"

	"ziniao/eventbus"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
)

// recHandler 记录调用次数的处理器替身，可控失败。
type recHandler struct {
	name  string
	types []string
	calls int
	fail  bool
}

func (h *recHandler) Name() string    { return h.name }
func (h *recHandler) Types() []string { return h.types }
func (h *recHandler) Handle(_ context.Context, _ *eventbus.Event) error {
	h.calls++
	if h.fail {
		return context.DeadlineExceeded
	}
	return nil
}

func newBusEnv(t *testing.T) (*mem.Store, *eventbus.Bus) {
	t.Helper()
	st := mem.New()
	cfg := config.Default()
	return st, eventbus.New(st, cfg, nil)
}

func emitOne(t *testing.T, st *mem.Store, id string) {
	t.Helper()
	err := st.WithTx(context.Background(), func(tx store.Tx) error {
		return tx.Outbox().Insert(context.Background(), &types.OutboxEnvelope{
			ID: id, TenantID: "t1", Type: "test.event", Payload: []byte(`{}`),
			Status: "pending", AvailableAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPartialProgressSurvivesRedelivery A 成功 + B 失败 → 事件重投时 A 不再执行。
//
// 这是接收侧投递幂等表（event_consumptions）的核心语义：Handle 成功到 Ack 之间
// 崩溃（或另一个处理器失败把整条事件打回 pending）时，重投必须从断点继续，
// 而不是让成功的处理器把副作用再来一遍——notify 会因此发出两份通知，
// audit 会撞审计主键（entry.ID = event.ID）。
func TestPartialProgressSurvivesRedelivery(t *testing.T) {
	st, bus := newBusEnv(t)
	a := &recHandler{name: "a", calls: 0}
	b := &recHandler{name: "b", fail: true}
	bus.Register(a)
	bus.Register(b)
	emitOne(t, st, "ev-1")

	if _, err := bus.DispatchOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("首轮应各执行一次: a=%d b=%d", a.calls, b.calls)
	}

	// 重投（Fail 之后的退避是秒级，这里直接等过退避窗口）
	b.fail = false
	time.Sleep(1200 * time.Millisecond)
	if _, err := bus.DispatchOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 {
		t.Fatalf("重投后 a 不应再次执行（已消费登记），实际 %d 次", a.calls)
	}
	if b.calls != 2 {
		t.Fatalf("重投后 b 应补跑一次，实际 %d 次", b.calls)
	}
}

// TestConsumedSkipOnDuplicateDispatch 已登记消费的事件在重投时整体跳过。
func TestConsumedSkipOnDuplicateDispatch(t *testing.T) {
	st, bus := newBusEnv(t)
	h := &recHandler{name: "h"}
	bus.Register(h)
	emitOne(t, st, "ev-2")

	if _, err := bus.DispatchOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 模拟「处理成功但 Ack 丢失」：把信封手动打回 pending 再派发一次
	ctx := store.WithTenant(context.Background(), "t1")
	if err := st.Outbox().Insert(ctx, &types.OutboxEnvelope{ID: "ev-2", TenantID: "t1",
		Type: "test.event", Payload: []byte(`{}`), Status: "pending",
		AvailableAt: time.Now().UTC(), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// mem 的 Insert 是 append（同一 id 会出现两行），派发会各跑一次；
	// 消费登记保证第二行也不会再次触发处理器副作用。
	if _, err := bus.DispatchOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.calls != 1 {
		t.Fatalf("重复派发不应重复执行处理器，实际 %d 次", h.calls)
	}
}
