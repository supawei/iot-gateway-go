package processing

import (
	"context"
	"sync"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/store"
)

// newTestEngine 构造引擎:内存 store + 收集出口,保存默认设备并同步加载规则。
func newTestEngine(t *testing.T, points ...model.Point) (*Engine, *collector) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveConnection(model.Connection{ID: "conn-1", Name: "conn-1", Driver: "modbus", Config: []byte(`{}`)}); err != nil {
		t.Fatalf("save connection: %v", err)
	}
	if len(points) > 0 {
		if err := st.SaveDevice(model.Device{
			ID: "d1", Name: "d1", ConnectionID: "conn-1",
			Params: []byte(`{}`), IntervalMs: 1000, Enabled: true, Points: points,
		}); err != nil {
			t.Fatalf("save device: %v", err)
		}
	}
	c := &collector{}
	e := NewEngine(st, c.add)
	if err := e.reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	return e, c
}

// startRun 启动引擎的变更监听 + 冲刷 goroutine,返回 cancel。
func startRun(e *Engine) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx)
	return cancel
}

// collector 收集引擎放行/派生上送的点;flushLoop 与测试 goroutine 并发读写,加锁。
type collector struct {
	mu  sync.Mutex
	pts []model.DataPoint
}

func (c *collector) add(dp model.DataPoint) {
	c.mu.Lock()
	c.pts = append(c.pts, dp)
	c.mu.Unlock()
}

// snapshot 返回已收集点的拷贝(并发安全读取)。
func (c *collector) snapshot() []model.DataPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]model.DataPoint, len(c.pts))
	copy(out, c.pts)
	return out
}

func point(name string, value interface{}, q model.Quality) model.DataPoint {
	return model.DataPoint{DeviceID: "d1", Point: name, Value: value, Timestamp: time.Now(), Quality: q}
}

func good(name string, value interface{}) model.DataPoint {
	return point(name, value, model.QualityGood)
}

// noRulePassthrough 无规则时直通。
func TestNoRulePassthrough(t *testing.T) {
	e, c := newTestEngine(t, model.Point{Name: "p1", Address: "a", DataType: model.DataTypeInt16})
	e.Process(good("p1", 1))
	e.Process(good("p2", 2)) // 未配置点位也直通
	if len(c.snapshot()) != 2 {
		t.Fatalf("want 2 passthrough, got %d: %+v", len(c.snapshot()), c.snapshot())
	}
	s := e.Stats()
	if s.PointsIn != 2 || s.PointsPass != 2 {
		t.Fatalf("stats mismatch: %+v", s)
	}
}

func deadbandPoint(delta float64) model.Point {
	return model.Point{Name: "p1", Address: "a", DataType: model.DataTypeFloat,
		Processing: &model.PointProcessing{Filters: []model.Filter{{Type: "deadband", Delta: delta}}}}
}

// TestDeadband 死区:首值放行,<delta 丢弃,越界放行,基线随放行值更新。
func TestDeadband(t *testing.T) {
	e, c := newTestEngine(t, deadbandPoint(1.0))
	e.Process(good("p1", 10.0)) // 首值放行,基线=10
	e.Process(good("p1", 10.5)) // 变化 0.5 < 1,丢弃
	e.Process(good("p1", 11.0)) // 变化 1.0 >= 1,放行,基线=11
	e.Process(good("p1", 11.2)) // 变化 0.2 < 1,丢弃
	e.Process(good("p1", 12.0)) // 变化 1.0 >= 1,放行
	if len(c.snapshot()) != 3 {
		t.Fatalf("want 3 passed, got %d: %+v", len(c.snapshot()), c.snapshot())
	}
}

// TestDeadbandDeltaZero delta=0:值变化才放行,相同值丢弃。
func TestDeadbandDeltaZero(t *testing.T) {
	e, c := newTestEngine(t, deadbandPoint(0))
	e.Process(good("p1", 10.0)) // 首值放行
	e.Process(good("p1", 10.0)) // 相同值丢弃
	e.Process(good("p1", 10.1)) // 变化放行
	if len(c.snapshot()) != 2 {
		t.Fatalf("want 2 passed, got %d: %+v", len(c.snapshot()), c.snapshot())
	}
}

