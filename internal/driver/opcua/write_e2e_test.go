package opcua

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/server"
	"github.com/gopcua/opcua/ua"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
)

// ============================================================================
// 共享测试设施:进程内 gopcua OPC UA server。
// server.New 会导入约 7MB 的标准 nodeset(约 13s 一次性开销),故用包级单例
// 摊薄成本,各测试用例以独立 ConnectionID 访问,互不干扰(包内测试默认串行)。
// ============================================================================

type e2eEnv struct {
	endpoint string
	ns       *server.NodeNameSpace
	intNode  *server.Node // ns=1;i=101
	dblNode  *server.Node // ns=1;i=102
	boolNode *server.Node // ns=1;i=103
	strNode  *server.Node // ns=1;s=StrVar
}

var (
	e2eOnce sync.Once
	e2eInst *e2eEnv
	e2eErr  error
)

// getE2EEnv 返回包级共享测试服务器,首次调用时启动。
func getE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	e2eOnce.Do(func() {
		const port = 48491
		srv := server.New(server.EndPoint("127.0.0.1", port))
		ns := server.NewNodeNameSpace(srv, "urn:iot-gateway:e2e")
		// 共享服务器生命周期跟随测试进程,ctx 用 Background 不取消;
		// 取消会让 acceptAndRegister/monitorConnections 立即退出、端口随即关闭。
		if err := srv.Start(context.Background()); err != nil {
			e2eErr = err
			return
		}
		e2eInst = &e2eEnv{
			endpoint: "opc.tcp://127.0.0.1:" + strconv.Itoa(port),
			ns:       ns,
			intNode:  ns.AddNewVariableNode("IntVar", int32(0)),
			dblNode:  ns.AddNewVariableNode("DoubleVar", float64(0)),
			boolNode: ns.AddNewVariableNode("BoolVar", false),
			strNode:  ns.AddNewVariableStringNode("StrVar", "x"),
		}
		// 把变量节点挂到本命名空间的 Objects 下,使浏览测试可从 ns=1;i=85 取到子节点
		objects := ns.Objects()
		objects.AddRef(e2eInst.intNode, server.RefTypeIDOrganizes, true)
		objects.AddRef(e2eInst.dblNode, server.RefTypeIDOrganizes, true)
		objects.AddRef(e2eInst.boolNode, server.RefTypeIDOrganizes, true)
		objects.AddRef(e2eInst.strNode, server.RefTypeIDOrganizes, true)
		// 给变量节点设真实 DataType 属性,供浏览测试断言 DataType 带回映射
		e2eInst.intNode.SetAttribute(ua.AttributeIDDataType, server.DataValueFromValue(ua.NewNumericNodeID(0, id.Int32)))
		e2eInst.dblNode.SetAttribute(ua.AttributeIDDataType, server.DataValueFromValue(ua.NewNumericNodeID(0, id.Double)))
		e2eInst.boolNode.SetAttribute(ua.AttributeIDDataType, server.DataValueFromValue(ua.NewNumericNodeID(0, id.Boolean)))
		e2eInst.strNode.SetAttribute(ua.AttributeIDDataType, server.DataValueFromValue(ua.NewNumericNodeID(0, id.String)))
		time.Sleep(150 * time.Millisecond) // 等服务监听就绪
	})
	if e2eErr != nil {
		t.Fatalf("start e2e server: %v", e2eErr)
	}
	return e2eInst
}

