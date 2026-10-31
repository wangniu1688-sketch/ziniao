// Package jobs 定时任务：设备健康巡检、回收站清理、审批单过期扫描。
//
// 设计原则：
//  1. 任务以「系统身份」执行，但每次执行都要留审计（L5 铁律同样约束自动化）；
//  2. 任务可单步执行（RunOnce），便于单测与 CLI 手动触发，避免"只能等定时"；
//  3. 每个任务按 Interval 节流，不会因为调度器频繁 tick 而重复报警。
package jobs

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Context 任务执行上下文：调度器解析出的团队列表与当前时间（便于单测固定时间）。
type Context struct {
	Tenants []TenantRef
	Now     time.Time
}

// TenantRef 团队引用（任务按租户隔离执行）。
type TenantRef struct {
	ID   string
	Name string
}

// Result 单次任务结果。
type Result struct {
	Job      string `json:"job"`
	TenantID string `json:"tenant_id,omitempty"`
	Scanned  int    `json:"scanned"`
	Affected int    `json:"affected"`
	Note     string `json:"note,omitempty"`
	Err      string `json:"error,omitempty"`
}

// Job 定时任务接口。
type Job interface {
	Name() string
	Interval() time.Duration
	Run(ctx context.Context, jc Context) []Result
}

// TenantLister 团队列表来源（定时任务需要跨租户执行）。
type TenantLister interface {
	List(ctx context.Context, limit, offset int) ([]TenantRef, int, error)
}

// Scheduler 内存调度器（无外部依赖；生产可替换为分布式调度，接口不变）。
type Scheduler struct {
	mu        sync.Mutex
	jobs      []Job
	lister    TenantLister
	log       *slog.Logger
	last      map[string]time.Time
	stopCh    chan struct{}
	stopped   bool
	batchSize int
}

// NewScheduler 构造调度器。
func NewScheduler(lister TenantLister, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{lister: lister, log: log, last: map[string]time.Time{}, stopCh: make(chan struct{}), batchSize: 200}
}

// Register 注册任务。
func (s *Scheduler) Register(jobs ...Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, jobs...)
}

// Names 已注册任务名（便于自检与 OpenAPI 描述）。
func (s *Scheduler) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j.Name())
	}
	sort.Strings(out)
	return out
}

// RunOnce 立即执行全部任务（忽略 Interval 节流）：单测、CLI 与手动触发用。
func (s *Scheduler) RunOnce(ctx context.Context) []Result {
	jc, err := s.context(ctx)
	if err != nil {
		return []Result{{Job: "scheduler", Err: err.Error()}}
	}
	out := []Result{}
	for _, j := range s.snapshot() {
		out = append(out, j.Run(ctx, jc)...)
		s.mu.Lock()
		s.last[j.Name()] = jc.Now
		s.mu.Unlock()
	}
	return out
}

// RunDue 执行已到期的任务（按 Interval 节流），由 Start 的 tick 调用。
func (s *Scheduler) RunDue(ctx context.Context) []Result {
	jc, err := s.context(ctx)
	if err != nil {
		return []Result{{Job: "scheduler", Err: err.Error()}}
	}
	out := []Result{}
	for _, j := range s.snapshot() {
		s.mu.Lock()
		last, ok := s.last[j.Name()]
		s.mu.Unlock()
		if ok && jc.Now.Sub(last) < j.Interval() {
			continue
		}
		out = append(out, j.Run(ctx, jc)...)
		s.mu.Lock()
		s.last[j.Name()] = jc.Now
		s.mu.Unlock()
	}
	return out
}

// Start 启动调度循环（阻塞调用方请用 goroutine）。
func (s *Scheduler) Start(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = 10 * time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			results := s.RunDue(ctx)
			for _, r := range results {
				if r.Err != "" {
					s.log.Error("定时任务失败", "job", r.Job, "tenant", r.TenantID, "err", r.Err)
					continue
				}
				s.log.Info("定时任务完成", "job", r.Job, "tenant", r.TenantID,
					"scanned", r.Scanned, "affected", r.Affected, "note", r.Note)
			}
		}
	}
}

// Stop 停止调度循环。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	close(s.stopCh)
}

func (s *Scheduler) snapshot() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, len(s.jobs))
	copy(out, s.jobs)
	return out
}

func (s *Scheduler) context(ctx context.Context) (Context, error) {
	tenants, _, err := s.lister.List(ctx, s.batchSize, 0)
	if err != nil {
		return Context{}, err
	}
	return Context{Tenants: tenants, Now: time.Now().UTC()}, nil
}
