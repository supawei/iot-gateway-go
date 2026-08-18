// Package processing 实现边缘计算处理层:对采集 DataPoint 做过滤(死区/阈值/质量)
// 与时间窗口聚合,产出放行点与派生点,再经 out 回调上送下游。规则配置挂在设备点位
// 上(Point.Processing),随 store.OnChange 增量热重载。设计见 docs/edge-computing-design.md。
package processing

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/store"
)

// key 定位一条规则:设备 + 点位。
type key struct {
	deviceID string
	point    string
}

// Stats 是处理层运行统计,经 REST 暴露用于确认处理层在生效。
type Stats struct {
	PointsIn          int64 `json:"pointsIn"`
	PointsPass        int64 `json:"pointsPass"`
	PointsFiltered    int64 `json:"pointsFiltered"`
	PointsAggregated  int64 `json:"pointsAggregated"`
	ActiveRules       int   `json:"activeRules"`
	ActiveAggregators int   `json:"activeAggregators"`
}

// Engine 是边缘处理引擎。
//
// 并发模型:
//   - Process 由 pipeline 单 goroutine 调用;
//   - reload 由 Run 的变更监听 goroutine 调用;
//   - flushLoop 由 Run 的后台冲刷 goroutine 调用。
//
// 三者共用一把互斥锁(e.mu)保护规则快照、deadband 基线、聚合器与统计。
type Engine struct {
	store *store.Store
	out   func(model.DataPoint)

	mu    sync.Mutex
	rules map[key]model.PointProcessing // 规则快照(热重载整体替换)
	last  map[key]float64               // deadband「上次放行值」基线
	aggs  map[key]*aggregator           // 聚合器(含窗口状态)
	stats Stats
}

// NewEngine 构造处理引擎。out 为放行/派生点的出口(通常接 output.Manager.Publish)。
func NewEngine(st *store.Store, out func(model.DataPoint)) *Engine {
	return &Engine{
		store: st,
		out:   out,
		rules: make(map[key]model.PointProcessing),
		last:  make(map[key]float64),
		aggs:  make(map[key]*aggregator),
	}
}

// Run 阻塞运行:监听配置变更并热重载规则,同时以固定节拍冲刷到期的聚合窗口。
// ctx 取消时返回。
func (e *Engine) Run(ctx context.Context) {
	if err := e.reload(); err != nil {
		slog.Error("processing initial reload failed", "err", err)
	}
	flushCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.flushLoop(flushCtx)

	changeCh, cancelChange := e.store.OnChange()
	defer cancelChange()
	for {
		select {
		case <-ctx.Done():
			return
		case <-changeCh:
			if err := e.reload(); err != nil {
				slog.Error("processing reload failed", "err", err)
			}
		}
	}
}

// Process 处理单个采集 DataPoint:查规则 → 无规则直通;有规则先过滤(不过即丢),
// 再决定进聚合窗口(不立即上送)或放行。out 回调在锁外调用。
func (e *Engine) Process(dp model.DataPoint) {
	k := key{deviceID: dp.DeviceID, point: dp.Point}

	e.mu.Lock()
	e.stats.PointsIn++
	pp, hasRule := e.rules[k]
	if !hasRule {
		e.stats.PointsPass++
		e.mu.Unlock()
		e.out(dp)
		return
	}
	if !e.passFiltersLocked(k, dp, pp) {
		e.stats.PointsFiltered++
		e.mu.Unlock()
		return
	}
	if pp.Aggregate != nil {
		derived := e.aggregateAddLocked(k, dp, *pp.Aggregate)
		e.stats.PointsAggregated += int64(len(derived))
		e.mu.Unlock()
		for _, d := range derived {
			e.out(d)
		}
		return
	}
	e.stats.PointsPass++
	e.mu.Unlock()
	e.out(dp)
}

// Stats 返回处理层运行统计快照。
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.stats
	s.ActiveRules = len(e.rules)
	s.ActiveAggregators = 0
	for _, r := range e.rules {
		if r.Aggregate != nil {
			s.ActiveAggregators++
		}
	}
	return s
}

