package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"iot-gateway-go/internal/api"
	"iot-gateway-go/internal/config"
	"iot-gateway-go/internal/core"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/output/mqtt"
	"iot-gateway-go/internal/store"

	_ "iot-gateway-go/internal/driver/modbus" // 注册 modbus 驱动
	_ "iot-gateway-go/internal/driver/opcua"  // 注册 opcua 驱动
)

const (
	datapointBufferSize = 1024
	shutdownTimeout     = 5 * time.Second
)

func main() {
	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	st, err := store.Open(cfg.Storage.SqlitePath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	outputs, err := buildOutputs(cfg)
	if err != nil {
		log.Fatalf("build outputs: %v", err)
	}

	dataPoints := make(chan model.DataPoint, datapointBufferSize)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pipelineDone := make(chan struct{})
	go func() {
		core.RunPipeline(ctx, dataPoints, outputs)
		close(pipelineDone)
	}()

	schedulerDone := make(chan struct{})
	go func() {
		if err := core.NewScheduler(st, dataPoints, cfg.Scheduler.PoolSize).Run(ctx); err != nil {
			log.Printf("scheduler exited: %v", err)
		}
		close(schedulerDone)
	}()

	server := &http.Server{Addr: cfg.HTTP.Addr, Handler: api.New(st).Routes()}
	go func() {
		log.Printf("HTTP API listening on %s", cfg.HTTP.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	server.Shutdown(shutdownCtx)

	<-schedulerDone
	<-pipelineDone
	for _, out := range outputs {
		out.Close()
	}
}

func buildOutputs(cfg config.Config) ([]output.Output, error) {
	mqttOutput, err := mqtt.New(cfg.MQTT, cfg.Gateway.ID)
	if err != nil {
		return nil, err
	}
	return []output.Output{mqttOutput}, nil
}