// TestDeadbandNonNumeric 非数值点不受死区约束(直通)。
func TestDeadbandNonNumeric(t *testing.T) {
	e, c := newTestEngine(t, deadbandPoint(0.5))
	e.Process(point("p1", "hello", model.QualityGood))
	if len(c.snapshot()) != 1 {
		t.Fatalf("non-numeric should pass through, got %d", len(c.snapshot()))
	}
}

func thresholdPoint(op string, value float64) model.Point {
	return model.Point{Name: "p1", Address: "a", DataType: model.DataTypeFloat,
		Processing: &model.PointProcessing{Filters: []model.Filter{{Type: "threshold", Op: op, Value: value}}}}
}

// TestThreshold 阈值各操作符判定。
func TestThreshold(t *testing.T) {
	cases := []struct {
		op   string
		val  float64
		in   float64
		pass bool
	}{
		{"gt", 10, 11, true}, {"gt", 10, 10, false},
		{"ge", 10, 10, true}, {"lt", 10, 9, true}, {"lt", 10, 10, false},
		{"le", 10, 10, true}, {"eq", 10, 10, true}, {"eq", 10, 11, false},
		{"ne", 10, 11, true}, {"ne", 10, 10, false},
	}
	for _, tc := range cases {
		e, c := newTestEngine(t, thresholdPoint(tc.op, tc.val))
		e.Process(good("p1", tc.in))
		got := len(c.snapshot()) == 1
		if got != tc.pass {
			t.Fatalf("op=%s value=%v in=%v: pass=%v, want %v", tc.op, tc.val, tc.in, got, tc.pass)
		}
	}
}

// TestThresholdBetweenOutside between/outside 含端点判定。
func TestThresholdBetweenOutside(t *testing.T) {
	between := model.Point{Name: "p1", Address: "a", DataType: model.DataTypeFloat,
		Processing: &model.PointProcessing{Filters: []model.Filter{{Type: "threshold", Op: "between", Min: 0, Max: 10}}}}
	e, c := newTestEngine(t, between)
	e.Process(good("p1", -1.0)) // 外
	e.Process(good("p1", 0.0))  // 含下界
	e.Process(good("p1", 5.0))  // 内
	e.Process(good("p1", 10.0)) // 含上界
	e.Process(good("p1", 11.0)) // 外
	if len(c.snapshot()) != 3 {
		t.Fatalf("between want 3, got %d", len(c.snapshot()))
	}

	outside := model.Point{Name: "p1", Address: "a", DataType: model.DataTypeFloat,
		Processing: &model.PointProcessing{Filters: []model.Filter{{Type: "threshold", Op: "outside", Min: 0, Max: 10}}}}
	e2, c2 := newTestEngine(t, outside)
	e2.Process(good("p1", -1.0)) // 外,放行
	e2.Process(good("p1", 5.0))  // 内,丢弃
	e2.Process(good("p1", 11.0)) // 外,放行
	if len(c2.snapshot()) != 2 {
		t.Fatalf("outside want 2, got %d", len(c2.snapshot()))
	}
}

// TestQualityDropBad 质量过滤:dropBad 丢弃 bad/uncertain,good 放行。
func TestQualityDropBad(t *testing.T) {
	p := model.Point{Name: "p1", Address: "a", DataType: model.DataTypeInt16,
		Processing: &model.PointProcessing{Filters: []model.Filter{{Type: "quality", DropBad: true}}}}
	e, c := newTestEngine(t, p)
	e.Process(good("p1", 1))
	e.Process(point("p1", 2, model.QualityBad))
	e.Process(point("p1", 3, model.QualityUncertain))
	if len(c.snapshot()) != 1 {
		t.Fatalf("want 1 passed, got %d", len(c.snapshot()))
	}
}