// ---- 规则加载 ----

// reload 读取最新设备配置,提取点位处理规则并整体替换快照;
// 同时清理已删除规则的基线与聚合器,重置聚合配置变化的窗口。须在锁外调用。
func (e *Engine) reload() error {
	devices, err := e.store.ListDevices()
	if err != nil {
		return err
	}
	newRules := make(map[key]model.PointProcessing)
	for _, d := range devices {
		if !d.Enabled {
			continue
		}
		for _, p := range d.Points {
			if p.Processing == nil {
				continue
			}
			if !validProcessing(*p.Processing) {
				slog.Warn("processing rule skipped: invalid config", "device", d.ID, "point", p.Name)
				continue
			}
			newRules[key{d.ID, p.Name}] = *p.Processing
		}
	}

	e.mu.Lock()
	e.rules = newRules
	for k := range e.last {
		if _, ok := newRules[k]; !ok {
			delete(e.last, k)
		}
	}
	for k, a := range e.aggs {
		pp, ok := newRules[k]
		if !ok || pp.Aggregate == nil {
			delete(e.aggs, k)
			continue
		}
		if !sameAggregate(a.aggr, *pp.Aggregate) {
			// 聚合配置变化:丢弃窗口内未完成数据,重置。
			a.aggr = *pp.Aggregate
			a.window = windowDuration(*pp.Aggregate)
			a.reset()
		}
	}
	e.mu.Unlock()
	return nil
}

// validProcessing 校验处理配置:过滤类型/阈值操作符已知,聚合类型已知且窗口可解析。
func validProcessing(pp model.PointProcessing) bool {
	for _, f := range pp.Filters {
		switch f.Type {
		case "deadband":
			if f.Delta < 0 {
				return false
			}
		case "threshold":
			switch f.Op {
			case "gt", "ge", "lt", "le", "eq", "ne":
			case "between", "outside":
				if f.Max < f.Min {
					return false
				}
			default:
				return false
			}
		case "quality":
		default:
			return false
		}
	}
	if pp.Aggregate != nil {
		switch pp.Aggregate.Type {
		case "avg", "min", "max", "sum", "count", "last":
		default:
			return false
		}
		if windowDuration(*pp.Aggregate) <= 0 {
			return false
		}
	}
	return true
}