// openE2EConn 打开一个指向共享服务器的 opcua 驱动连接(独立 ConnectionID)。
func openE2EConn(t *testing.T, ctx context.Context, env *e2eEnv, connectionID string) driver.Conn {
	t.Helper()
	drv, err := driver.Get("opcua")
	if err != nil {
		t.Fatalf("get driver: %v", err)
	}
	conn, err := drv.Open(ctx, driver.OpenRequest{
		DeviceID:     "e2e-" + connectionID,
		ConnectionID: connectionID,
		ConnConfig:   []byte(`{"endpoint":"` + env.endpoint + `","timeout":"3s"}`),
		DeviceParams: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("open conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// writePoints 便捷封装:用驱动 Writer 批量写。
func writePoints(t *testing.T, ctx context.Context, conn driver.Conn, items []model.WriteItem) {
	t.Helper()
	results, err := conn.(driver.Writer).Write(ctx, items)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, r := range results {
		if !r.Ok {
			t.Fatalf("write result not ok: %+v", r)
		}
	}
}

// ============================================================================

// TestWriteE2E 端到端验证写值:驱动 Write 后直接读 server 命名空间内节点值。
// 回归背景:Write 若漏设 DataValue.EncodingMask=DataValueValue,gopcua 编码不
// 序列化 Value,server 收到空写而不写入,此测试防该类回归。
func TestWriteE2E(t *testing.T) {
	env := getE2EEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn := openE2EConn(t, ctx, env, "write-conn")

	writePoints(t, ctx, conn, []model.WriteItem{
		{Point: model.Point{Name: "IntVar", Address: env.intNode.ID().String(), DataType: model.DataTypeInt32}, Value: 42.0},
		{Point: model.Point{Name: "DoubleVar", Address: env.dblNode.ID().String(), DataType: model.DataTypeDouble}, Value: 3.14},
		{Point: model.Point{Name: "BoolVar", Address: env.boolNode.ID().String(), DataType: model.DataTypeBool}, Value: true},
		{Point: model.Point{Name: "StrVar", Address: env.strNode.ID().String(), DataType: model.DataTypeString}, Value: "wrote-ok"},
	})

	check := func(node *server.Node, want interface{}) {
		dv := env.ns.Attribute(node.ID(), ua.AttributeIDValue)
		if dv.Status != ua.StatusOK {
			t.Fatalf("read %s status: %v", node.ID(), dv.Status)
		}
		if got := dv.Value.Value(); got != want {
			t.Fatalf("%s = %v (%T), want %v", node.ID(), got, got, want)
		}
	}
	check(env.intNode, int32(42))
	check(env.dblNode, 3.14)
	check(env.boolNode, true)
	check(env.strNode, "wrote-ok")
}

// TestReadE2E 端到端验证轮询读取:写入已知值后 Read 批量读回,含无效地址点位
// 保持 bad、不阻断同批的语义。
func TestReadE2E(t *testing.T) {
	env := getE2EEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn := openE2EConn(t, ctx, env, "read-conn")

	// 先写已知值,再读回核对
	writePoints(t, ctx, conn, []model.WriteItem{
		{Point: model.Point{Name: "IntVar", Address: env.intNode.ID().String(), DataType: model.DataTypeInt32}, Value: 7.0},
		{Point: model.Point{Name: "DoubleVar", Address: env.dblNode.ID().String(), DataType: model.DataTypeDouble}, Value: 2.5},
		{Point: model.Point{Name: "BoolVar", Address: env.boolNode.ID().String(), DataType: model.DataTypeBool}, Value: false},
		{Point: model.Point{Name: "StrVar", Address: env.strNode.ID().String(), DataType: model.DataTypeString}, Value: "read-me"},
	})

	points := []model.Point{
		{Name: "IntVar", Address: env.intNode.ID().String(), DataType: model.DataTypeInt32},
		{Name: "DoubleVar", Address: env.dblNode.ID().String(), DataType: model.DataTypeDouble},
		{Name: "BoolVar", Address: env.boolNode.ID().String(), DataType: model.DataTypeBool},
		{Name: "StrVar", Address: env.strNode.ID().String(), DataType: model.DataTypeString},
		{Name: "Missing", Address: "ns=1;i=99999", DataType: model.DataTypeInt32}, // 无效地址:应保持 bad
	}
	dps, err := conn.Read(ctx, points)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []interface{}{int64(7), 2.5, false, "read-me"}
	for i := 0; i < 4; i++ {
		if dps[i].Quality != model.QualityGood || dps[i].Value != want[i] {
			t.Fatalf("point %d = %+v, want good %v", i, dps[i], want[i])
		}
	}
	if dps[4].Quality != model.QualityBad {
		t.Fatalf("missing node should be bad, got %+v", dps[4])
	}
}

// TestProbeE2E 端到端验证探测:可达 server 返回 nil;无可解析点位报错;
// 对未监听端口 Open 失败(Probe 前置的 Open 即判不可达)。
func TestProbeE2E(t *testing.T) {
	env := getE2EEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn := openE2EConn(t, ctx, env, "probe-conn")
	prober := conn.(driver.Prober)

	// 可达:能收到响应即判可达,即使混入不存在的节点
	points := []model.Point{
		{Name: "IntVar", Address: env.intNode.ID().String(), DataType: model.DataTypeInt32},
		{Name: "Missing", Address: "ns=1;i=99999", DataType: model.DataTypeInt32},
	}
	if err := prober.Probe(ctx, points); err != nil {
		t.Fatalf("probe reachable: %v", err)
	}

	// 无可解析点位 -> 报错
	if err := prober.Probe(ctx, []model.Point{{Name: "Bad", Address: "ns=abc;i=1", DataType: model.DataTypeInt32}}); err == nil {
		t.Fatal("probe with no parseable points should fail")
	}

	// 不可达:连接未监听端口应失败
	drv, err := driver.Get("opcua")
	if err != nil {
		t.Fatalf("get driver: %v", err)
	}
	if _, err := drv.Open(ctx, driver.OpenRequest{
		DeviceID:     "e2e-dead",
		ConnectionID: "dead-conn",
		ConnConfig:   []byte(`{"endpoint":"opc.tcp://127.0.0.1:1","timeout":"2s"}`),
		DeviceParams: []byte(`{}`),
	}); err == nil {
		t.Fatal("open to unbound endpoint should fail")
	}
}

// TestSubscribeE2E 端到端验证订阅推送:注册监控项后,从另一连接写新值,
// 订阅应经 onData 回调推送新值(初始值通知会被忽略,只认目标值)。
func TestSubscribeE2E(t *testing.T) {
	env := getE2EEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	drv, err := driver.Get("opcua")
	if err != nil {
		t.Fatalf("get driver: %v", err)
	}
	conn, err := drv.Open(ctx, driver.OpenRequest{
		DeviceID:     "e2e-sub",
		ConnectionID: "sub-conn",
		ConnConfig:   []byte(`{"endpoint":"` + env.endpoint + `","timeout":"3s","mode":"subscribe","publishInterval":"500ms"}`),
		DeviceParams: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("open subscribe conn: %v", err)
	}
	defer conn.Close()

	sub, ok := conn.(driver.Subscriber)
	if !ok {
		t.Fatal("subscribe conn should implement Subscriber")
	}
	point := model.Point{Name: "IntVar", Address: env.intNode.ID().String(), DataType: model.DataTypeInt32}
	notif := make(chan model.DataPoint, 16)
	if err := sub.Subscribe(ctx, []model.Point{point}, func(dp model.DataPoint) { notif <- dp }); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// 从另一独立连接写新值,触发服务器 DataChange 通知
	writer := openE2EConn(t, ctx, env, "sub-writer")
	writePoints(t, ctx, writer, []model.WriteItem{{Point: point, Value: 99.0}})

	// 等待推送的目标值;忽略服务器创建监控项时可能下发的初始值
	deadline := time.After(8 * time.Second)
	for {
		select {
		case dp := <-notif:
			if dp.Quality == model.QualityGood && dp.Value == int64(99) {
				t.Logf("received subscription notification: %+v", dp)
				return
			}
			t.Logf("ignoring notification %+v (initial value)", dp)
		case <-deadline:
			t.Fatal("no subscription notification with target value received")
		}
	}
}

// TestBrowseE2E 端到端验证节点浏览:从本命名空间 Objects(ns=1;i=85)浏览取到
// 已挂接的变量子节点,核对 NodeID/展示名/类型;空 parent(根)浏览不报错。
func TestBrowseE2E(t *testing.T) {
	env := getE2EEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	drv, err := driver.Get("opcua")
	if err != nil {
		t.Fatalf("get driver: %v", err)
	}
	browser, ok := drv.(driver.Browser)
	if !ok {
		t.Fatal("opcua driver should implement Browser")
	}
	cfg := []byte(`{"endpoint":"` + env.endpoint + `","timeout":"3s"}`)

	// 根浏览不报错
	if _, err := browser.Browse(ctx, "browse-conn", cfg, ""); err != nil {
		t.Fatalf("browse root: %v", err)
	}

	// 从本命名空间 Objects 浏览,应取到 4 个变量子节点
	infos, err := browser.Browse(ctx, "browse-conn", cfg, env.ns.Objects().ID().String())
	if err != nil {
		t.Fatalf("browse ns objects: %v", err)
	}
	byName := map[string]driver.NodeInfo{}
	for _, n := range infos {
		byName[n.DisplayName] = n
	}
	want := map[string]string{
		"IntVar":    env.intNode.ID().String(),
		"DoubleVar": env.dblNode.ID().String(),
		"BoolVar":   env.boolNode.ID().String(),
		"StrVar":    env.strNode.ID().String(),
	}
	wantType := map[string]string{
		"IntVar": "int32", "DoubleVar": "float64", "BoolVar": "bool", "StrVar": "string",
	}
	for name, nid := range want {
		n, ok := byName[name]
		if !ok {
			t.Fatalf("browse result missing %s (got %v)", name, infos)
		}
		if n.NodeID != nid {
			t.Fatalf("%s node id = %q, want %q", name, n.NodeID, nid)
		}
		// 回归:nodeClass 必须归一化为短名 "Variable"(前端据此允许选中回填),
		// 不能是 gopcua 的 "NodeClassVariable" 前缀形式。
		if n.NodeClass != "Variable" {
			t.Fatalf("%s nodeClass = %q, want Variable", name, n.NodeClass)
		}
		// 回归:Variable 节点须带回映射后的 dataType 短名(前端可回填 Point.dataType)
		if n.DataType != wantType[name] {
			t.Fatalf("%s dataType = %q, want %q", name, n.DataType, wantType[name])
		}
	}
}
