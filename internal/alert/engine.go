// Package alert 实现跨设备/跨点位告警引擎:采集 DataPoint 经反向索引找到引用它的告警规则,
// 更新规则状态(各引用点最新值),expr 求值为 true 且未在告警态时边沿触发告警
// (写告警表 + 定向投递到规则指定的输出)。规则配置存 SQLite(alert_rules 表),
// 随 store.OnChange 热重载,范式与 processing.Engine 一致(见 docs/edge-computing-design.md)。
package alert

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/store"
)

const defaultFreshnessSeconds = 300

// pointKey 定位一个引用点位:设备 + 点位。
type pointKey struct {
	deviceID string
	point    string
}

// pointValue 是某引用点位的最新采集值与时间戳。
type pointValue struct {
	value interface{}
	ts    time.Time
}

// compiledRule 是一条已编译的告警规则及其运行态。
type compiledRule struct {
	rule          model.AlertRule
	program       *vm.Program
	env           map[string]any
	state         map[pointKey]pointValue
	refs          []pointKey
	active        bool   // 当前是否处于告警态(边沿触发:仅状态翻转时动作)
	activeAlertID string // 当前告警态对应的告警 ID(解除时据此更新表)
	lastResolved  time.Time
	freshness     time.Duration
	cooldown      time.Duration
}

// Engine 是告警引擎。Process 由 pipeline 单 goroutine 调用,Run/reload 由 OnChange 监听 goroutine
// 调用,三者共用 e.mu 保护 rules/index 与各规则的运行态。
type Engine struct {
	store     *store.Store
	mgr       *output.Manager
	gatewayID string

	mu    sync.Mutex
	rules map[string]*compiledRule
	index map[pointKey][]string
}

// NewEngine 构造告警引擎。mgr 用于数据继续扇出(Publish)与告警定向投递(PublishAlertTo)。
func NewEngine(st *store.Store, mgr *output.Manager, gatewayID string) *Engine {
	return &Engine{
		store:     st,
		mgr:       mgr,
		gatewayID: gatewayID,
		rules:     make(map[string]*compiledRule),
		index:     make(map[pointKey][]string),
	}
}

// Run 阻塞运行:启动加载规则,随后监听配置变更热重载。ctx 取消时返回。
func (e *Engine) Run(ctx context.Context) {
	if err := e.reload(); err != nil {
		slog.Error("alert initial reload failed", "err", err)
	}
	changeCh, cancel := e.store.OnChange()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case <-changeCh:
			if err := e.reload(); err != nil {
				slog.Error("alert reload failed", "err", err)
			}
		}
	}
}

// Process 处理单个采集 DataPoint:更新命中规则的状态,边沿触发/解除告警,最后让数据继续扇出。
// 无规则引用该点位时直接扇出,零开销。
func (e *Engine) Process(dp model.DataPoint) {
	pk := pointKey{dp.DeviceID, dp.Point}
	e.mu.Lock()
	ruleIDs := e.index[pk]
	if len(ruleIDs) == 0 {
		e.mu.Unlock()
		e.mgr.Publish(dp)
		return
	}
	now := time.Now()
	actions := e.evaluateRulesLocked(pk, dp, now)
	e.mu.Unlock()

	for _, act := range actions {
		if act.resolve {
			e.resolveAlert(act.alertID, now)
		} else {
			e.fireAlert(act.rule, act.alertID, act.context, now)
		}
	}
	e.mgr.Publish(dp)
}

// action 是一次边沿翻转产生的待执行动作(锁外执行写表与投递,避免持锁 IO)。
type action struct {
	rule     *compiledRule
	alertID  string
	context  []model.AlertContext
	resolve  bool
}

// evaluateRulesLocked 更新命中规则状态并产出边沿翻转动作。须持有 e.mu。
func (e *Engine) evaluateRulesLocked(pk pointKey, dp model.DataPoint, now time.Time) []action {
	var actions []action
	for _, rid := range e.index[pk] {
		cr := e.rules[rid]
		if cr == nil {
			continue
		}
		cr.state[pk] = pointValue{value: dp.Value, ts: now}
		if !e.allFreshLocked(cr, now) {
			continue
		}
		fired, err := e.evaluateLocked(cr)
		if err != nil {
			slog.Warn("alert expr eval failed", "rule", cr.rule.ID, "err", err)
			continue
		}
		switch {
		case fired && !cr.active:
			if cr.cooldown > 0 && now.Sub(cr.lastResolved) < cr.cooldown {
				continue // 解除后 cooldown 内抑制重触发,防抖
			}
			alertID := newAlertID()
			cr.active = true
			cr.activeAlertID = alertID
			actions = append(actions, action{rule: cr, alertID: alertID, context: buildContextLocked(cr)})
		case !fired && cr.active:
			alertID := cr.activeAlertID
			cr.active = false
			cr.activeAlertID = ""
			cr.lastResolved = now
			actions = append(actions, action{rule: cr, alertID: alertID, resolve: true})
		}
	}
	return actions
}

