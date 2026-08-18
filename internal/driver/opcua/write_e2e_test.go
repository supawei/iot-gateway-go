package opcua

import (
	"context"
	"testing"
	"time"

	"github.com/gopcua/opcua/server"
	"github.com/gopcua/opcua/ua"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
)

// TestWriteE2E 用 gopcua 自带的进程内 OPC UA server 做端到端验证:驱动 Write 后
// 直接读 server 命名空间内节点值,确认真实写入生效。
// 回归背景:Write 构造 DataValue 时若漏设 EncodingMask=DataValueValue,gopcua 编码
// 不会序列化 Value,server 收到空写请求而不写入,此测试即防该类回归。
func TestWriteE2E(t *testing.T) {
	srv := server.New(server.EndPoint("127.0.0.1", 48491))
	ns := server.NewNodeNameSpace(srv, "urn:iot-gateway:e2e")
	nodeInt := ns.AddNewVariableNode("IntVar", int32(0))
	nodeDbl := ns.AddNewVariableNode("DoubleVar", float64(0))
	nodeBool := ns.AddNewVariableNode("BoolVar", false)
	nodeStr := ns.AddNewVariableStringNode("StrVar", "x")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	// 等服务监听就绪
	time.Sleep(150 * time.Millisecond)

	drv, err := driver.Get("opcua")
	if err != nil {
		t.Fatalf("get driver: %v", err)
	}
	conn, err := drv.Open(ctx, driver.OpenRequest{
		DeviceID:     "e2e",
		ConnectionID: "e2e-conn",
		ConnConfig:   []byte(`{"endpoint":"opc.tcp://127.0.0.1:48491","timeout":"3s"}`),
		DeviceParams: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("open conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	results, err := conn.(driver.Writer).Write(ctx, []model.WriteItem{
		{Point: model.Point{Name: "IntVar", Address: nodeInt.ID().String(), DataType: model.DataTypeInt32}, Value: 42.0},
		{Point: model.Point{Name: "DoubleVar", Address: nodeDbl.ID().String(), DataType: model.DataTypeDouble}, Value: 3.14},
		{Point: model.Point{Name: "BoolVar", Address: nodeBool.ID().String(), DataType: model.DataTypeBool}, Value: true},
		{Point: model.Point{Name: "StrVar", Address: nodeStr.ID().String(), DataType: model.DataTypeString}, Value: "wrote-ok"},
	})
	if err != nil {
		t.Fatalf("driver write: %v", err)
	}
	for _, r := range results {
		if !r.Ok {
			t.Fatalf("write result not ok: %+v", r)
		}
	}

	// 从 server 侧直接核对节点值已更新
	check := func(node *server.Node, want interface{}) {
		dv := ns.Attribute(node.ID(), ua.AttributeIDValue)
		if dv.Status != ua.StatusOK {
			t.Fatalf("read %s status: %v", node.ID(), dv.Status)
		}
		got := dv.Value.Value()
		if got != want {
			t.Fatalf("%s = %v (%T), want %v", node.ID(), got, got, want)
		}
	}
	check(nodeInt, int32(42))
	check(nodeDbl, 3.14)
	check(nodeBool, true)
	check(nodeStr, "wrote-ok")
}
