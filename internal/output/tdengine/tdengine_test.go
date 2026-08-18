package tdengine

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
)

// TestChildTableDeterministic 验证子表名由 (deviceID, point) 稳定派生且合法。
func TestChildTableDeterministic(t *testing.T) {
	a := childTable("sensor-01", "temperature")
	b := childTable("sensor-01", "temperature")
	if a != b {
		t.Fatalf("child table name not deterministic: %q vs %q", a, b)
	}
	if a[:2] != "t_" || len(a) != 18 {
		t.Fatalf("child table name shape: %q", a)
	}
	if childTable("sensor-01", "humidity") == a {
		t.Fatal("different point should yield different child table")
	}
}

// TestValueLiteralTypeMapping 验证各类 Go 值映射到正确的强类型列与字面量。
func TestValueLiteralTypeMapping(t *testing.T) {
	cases := []struct {
		in      interface{}
		wantCol string
		wantLit string
	}{
		{true, "v_bool", "true"},
		{false, "v_bool", "false"},
		{int16(-12), "v_int", "-12"},
		{uint16(65535), "v_int", "65535"},
		{int32(123456), "v_int", "123456"},
		{int64(-9223372036854775808), "v_int", "-9223372036854775808"},
		{float32(25.5), "v_double", "25.5"},
		{float64(3.14), "v_double", "3.14"},
		{"hello", "v_str", "'hello'"},
		{nil, "", ""},
	}
	for _, c := range cases {
		col, lit, ok := valueLiteral(c.in)
		if c.wantCol == "" {
			if ok {
				t.Fatalf("valueLiteral(%v) should fail, got %s %s", c.in, col, lit)
			}
			continue
		}
		if !ok || col != c.wantCol || lit != c.wantLit {
			t.Fatalf("valueLiteral(%v) = (%s,%s,%v), want (%s,%s,true)", c.in, col, lit, ok, c.wantCol, c.wantLit)
		}
	}
}

// TestEscapeString 验证 TDengine 字符串转义(反斜杠与单引号)。
func TestEscapeString(t *testing.T) {
	if got := escapeString(`a'b\c`); got != `a\'b\\c` {
		t.Fatalf("escapeString = %q", got)
	}
}

// TestRowTuple 验证一行 VALUES 元组:值列按类型四选一,其余 NULL;ts 用毫秒。
func TestRowTuple(t *testing.T) {
	ts := time.UnixMilli(1700000000000)
	dp := model.DataPoint{Point: "temperature", Value: 25.5, Timestamp: ts, Quality: model.QualityGood}
	got := rowTuple(dp)
	want := "(1700000000000,'good',25.5,NULL,NULL,NULL)"
	if got != want {
		t.Fatalf("rowTuple:\n got %s\nwant %s", got, want)
	}
}

// TestRowTupleNilValue 验证无值(bad)点的元组:四值列全 NULL,保留 quality。
func TestRowTupleNilValue(t *testing.T) {
	dp := model.DataPoint{Point: "temperature", Value: nil, Timestamp: time.UnixMilli(1700000000000), Quality: model.QualityBad}
	got := rowTuple(dp)
	want := "(1700000000000,'bad',NULL,NULL,NULL,NULL)"
	if got != want {
		t.Fatalf("rowTuple nil:\n got %s\nwant %s", got, want)
	}
}

// TestRowTupleStringEscape 验证字符串值转义进元组。
func TestRowTupleStringEscape(t *testing.T) {
	dp := model.DataPoint{Point: "label", Value: "it's", Timestamp: time.UnixMilli(1700000000000), Quality: model.QualityGood}
	got := rowTuple(dp)
	if !strings.Contains(got, `'it\'s'`) {
		t.Fatalf("rowTuple string not escaped: %s", got)
	}
}