// allFreshLocked 检查规则所有引用点是否都已收到且在新鲜度窗口内。须持有 e.mu。
func (e *Engine) allFreshLocked(cr *compiledRule, now time.Time) bool {
	for _, pk := range cr.refs {
		pv, ok := cr.state[pk]
		if !ok || now.Sub(pv.ts) > cr.freshness {
			return false
		}
	}
	return true
}

// evaluateLocked 求值规则表达式;env 的 point 函数从 state 查最新值。须持有 e.mu。
func (e *Engine) evaluateLocked(cr *compiledRule) (bool, error) {
	result, err := expr.Run(cr.program, cr.env)
	if err != nil {
		return false, err
	}
	b, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("expr result not bool: %T", result)
	}
	return b, nil
}

// buildContextLocked 构造触发瞬间各引用点的值快照。须持有 e.mu。
func buildContextLocked(cr *compiledRule) []model.AlertContext {
	ctx := make([]model.AlertContext, 0, len(cr.refs))
	for _, pk := range cr.refs {
		pv, ok := cr.state[pk]
		if !ok {
			continue
		}
		ctx = append(ctx, model.AlertContext{
			DeviceID: pk.deviceID, Point: pk.point, Value: pv.value, Timestamp: pv.ts,
		})
	}
	return ctx
}

// fireAlert 写告警表并定向投递告警消息到规则指定的输出。
func (e *Engine) fireAlert(cr *compiledRule, alertID string, context []model.AlertContext, now time.Time) {
	message := cr.rule.Name + " 触发"
	msg := model.AlertMessage{
		AlertID:     alertID,
		RuleID:      cr.rule.ID,
		RuleName:    cr.rule.Name,
		Severity:    cr.rule.Severity,
		Message:     message,
		TriggeredAt: now,
		GatewayID:   e.gatewayID,
		Context:     context,
	}
	alert := model.Alert{
		AlertID: alertID, RuleID: cr.rule.ID, RuleName: cr.rule.Name,
		Severity: cr.rule.Severity, Message: message,
		TriggeredAt: now, GatewayID: e.gatewayID, Context: context,
		Status: string(model.AlertPending),
	}
	if err := e.store.SaveAlert(alert); err != nil {
		slog.Error("save alert failed", "rule", cr.rule.ID, "err", err)
	}
	e.mgr.PublishAlertTo(cr.rule.OutputIDs, msg)
}

// resolveAlert 把告警记录置为 resolved 并记解除时间。
func (e *Engine) resolveAlert(alertID string, now time.Time) {
	if err := e.store.UpdateAlertStatus(alertID, string(model.AlertResolved), now); err != nil {
		slog.Error("resolve alert failed", "alert", alertID, "err", err)
	}
}

// reload 读取最新告警规则,编译表达式,建反向索引;保留同 ID 规则的运行态(state/active),
// 被删除规则的 active 告警在锁外置 resolved。须在锁外调用(除最后替换段)。
func (e *Engine) reload() error {
	rules, err := e.store.ListAlertRules()
	if err != nil {
		return fmt.Errorf("list alert rules: %w", err)
	}
	newRules := make(map[string]*compiledRule, len(rules))
	newIndex := make(map[pointKey][]string)
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		cr, err := compileRule(r, e.rules[r.ID])
		if err != nil {
			slog.Warn("alert rule compile failed, skipped", "rule", r.ID, "expr", r.Expr, "err", err)
			continue
		}
		newRules[r.ID] = cr
		for _, rp := range r.ReferencedPoints {
			pk := pointKey{rp.DeviceID, rp.Point}
			cr.refs = append(cr.refs, pk)
			newIndex[pk] = append(newIndex[pk], r.ID)
		}
	}
	e.mu.Lock()
	old := e.rules
	e.rules = newRules
	e.index = newIndex
	e.mu.Unlock()

	for rid, oc := range old {
		if _, ok := newRules[rid]; !ok && oc.active {
			e.resolveAlert(oc.activeAlertID, time.Now())
		}
	}
	return nil
}

// compileRule 编译一条规则表达式并复用旧运行态(state/active)。prev 为同 ID 旧规则,nil 则全新。
func compileRule(r model.AlertRule, prev *compiledRule) (*compiledRule, error) {
	freshness := r.FreshnessSeconds
	if freshness <= 0 {
		freshness = defaultFreshnessSeconds
	}
	cr := &compiledRule{
		rule:      r,
		state:     make(map[pointKey]pointValue),
		freshness: time.Duration(freshness) * time.Second,
		cooldown:  time.Duration(r.CooldownSeconds) * time.Second,
	}
	if prev != nil {
		cr.state = prev.state
		cr.active = prev.active
		cr.activeAlertID = prev.activeAlertID
		cr.lastResolved = prev.lastResolved
	}
	cr.env = map[string]any{
		"point": func(deviceID, pointName string) any {
			pv, ok := cr.state[pointKey{deviceID, pointName}]
			if !ok {
				return nil
			}
			return pv.value
		},
	}
	program, err := expr.Compile(r.Expr, expr.Env(cr.env))
	if err != nil {
		return nil, fmt.Errorf("compile expr: %w", err)
	}
	cr.program = program
	return cr, nil
}

// newAlertID 生成告警 ID(crypto/rand 16 字节 hex)。
func newAlertID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