// windowDuration 解析聚合窗口;空或非法返回 0。
func windowDuration(a model.Aggregate) time.Duration {
	d, err := time.ParseDuration(a.Window)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

func sameAggregate(a, b model.Aggregate) bool {
	return a.Type == b.Type && a.Window == b.Window && a.EmitName == b.EmitName
}

// ---- 过滤 ----

// passFiltersLocked 依次应用所有过滤规则,全部通过才放行。须持有 e.mu。
func (e *Engine) passFiltersLocked(k key, dp model.DataPoint, pp model.PointProcessing) bool {
	for _, f := range pp.Filters {
		switch f.Type {
		case "deadband":
			if !e.passDeadbandLocked(k, dp, f) {
				return false
			}
		case "threshold":
			if !passThreshold(dp, f) {
				return false
			}
		case "quality":
			if f.DropBad && dp.Quality != model.QualityGood {
				return false
			}
		}
	}
	return true
}

// passDeadbandLocked 死区过滤:数值变化量 < delta 丢弃,只放行越过死区的变化。
// 首值无条件放行并记录基线;delta=0 表示值变化即放行。非数值点直通。须持有 e.mu。
func (e *Engine) passDeadbandLocked(k key, dp model.DataPoint, f model.Filter) bool {
	v, ok := numeric(dp.Value)
	if !ok {
		return true
	}
	baseline, seen := e.last[k]
	if !seen {
		e.last[k] = v
		return true
	}
	if v != baseline && math.Abs(v-baseline) >= f.Delta {
		e.last[k] = v
		return true
	}
	return false
}

// passThreshold 阈值过滤:命中才放行。非数值点直通。
func passThreshold(dp model.DataPoint, f model.Filter) bool {
	v, ok := numeric(dp.Value)
	if !ok {
		return true
	}
	switch f.Op {
	case "gt":
		return v > f.Value
	case "ge":
		return v >= f.Value
	case "lt":
		return v < f.Value
	case "le":
		return v <= f.Value
	case "eq":
		return v == f.Value
	case "ne":
		return v != f.Value
	case "between":
		return v >= f.Min && v <= f.Max
	case "outside":
		return v < f.Min || v > f.Max
	}
	return true
}

// ---- 聚合 ----

// aggregator 是一个点位的时间窗口聚合器(窗口内中间数据仅内存)。
type aggregator struct {
	aggr   model.Aggregate
	window time.Duration
	start  time.Time
	any    bool
	count  int
	sum    float64
	min    float64
	max    float64
	last   float64
}

func (a *aggregator) reset() {
	a.start = time.Time{}
	a.any = false
	a.count = 0
	a.sum = 0
	a.min = 0
	a.max = 0
	a.last = 0
}

// due 判断窗口是否到期(有数据且到达时长超过窗口)。
func (a *aggregator) due(now time.Time) bool {
	return a.any && now.Sub(a.start) >= a.window
}

// aggregateAddLocked 把过滤通过的数值点加入窗口;窗口到期先冲刷旧窗口再开新窗口。
// 返回本次需上送的派生点(窗口切换时的旧窗口产物)。须持有 e.mu。
func (e *Engine) aggregateAddLocked(k key, dp model.DataPoint, aggr model.Aggregate) []model.DataPoint {
	a, ok := e.aggs[k]
	if !ok {
		a = &aggregator{aggr: aggr, window: windowDuration(aggr)}
		e.aggs[k] = a
	}
	v, isNum := numeric(dp.Value)
	now := time.Now()
	var derived []model.DataPoint
	if isNum && a.due(now) {
		derived = append(derived, a.flush(k, now))
	}
	if isNum {
		if !a.any {
			a.start = now
			a.any = true
			a.min, a.max, a.last = v, v, v
		}
		a.count++
		a.sum += v
		if v < a.min {
			a.min = v
		}
		if v > a.max {
			a.max = v
		}
		a.last = v
	}
	return derived
}

// flush 产出当前窗口的派生点并重置聚合器。
func (a *aggregator) flush(k key, now time.Time) model.DataPoint {
	dp := model.DataPoint{
		DeviceID:  k.deviceID,
		Point:     emitName(k.point, a.aggr),
		Value:     a.value(),
		Timestamp: now,
		Quality:   model.QualityGood,
	}
	a.reset()
	return dp
}

func emitName(point string, a model.Aggregate) string {
	if a.EmitName != "" {
		return a.EmitName
	}
	return point + "." + a.Type
}

func (a *aggregator) value() interface{} {
	switch a.aggr.Type {
	case "avg":
		if a.count == 0 {
			return float64(0)
		}
		return a.sum / float64(a.count)
	case "min":
		return a.min
	case "max":
		return a.max
	case "sum":
		return a.sum
	case "count":
		return a.count
	case "last":
		return a.last
	}
	return nil
}

// flushLoop 按节拍冲刷到期的聚合窗口,保证「窗口无新点也能按时产出」。
func (e *Engine) flushLoop(ctx context.Context) {
	const tick = 500 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			var derived []model.DataPoint
			e.mu.Lock()
			for k, a := range e.aggs {
				if a.due(now) {
					derived = append(derived, a.flush(k, now))
				}
			}
			e.stats.PointsAggregated += int64(len(derived))
			e.mu.Unlock()
			for _, d := range derived {
				e.out(d)
			}
		}
	}
}

// ---- 数值判定 ----

// numeric 把常见数值类型统一为 float64;bool/string 等非数值返回 false。
func numeric(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case bool:
		return 0, false
	case string:
		return 0, false
	}
	return 0, false
}