// TestBuildInsertSQL 验证多行 INSERT 语句结构:列头 + 空格分隔的多个 VALUES 元组。
func TestBuildInsertSQL(t *testing.T) {
	ts := time.UnixMilli(1700000000000)
	points := []model.DataPoint{
		{Point: "temperature", Value: 25.5, Timestamp: ts, Quality: model.QualityGood},
		{Point: "temperature", Value: 26.0, Timestamp: ts.Add(time.Second), Quality: model.QualityGood},
	}
	sql := buildInsertSQL("iot_gateway", "t_0123", points)
	want := "INSERT INTO `iot_gateway`.`t_0123` (`ts`,`quality`,`v_double`,`v_int`,`v_bool`,`v_str`) VALUES " +
		"(1700000000000,'good',25.5,NULL,NULL,NULL) (1700000001000,'good',26,NULL,NULL,NULL)"
	if sql != want {
		t.Fatalf("buildInsertSQL:\n got %s\nwant %s", sql, want)
	}
}

// TestCreateSQL 验证建库/建表 SQL 结构。
func TestCreateSQL(t *testing.T) {
	if got := createDatabaseSQL("iot_gateway"); got != "CREATE DATABASE IF NOT EXISTS `iot_gateway`" {
		t.Fatalf("createDatabaseSQL = %s", got)
	}
	stable := createStableSQL("iot_gateway", "data_points")
	for _, frag := range []string{"CREATE STABLE IF NOT EXISTS", "TAGS (", "device_id", "point", "v_double", "v_int", "v_bool", "v_str", "NCHAR(4096)"} {
		if !strings.Contains(stable, frag) {
			t.Fatalf("createStableSQL missing %q:\n%s", frag, stable)
		}
	}
	child := createChildSQL("iot_gateway", "data_points", "t_0123", "sensor-01", "temperature")
	want := "CREATE TABLE IF NOT EXISTS `iot_gateway`.`t_0123` USING `iot_gateway`.`data_points` TAGS ('sensor-01','temperature')"
	if child != want {
		t.Fatalf("createChildSQL:\n got %s\nwant %s", child, want)
	}
}

// mockTDengine 是 taosAdapter 的内存桩:记录收到的 SQL 与鉴权头,返回 code 0。
type mockTDengine struct {
	mu   chan struct{}
	sqls []string
	auth []string
}

func newMockTDengine() *mockTDengine {
	return &mockTDengine{mu: make(chan struct{}, 1)}
}

func (m *mockTDengine) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.sqls = append(m.sqls, string(body))
		m.auth = append(m.auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"code": 0, "rows": 0})
	}
}

// TestNewCreatesSchemaAndAuth 验证 New 时依次建库、建表,且携带 Basic 鉴权。
func TestNewCreatesSchemaAndAuth(t *testing.T) {
	m := newMockTDengine()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	out, err := New(Config{URL: srv.URL, Username: "root", Password: "taosdata"}, "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer out.Close()

	if len(m.sqls) != 2 {
		t.Fatalf("want 2 schema SQL, got %d: %v", len(m.sqls), m.sqls)
	}
	if !strings.HasPrefix(m.sqls[0], "CREATE DATABASE") || !strings.HasPrefix(m.sqls[1], "CREATE STABLE") {
		t.Fatalf("unexpected schema order: %v", m.sqls)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("root:taosdata"))
	if m.auth[0] != wantAuth {
		t.Fatalf("auth header = %q want %q", m.auth[0], wantAuth)
	}
}

// TestFlushInsertsGrouped 验证 flush 按子表分组:先建子表、再批量 INSERT。
func TestFlushInsertsGrouped(t *testing.T) {
	m := newMockTDengine()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	out, err := New(Config{URL: srv.URL}, "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o := out.(*tdengineOutput)

	ts := time.UnixMilli(1700000000000)
	o.Publish(model.DataPoint{DeviceID: "sensor-01", Point: "temperature", Value: 25.5, Timestamp: ts, Quality: model.QualityGood})
	o.Publish(model.DataPoint{DeviceID: "sensor-01", Point: "humidity", Value: 60, Timestamp: ts, Quality: model.QualityGood})
	o.flush()

	// 前两条为建库建表,随后 2 组各 1 条建子表 + 1 条 INSERT。
	if len(m.sqls) != 6 {
		t.Fatalf("want 6 SQL total, got %d:\n%s", len(m.sqls), strings.Join(m.sqls, "\n"))
	}
	// 建子表语句
	if !strings.HasPrefix(m.sqls[2], "CREATE TABLE") || !strings.HasPrefix(m.sqls[4], "CREATE TABLE") {
		t.Fatalf("expect child table SQL at 2/4: %v", m.sqls)
	}
	// INSERT 语句
	if !strings.HasPrefix(m.sqls[3], "INSERT INTO") || !strings.HasPrefix(m.sqls[5], "INSERT INTO") {
		t.Fatalf("expect INSERT at 3/5: %v", m.sqls)
	}
	joined := strings.Join(m.sqls, "\n")
	if !strings.Contains(joined, "25.5") || !strings.Contains(joined, "60") {
		t.Fatalf("insert value missing:\n%s", joined)
	}
}

// TestNewRejectsError 验证 code != 0 时 New 返回错误(连通性校验)。
func TestNewRejectsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"code": 2304, "desc": "database not exist"})
	}))
	defer srv.Close()

	if _, err := New(Config{URL: srv.URL}, "", nil); err == nil {
		t.Fatal("New should return error when TDengine returns code != 0")
	}
}

