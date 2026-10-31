package shop

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"ziniao/eventbus"
	"ziniao/fingerprint"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// EnvService 店铺浏览器环境服务（M0.3 / M2.2）：
// 负责「按平台 × 站点采样画像 → 绑定内核与 TLS 基线 → 生成注入计划 → 自检报告」。
type EnvService struct {
	st       store.Store
	bus      *eventbus.Bus
	engine   *iam.Engine
	lib      *fingerprint.Library
	baseline *fingerprint.BaselineLibrary
}

// NewEnvService 构造环境服务。
func NewEnvService(st store.Store, bus *eventbus.Bus, engine *iam.Engine,
	lib *fingerprint.Library, baseline *fingerprint.BaselineLibrary) *EnvService {
	return &EnvService{st: st, bus: bus, engine: engine, lib: lib, baseline: baseline}
}

// CreateEnvInput 创建环境入参。
type CreateEnvInput struct {
	ShopID   string
	Country  string // 目标站点国家（决定画像时区/语言与代理出口国）
	OSFamily string // Windows|macOS|Linux，空表示不限
	DeviceID string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// CreateEnv 创建店铺环境：采样画像 → 自洽性体检 → 落库（含基线哈希）→ 事件。
func (s *EnvService) CreateEnv(ctx context.Context, in CreateEnvInput) (*types.Environment, *fingerprint.InjectionPlan, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:create", Path: "/shops"})
	if !d.Allow {
		return nil, nil, apierr.Forbidden("无权创建店铺环境（" + string(d.Layer) + "）")
	}
	profile, err := s.lib.Sample(in.Country, in.OSFamily)
	if err != nil {
		return nil, nil, apierr.Conflict(err.Error())
	}
	major, err := profile.ChromeMajor()
	if err != nil {
		return nil, nil, apierr.Internal("画像 UA 无法解析内核版本")
	}
	tls, hasTLS := s.baseline.For(major)
	// TLS/UA 自洽闸门：uTLS 基线滞后于 Chrome 正式版（v1.6.7 无 HelloChrome_131），
	// 131 实际复用 HelloChrome_120_PQ——即「发出的握手是 120」。
	// 若 UA 仍保持 131，平台交叉比对 UA 与 JA4 立刻识破，这是致命不自洽。
	// 处理原则：**宁可用低版本但完全自洽的组合，也不要高版本混搭**——把 UA 一起
	// 对齐到真正发出的版本，并把有效内核版本回写 defects（一致性体检/审计都用它）。
	if hasTLS && tls.HelloID != "" {
		if eff := fingerprint.ParseChromeMajorFromHelloID(tls.HelloID); eff > 0 && eff != major {
			if bl, ok := s.baseline.For(eff); ok {
				profile.UA = fingerprint.AlignUAChromeVersion(profile.UA, bl.ChromiumVersion)
				major = eff
				tls = bl
			}
		}
	}

	// 自洽性体检：创建环境时就要体检，不合格直接拒绝（M0.3）
	report := fingerprint.NewEngine().Evaluate(profile, fingerprint.CheckContext{
		IPCountry: in.Country, ProxyCountry: in.Country, KernelMajor: major, TLS: tls,
	})
	if len(report.Issues) > 0 && report.Score < report.Threshold {
		return nil, nil, apierr.Conflict("画像自洽性体检未通过（score=" + itoa(report.Score) + "），已拒绝创建")
	}
	rawReport, err := json.Marshal(report)
	if err != nil {
		return nil, nil, apierr.Internal("自洽性报告序列化失败")
	}
	plan, err := fingerprint.BuildInjectionPlan(ids.New(), profile, tls)
	if err != nil {
		return nil, nil, apierr.Internal(err.Error())
	}

	now := time.Now().UTC()
	env := &types.Environment{
		ID: plan.EnvID, TenantID: in.Actor.TenantID, ShopID: in.ShopID, DeviceID: in.DeviceID,
		ProfileID: profile.ID, BaselineHash: plan.BaselineHash, KernelMajor: major,
		Status: "created", Snapshot: fingerprint.Capture(profile).Values,
		Consistency: rawReport, InjectScript: plan.Script, CreatedAt: now, UpdatedAt: now,
	}
	if tls != nil {
		env.TLSHelloID = tls.HelloID
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Environments().Create(tctx, env); err != nil {
			return err
		}
		// L4：审计只记画像 ID 与基线哈希，不记任何凭证；L8：基线哈希是离线校验的唯一凭据
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.created", OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.create", Result: "success"},
			Data: map[string]any{
				"env_id": env.ID, "profile_id": profile.ID,
				"baseline_hash": env.BaselineHash, "kernel_major": major,
				"consistency_score": report.Score,
			},
		})
	}); err != nil {
		return nil, nil, err
	}
	return env, plan, nil
}

