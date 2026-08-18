package output

import (
	"encoding/json"
	"errors"
	"testing"

	"iot-gateway-go/internal/model"
)

// 测试用输出类型:testset-ok 构建成功,testset-fail 构建失败。
func init() {
	Register(Descriptor{Type: "testset-ok"}, func(bc BuildContext, raw json.RawMessage) (Output, error) {
		return &stubOutput{}, nil
	})
	Register(Descriptor{Type: "testset-fail"}, func(bc BuildContext, raw json.RawMessage) (Output, error) {
		return nil, errStubBuildFail
	})
}

type stubOutput struct{}

func (s *stubOutput) Publish(model.DataPoint) error { return nil }
func (s *stubOutput) Close() error                  { return nil }

var errStubBuildFail = errors.New("test build failure")

func outCfg(id, typ string, enabled bool) model.Output {
	return model.Output{ID: id, Type: typ, Enabled: enabled, Config: json.RawMessage(`{}`)}
}

// TestBuildSetPartialFailure 单输出失败只跳过,其余生效;禁用项跳过。
func TestBuildSetPartialFailure(t *testing.T) {
	outs, err := BuildSet(BuildContext{}, []model.Output{
		outCfg("o1", "testset-ok", true),
		outCfg("o2", "testset-fail", true), // 失败→跳过
		outCfg("o3", "testset-ok", false),  // 禁用→跳过
	})
	if err != nil {
		t.Fatalf("err = %v, want nil (partial success)", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1 (only the failing one skipped)", len(outs))
	}
}

// TestBuildSetAllFail 有启用项但全部失败时返回错误(调用方据此保留旧输出)。
func TestBuildSetAllFail(t *testing.T) {
	outs, err := BuildSet(BuildContext{}, []model.Output{
		outCfg("o1", "testset-fail", true),
		outCfg("o2", "testset-fail", true),
	})
	if err == nil {
		t.Fatal("want error when all enabled outputs fail")
	}
	if len(outs) != 0 {
		t.Fatalf("got %d outputs, want 0", len(outs))
	}
}

// TestBuildSetNoEnabled 无启用项时返回空集且无错误(正常清空)。
func TestBuildSetNoEnabled(t *testing.T) {
	outs, err := BuildSet(BuildContext{}, []model.Output{
		outCfg("o1", "testset-ok", false),
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(outs) != 0 {
		t.Fatalf("got %d outputs, want 0", len(outs))
	}
}

func TestBuildSetEmpty(t *testing.T) {
	outs, err := BuildSet(BuildContext{}, nil)
	if err != nil || len(outs) != 0 {
		t.Fatalf("want empty,nil, got %d, %v", len(outs), err)
	}
}
