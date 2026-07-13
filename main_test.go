package main

import "testing"

func TestBuildModelStatusSumsSlotTokens(t *testing.T) {
	cfg := timeWindowConfig{numSlots: 2, slotSeconds: 60}
	rows := map[int64]slotRow{
		0: {Total: 2, Success: 2, TotalTokens: 125},
		1: {Total: 1, Failure: 1, TotalTokens: 975},
	}

	status := buildModelStatus("test-model", "1h", cfg, 1_000, rows)

	if status.TotalTokens != 1_100 {
		t.Fatalf("TotalTokens = %d, want 1100", status.TotalTokens)
	}
	if status.SlotData[0].TotalTokens != 125 {
		t.Fatalf("slot 0 TotalTokens = %d, want 125", status.SlotData[0].TotalTokens)
	}
	if status.SlotData[1].TotalTokens != 975 {
		t.Fatalf("slot 1 TotalTokens = %d, want 975", status.SlotData[1].TotalTokens)
	}
}

func TestEmbeddingConsumeLogIsAvailable(t *testing.T) {
	row := slotRow{
		LogType:     logTypeConsume,
		Total:       1,
		TotalTokens: 128,
	}
	aggregated, err := mergeSlotRow(slotRow{}, row)
	if err != nil {
		t.Fatalf("mergeSlotRow() error = %v", err)
	}
	cfg := timeWindowConfig{numSlots: 1, slotSeconds: 60}

	status := buildModelStatus("text-embedding-test", "1h", cfg, 1_000, map[int64]slotRow{0: aggregated})

	if status.CurrentStatus != "green" {
		t.Fatalf("CurrentStatus = %q, want green", status.CurrentStatus)
	}
	if status.SuccessCount != 1 {
		t.Fatalf("SuccessCount = %d, want 1", status.SuccessCount)
	}
	if status.EmptyCount != 0 {
		t.Fatalf("EmptyCount = %d, want 0", status.EmptyCount)
	}
}

func TestErrorLogIsUnavailable(t *testing.T) {
	row := slotRow{LogType: logTypeError, Total: 1}
	aggregated, err := mergeSlotRow(slotRow{}, row)
	if err != nil {
		t.Fatalf("mergeSlotRow() error = %v", err)
	}
	cfg := timeWindowConfig{numSlots: 1, slotSeconds: 60}

	status := buildModelStatus("test-model", "1h", cfg, 1_000, map[int64]slotRow{0: aggregated})

	if status.CurrentStatus != "red" {
		t.Fatalf("CurrentStatus = %q, want red", status.CurrentStatus)
	}
	if status.FailureCount != 1 {
		t.Fatalf("FailureCount = %d, want 1", status.FailureCount)
	}
}
