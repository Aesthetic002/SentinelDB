package main

import (
	"fmt"

	"sentineldb/internal/predict"
	"sentineldb/internal/server"
)

// configureMonitor attaches the prediction layer when asked for. With neither a model nor
// a telemetry path the node is plain, purely reactive Raft.
func configureMonitor(cfg *server.Config, modelPath string, threshold float64, telemetry string) error {
	if modelPath == "" && telemetry == "" {
		return nil
	}
	var model *predict.Model
	if modelPath != "" {
		var err error
		model, err = predict.LoadModel(modelPath)
		if err != nil {
			return fmt.Errorf("loading the prediction model: %w", err)
		}
		if threshold != 0 {
			model.Threshold = threshold
		}
	}
	m, err := predict.NewMonitor(predict.MonitorConfig{ID: cfg.ID, Model: model, TelemetryPath: telemetry})
	if err != nil {
		return err
	}
	cfg.Monitor = m
	return nil
}