func aggPoint(agg model.Aggregate) model.Point {
	return model.Point{Name: "p1", Address: "a", DataType: model.DataTypeFloat,
		Processing: &model.PointProcessing{Aggregate: &agg}}
}

// aggregateWindow 启动引擎(含冲刷 goroutine),灌入窗口时长内的点,等待冲刷后再断言。
func aggregateWindow(t *testing.T, agg model.Aggregate, values []float64) ([]model.DataPoint, *Engine, *collector) {
	t.Helper()
	e, c := newTestEngine(t, aggPoint(agg))
	cancel := startRun(e)
	t.Cleanup(cancel)
	for _, v := range values {
		e.Process(good("p1", v))
	}
	// 等待窗口自然到期 + 冲刷(flushLoop 节拍 500ms)。
	time.Sleep(1100 * time.Millisecond)
	return c.snapshot(), e, c
}

// TestAggregateValues 各类聚合窗口关闭产出正确派生点。
func TestAggregateValues(t *testing.T) {
	cases := []struct {
		aggType string
		want    float64
		check   string
	}{
		{"avg", 5, "avg"},     // (2+4+6+8)/4 = 5
		{"sum", 20, "sum"},    // 2+4+6+8 = 20
		{"min", 2, "min"},     // min(2,4,6,8)
		{"max", 8, "max"},     // max(2,4,6,8)
		{"count", 4, "count"}, // 4 个点
		{"last", 8, "last"},   // 最后一点
	}
	values := []float64{2, 4, 6, 8}
	for _, tc := range cases {
		agg := model.Aggregate{Type: tc.aggType, Window: "500ms"}
		points, _, _ := aggregateWindow(t, agg, values)
		if len(points) == 0 {
			t.Fatalf("%s: no aggregated point emitted", tc.aggType)
		}
		dp := points[0]
		if dp.Point != "p1."+tc.aggType {
			t.Fatalf("%s: derived name = %q, want p1.%s", tc.aggType, dp.Point, tc.aggType)
		}
		got, ok := numeric(dp.Value)
		if !ok || got != tc.want {
			t.Fatalf("%s: value = %v, want %v", tc.aggType, dp.Value, tc.want)
		}
		if dp.Quality != model.QualityGood {
			t.Fatalf("%s: quality = %v, want good", tc.aggType, dp.Quality)
		}
	}
}

// TestAggregateEmitName 自定义派生点名。
func TestAggregateEmitName(t *testing.T) {
	agg := model.Aggregate{Type: "avg", Window: "500ms", EmitName: "temp.avg"}
	points, _, _ := aggregateWindow(t, agg, []float64{1, 2})
	if len(points) == 0 || points[0].Point != "temp.avg" {
		t.Fatalf("emit name mismatch: %+v", points)
	}
}

// TestAggregateNonNumeric 非数值点不进聚合窗口,窗口仍能正常产出。
func TestAggregateNonNumeric(t *testing.T) {
	e, c := newTestEngine(t, aggPoint(model.Aggregate{Type: "count", Window: "300ms"}))
	cancel := startRun(e)
	defer cancel()
	e.Process(point("p1", "abc", model.QualityGood)) // 非数值,忽略
	e.Process(good("p1", 5.0))                       // 数值,计数 1
	time.Sleep(700 * time.Millisecond)
	if len(c.snapshot()) != 1 {
		t.Fatalf("want 1 aggregated point, got %d", len(c.snapshot()))
	}
	if v, _ := numeric(c.snapshot()[0].Value); v != 1 {
		t.Fatalf("count should be 1, got %v", c.snapshot()[0].Value)
	}
}