// TestRuntimeStatusPending RuntimeStatus 应如实上报待写缓冲长度。
func TestRuntimeStatusPending(t *testing.T) {
	o := &tdengineOutput{}
	o.mu.Lock()
	o.pending = []model.DataPoint{{DeviceID: "d", Point: "p"}}
	o.mu.Unlock()
	st := o.RuntimeStatus()
	if st.Pending != 1 {
		t.Fatalf("pending = %d, want 1", st.Pending)
	}
	if st.Connected || st.ConnectionOpen {
		t.Fatal("tdengine has no connection state")
	}
}

// fakeBackfillSink 记录被保存到补传队列的点(断网补传测试用)。
type fakeBackfillSink struct {
	mu  sync.Mutex
	dps []model.DataPoint
}

func (f *fakeBackfillSink) Save(_ string, dps []model.DataPoint) error {
	f.mu.Lock()
	f.dps = append(f.dps, dps...)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackfillSink) saved() []model.DataPoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.DataPoint(nil), f.dps...)
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// TestFlushInsertFailSavesToBackfill INSERT 失败时,该组数据点应落库补传而非丢弃。
func TestFlushInsertFailSavesToBackfill(t *testing.T) {
	// CREATE 语句成功,INSERT 语句返回错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(string(body), "INSERT") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"code": -1, "desc": "boom"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"code": 0, "rows": 0})
	}))
	defer srv.Close()

	sink := &fakeBackfillSink{}
	out, err := New(Config{URL: srv.URL, FlushInterval: "50ms"}, "out-1", sink)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer out.Close()

	dp := model.DataPoint{DeviceID: "d1", Point: "p1", Value: 1.5, Timestamp: time.Now(), Quality: model.QualityGood}
	if err := out.Publish(dp); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitFor(t, func() bool { return len(sink.saved()) >= 1 }, 3*time.Second)
	got := sink.saved()
	if len(got) != 1 || got[0].DeviceID != "d1" || got[0].Point != "p1" {
		t.Fatalf("saved points = %+v, want [d1/p1]", got)
	}
}

// TestBackfillHealthy 验证 TDengine 的健康门控:失败后退避,失败后有成功上送即恢复。
func TestBackfillHealthy(t *testing.T) {
	o := &tdengineOutput{}
	if !o.BackfillHealthy() {
		t.Fatal("BackfillHealthy should be true when no error yet")
	}
	o.SendStats.Failure(errors.New("boom"))
	if o.BackfillHealthy() {
		t.Fatal("BackfillHealthy should be false right after a send failure")
	}
	o.SendStats.Success(time.Now())
	if !o.BackfillHealthy() {
		t.Fatal("BackfillHealthy should be true after recovery (success after error)")
	}
}
