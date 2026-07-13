package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestTokenTotalsEndpointReturnsPersistedHistory(t *testing.T) {
	source := &stubTokenLogSource{snapshots: []tokenLogSnapshot{{
		ThroughID: 1,
		Deltas: []modelTokenDelta{{
			ModelName:        "test-model",
			PromptTokens:     80,
			CompletionTokens: 20,
			FirstSeenAt:      1_000,
			LastSeenAt:       1_100,
		}},
	}}}
	history, err := openTokenHistory(filepath.Join(t.TempDir(), "history.db"), source)
	if err != nil {
		t.Fatalf("open token history: %v", err)
	}
	closeTokenHistoryAfterTest(t, history)
	if err := history.Collect(context.Background()); err != nil {
		t.Fatalf("collect token history: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	application := &app{
		cfg:     config{MaxModels: 100, StatusTimeout: time.Second},
		history: history,
	}
	application.registerRoutes(router)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/token-totals",
		bytes.NewBufferString(`{"models":["test-model"]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var payload struct {
		Data []modelTokenTotal `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0].RetainedTokens != 100 {
		t.Fatalf("data = %+v, want retained token total 100", payload.Data)
	}
}