// TestAggregateWindowSwitch 窗口到期后新点到达:先冲旧窗再开新窗。
func TestAggregateWindowSwitch(t *testing.T) {
	e, c := newTestEngine(t, aggPoint(model.Aggregate{Type: "sum", Window: "200ms"}))
	cancel := startRun(e)
	defer cancel()
	e.Process(good("p1", 1.0))
	e.Process(good("p1", 2.0))
	time.Sleep(300 * time.Millisecond) // 第一窗口到期(200ms);flushLoop 首拍 500ms 尚未触发
	e.Process(good("p1", 10.0))        // 触发切窗:先冲旧窗(sum=3)再开新窗
	time.Sleep(900 * time.Millisecond)
	// 期望:旧窗 sum=3 派生点 + 新窗(仅 10)到期 sum=10 派生点,共 2 个。
	if len(c.snapshot()) != 2 {
		t.Fatalf("want 2 aggregated points, got %d: %+v", len(c.snapshot()), c.snapshot())
	}
	first, _ := numeric(c.snapshot()[0].Value)
	second, _ := numeric(c.snapshot()[1].Value)
	if first != 3 || second != 10 {
		t.Fatalf("sums = %v, %v; want 3, 10", first, second)
	}
}

// TestAggregateFlushWithoutData flushLoop 在窗口无新点时仍按时产出。
func TestAggregateFlushWithoutData(t *testing.T) {
	e, c := newTestEngine(t, aggPoint(model.Aggregate{Type: "last", Window: "300ms"}))
	cancel := startRun(e)
	defer cancel()
	e.Process(good("p1", 7.0)) // 窗口 300ms,flushLoop 首拍 500ms 时已到期
	time.Sleep(700 * time.Millisecond)
	if len(c.snapshot()) != 1 || c.snapshot()[0].Value != float64(7) {
		t.Fatalf("flush without data failed: %+v", c.snapshot())
	}
}

// TestFilterThenAggregate 过滤与聚合串联:不过过滤规则的点不进窗口。
func TestFilterThenAggregate(t *testing.T) {
	p := model.Point{Name: "p1", Address: "a", DataType: model.DataTypeFloat,
		Processing: &model.PointProcessing{
			Filters:   []model.Filter{{Type: "threshold", Op: "ge", Value: 100}},
			Aggregate: &model.Aggregate{Type: "count", Window: "300ms"},
		}}
	e, c := newTestEngine(t, p)
	cancel := startRun(e)
	defer cancel()
	e.Process(good("p1", 50.0))  // 不过阈值,丢弃
	e.Process(good("p1", 200.0)) // 过阈值,进窗口
	e.Process(good("p1", 300.0)) // 过阈值,进窗口
	time.Sleep(700 * time.Millisecond)
	if len(c.snapshot()) != 1 {
		t.Fatalf("want 1 aggregated point, got %d", len(c.snapshot()))
	}
	if v, _ := numeric(c.snapshot()[0].Value); v != 2 {
		t.Fatalf("count want 2, got %v", c.snapshot()[0].Value)
	}
}

// TestReloadRemovesRule 热重载删除规则后,该点位恢复直通、聚合器被清理。
func TestReloadRemovesRule(t *testing.T) {
	e, c := newTestEngine(t, aggPoint(model.Aggregate{Type: "sum", Window: "10s"}))
	if got := e.Stats().ActiveAggregators; got != 1 {
		t.Fatalf("active aggregators want 1, got %d", got)
	}
	// 移除处理配置(点位恢复直通),保存到同一 store 再 reload。
	if err := e.store.SaveDevice(model.Device{ID: "d1", Name: "d1", ConnectionID: "conn-1",
		Params: []byte(`{}`), IntervalMs: 1000, Enabled: true, Points: []model.Point{
			{Name: "p1", Address: "a", DataType: model.DataTypeFloat},
		}}); err != nil {
		t.Fatalf("save device: %v", err)
	}
	if err := e.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := e.Stats()
	if s.ActiveAggregators != 0 || s.ActiveRules != 0 {
		t.Fatalf("rules should be cleared: %+v", s)
	}
	e.Process(good("p1", 1.0)) // 恢复直通,应上送
	if len(c.snapshot()) != 1 {
		t.Fatalf("point should pass through after rule removal, got %d: %+v", len(c.snapshot()), c.snapshot())
	}
}
