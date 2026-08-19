package observability

import (
	"context"
	"encoding/json"
	"net/http"

	"iot-gateway-go/internal/core"
)

// LivezHandler 进程存活探针(匿名):baseCtx 未取消返 200,优雅退出期返 503。
func LivezHandler(baseCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if baseCtx.Err() != nil {
			writeHealth(w, http.StatusServiceUnavailable, "down", false)
			return
		}
		writeHealth(w, http.StatusOK, "ok", true)
	}
}

// ReadyzHandler 就绪探针(匿名):scheduler 采集基础设施(cron)已启动即就绪。
// store 已开 + 配置加载完成在装配到挂路由阶段已必然为真,故就绪判定主要看 scheduler。
// 不含输出连接健康:上游全断不应让网关被判 not-ready 而被负载均衡误杀。
func ReadyzHandler(sched *core.Scheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ready := sched != nil && sched.IsReady()
		state := "ok"
		status := http.StatusOK
		if !ready {
			state = "not ready"
			status = http.StatusServiceUnavailable
		}
		writeHealth(w, status, state, ready)
	}
}

// writeHealth 输出小 JSON 体 {status,checks:{scheduler}} 便于排障。
func writeHealth(w http.ResponseWriter, status int, state string, schedulerReady bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"status": state,
		"checks": map[string]bool{"scheduler": schedulerReady},
	})
}
