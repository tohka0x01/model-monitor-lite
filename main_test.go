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