// LaunchSpec 环境启动单：客户端拿到后**原样执行**，不做二次加工。
//
// 红线：
//   - 注入脚本、UA、时区、TLS 基线全部由服务端下发，客户端随机化 = 与画像库脱钩（L2 致命项）；
//   - 不返回任何凭证（L4）；
//   - ProxyPort 由服务端分配端口池，客户端按此端口起 uTLS 前置代理，
//     保证"声称的内核版本"与"实际 TLS 指纹"同源。
type LaunchSpec struct {
	EnvID        string            `json:"env_id"`
	ShopID       string            `json:"shop_id"`
	ShopName     string            `json:"shop_name,omitempty"`
	BaselineHash string            `json:"baseline_hash"`
	KernelMajor  int               `json:"kernel_major"`
	UserAgent    string            `json:"user_agent"`
	ProxyPort    int               `json:"proxy_port"`
	TLSHelloID   string            `json:"tls_hello_id,omitempty"`
	Country      string            `json:"country,omitempty"`
	Script       string            `json:"script"`
	ScriptBytes  int               `json:"script_bytes"`
	OpenURL      string            `json:"open_url"`
	Snapshot     map[string]string `json:"snapshot,omitempty"`
}

// Launch 下发环境启动单（真实浏览器窗口的能力源）。
//
// 裁决仍在服务端（L3）：无 env:launch 权限直接拒绝；启动写审计但不含凭证。
func (s *EnvService) Launch(ctx context.Context, in LaunchInput) (*LaunchSpec, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:launch", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权启动店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	env, err := s.st.Environments().GetByShop(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	script := env.InjectScript
	if script == "" {
		// 兼容历史环境（建环境时尚未落脚本）：按快照重建，但标记为服务端补齐
		script = rebuildScript(env)
	}
	// 注入运行时标记：实测页/自检用（基线哈希是离线启动校验的唯一凭据，L8）
	script += "\n;window.__ZINIAO_BASELINE__=" + strconv.Quote(env.BaselineHash) + ";"
	var shopName string
	if sp, err2 := s.st.Shops().Get(tctx, in.ShopID); err2 == nil && sp != nil {
		shopName = sp.Name
	}
	spec := &LaunchSpec{
		EnvID:        env.ID,
		ShopID:       env.ShopID,
		ShopName:     shopName,
		BaselineHash: env.BaselineHash,
		KernelMajor:  env.KernelMajor,
		UserAgent:    env.Snapshot["ua"],
		ProxyPort:    proxyPortFor(env.ID),
		TLSHelloID:   env.TLSHelloID,
		Country:      env.Snapshot["country"],
		Script:       script,
		ScriptBytes:  len(script),
		OpenURL:      in.OpenURL,
		Snapshot:     env.Snapshot,
	}
	// 启动也是一次受管控的操作：写审计（不含凭证、不含脚本正文）
	now := time.Now().UTC()
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		env.Status = "running"
		env.UpdatedAt = now
		if err := tx.Environments().Update(tctx, env); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.launched", OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal, Context: eventbus.ContextInfo{URL: in.URL, Action: "env.launch", Result: "success"},
			Data: map[string]any{
				"env_id": spec.EnvID, "kernel_major": spec.KernelMajor,
				"baseline_hash": spec.BaselineHash, "script_bytes": spec.ScriptBytes,
				"proxy_port": spec.ProxyPort,
			},
		})
	}); err != nil {
		return nil, err
	}
	return spec, nil
}

// proxyPortFor 端口池分配：同环境恒定端口，便于本地代理复用与排障。
func proxyPortFor(envID string) int {
	h := uint32(2166136261)
	for i := 0; i < len(envID); i++ {
		h ^= uint32(envID[i])
		h *= 16777619 // uint32 自然回绕
	}
	return 20000 + int(h%20000)
}

// rebuildScript 历史环境补脚本：只做兜底，正常路径一律使用建环境时落库的原文。
func rebuildScript(env *types.Environment) string {
	if env == nil {
		return ""
	}
	cfg, err := json.Marshal(map[string]any{
		"ua":             env.Snapshot["ua"],
		"platform":       env.Snapshot["platform"],
		"timezone":       env.Snapshot["timezone"],
		"utc_offset":     env.Snapshot["utc_offset"],
		"languages":      env.Snapshot["languages"],
		"canvas_seed":    env.Snapshot["canvas_seed"],
		"audio_seed":     env.Snapshot["audio_seed"],
		"webgl_vendor":   env.Snapshot["webgl_vendor"],
		"webgl_renderer": env.Snapshot["webgl_renderer"],
		"compat":         true,
	})
	if err != nil {
		return ""
	}
	return "(function(p){window.__ZINIAO_COMPAT__=" + string(cfg) + ";})(window);"
}

// LaunchInput 启动环境入参。
type LaunchInput struct {
	ShopID   string
	OpenURL  string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// CheckReport 环境自检报告（M0.3 环境自检页）。
type CheckReport struct {
	EnvID           string                  `json:"env_id"`
	BaselineHash    string                  `json:"baseline_hash"`
	Consistency     fingerprint.Report      `json:"consistency"`
	Drift           fingerprint.DriftReport `json:"drift"`
	BaselineChecked bool                    `json:"baseline_checked"`
	BaselineMatch   bool                    `json:"baseline_match"`
	BaselineNote    string                  `json:"baseline_note,omitempty"`
	TLSMatch        bool                    `json:"tls_match"`
	Score           int                     `json:"score"`
	Passed          bool                    `json:"passed"`
}

// Check 环境自检：自洽性 + 漂移 + 基线校验（离线启动前必做，L8）。
//
// observed 为客户端上报的观测值：
//   - 与快照同名的键参与漂移检测；
//   - baseline_hash：离线启动校验用的基线哈希；
//   - tls_ja3：实际 TLS JA3，与内核基线比对。
func (s *EnvService) Check(ctx context.Context, tenantID, shopID string, observed map[string]string) (*CheckReport, error) {
	tctx := store.WithTenant(ctx, tenantID)
	env, err := s.st.Environments().GetByShop(tctx, shopID)
	if err != nil {
		return nil, err
	}
	if observed == nil {
		observed = map[string]string{}
	}
	out := &CheckReport{EnvID: env.ID, BaselineHash: env.BaselineHash}
	if len(env.Consistency) > 0 {
		_ = json.Unmarshal(env.Consistency, &out.Consistency)
	}
	// 漂移检测：观测值 vs 创建时基线快照
	out.Drift = fingerprint.DetectDrift(fingerprint.Snapshot{Values: env.Snapshot}, observed)

	// 离线启动校验（L8）：客户端必须上报与创建时一致的基线哈希
	if reported, ok := observed["baseline_hash"]; ok && reported != "" {
		out.BaselineChecked = true
		out.BaselineMatch = reported == env.BaselineHash
		if !out.BaselineMatch {
			out.BaselineNote = "基线哈希与服务端不一致，禁止离线启动"
		}
	} else {
		out.BaselineNote = "未上报基线哈希（离线启动前必须上报）"
	}
	// TLS 指纹校验：实际 JA3 必须与内核基线一致（TLS 与声称版本脱钩是致命项）
	out.TLSMatch = true
	if ja3, ok := observed["tls_ja3"]; ok && ja3 != "" {
		if bl, ok2 := s.baseline.For(env.KernelMajor); ok2 && bl.JA3 != "" && bl.JA3 != ja3 {
			out.TLSMatch = false
			out.BaselineNote = "TLS JA3 与内核基线不一致"
		}
	}

	score := out.Consistency.Score - (100-out.Drift.Score)/2
	if out.BaselineChecked && !out.BaselineMatch {
		score -= 20
	}
	if !out.TLSMatch {
		score -= 20
	}
	if score < 0 {
		score = 0
	}
	out.Score = score
	out.Passed = score >= 80 && out.Drift.Risk != "high" && out.TLSMatch &&
		(!out.BaselineChecked || out.BaselineMatch)
	return out, nil
}

// Get 取环境（按店铺）。
func (s *EnvService) Get(ctx context.Context, tenantID, shopID string) (*types.Environment, error) {
	return s.st.Environments().GetByShop(store.WithTenant(ctx, tenantID), shopID)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+i%10)) + out
		i /= 10
	}
	if neg {
		out = "-" + out
	}
	return out
}
