package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type config struct {
	SQLDSN          string
	LogSQLDSN       string
	ServerHost      string
	ServerPort      string
	BasePath        string
	PublicTitle     string
	DefaultModels   []string
	DefaultWindow   string
	RefreshSeconds  int
	MaxModels       int
	StatusTimeout   time.Duration
	HistoryDataPath string
	HistoryRefresh  time.Duration
	HistoryTimeout  time.Duration
	MockData        bool
}

type app struct {
	cfg             config
	db              *sqlx.DB
	logDB           *sqlx.DB
	history         *tokenHistory
	isPG            bool
	collectorCancel context.CancelFunc
	collectorDone   chan struct{}
}

type timeWindowConfig struct {
	totalSeconds int64
	numSlots     int
	slotSeconds  int64
}

var timeWindows = map[string]timeWindowConfig{
	"1h":  {totalSeconds: 3600, numSlots: 60, slotSeconds: 60},
	"6h":  {totalSeconds: 21600, numSlots: 24, slotSeconds: 900},
	"12h": {totalSeconds: 43200, numSlots: 24, slotSeconds: 1800},
	"24h": {totalSeconds: 86400, numSlots: 24, slotSeconds: 3600},
}

func main() {
	cfg := loadConfig()
	if cfg.SQLDSN == "" && !cfg.MockData {
		log.Fatal("SQL_DSN is required unless MOCK_DATA=true")
	}

	app, err := newApp(cfg)
	if err != nil {
		log.Fatal(err)
	}
	app.startHistoryCollector()
	defer app.close()

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(cors.New(cors.Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{http.MethodGet, http.MethodPost, http.MethodOptions},
		AllowHeaders: []string{"Origin", "Content-Type", "Accept"},
		MaxAge:       12 * time.Hour,
	}))

	app.registerRoutes(r)

	addr := cfg.ServerHost + ":" + cfg.ServerPort
	log.Printf("model monitor lite listening on http://%s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatal(err)
	}
}

func loadConfig() config {
	return config{
		SQLDSN:          strings.TrimSpace(os.Getenv("SQL_DSN")),
		LogSQLDSN:       strings.TrimSpace(os.Getenv("LOG_SQL_DSN")),
		ServerHost:      envDefault("SERVER_HOST", "0.0.0.0"),
		ServerPort:      envDefault("SERVER_PORT", "1145"),
		BasePath:        cleanBasePath(os.Getenv("BASE_PATH")),
		PublicTitle:     envDefault("PUBLIC_TITLE", "模型状态监控"),
		DefaultModels:   splitCSV(os.Getenv("DEFAULT_MODELS")),
		DefaultWindow:   envDefault("DEFAULT_WINDOW", "24h"),
		RefreshSeconds:  envIntDefault("REFRESH_SECONDS", 60),
		MaxModels:       envPositiveIntDefault("MAX_MODELS", 100),
		StatusTimeout:   time.Duration(envPositiveIntDefault("STATUS_TIMEOUT_SECONDS", 15)) * time.Second,
		HistoryDataPath: envDefault("HISTORY_DATA_PATH", "./data/model-monitor.db"),
		HistoryRefresh:  time.Duration(envPositiveIntDefault("HISTORY_REFRESH_SECONDS", 60)) * time.Second,
		HistoryTimeout:  time.Duration(envPositiveIntDefault("HISTORY_TIMEOUT_SECONDS", 300)) * time.Second,
		MockData:        envBoolDefault("MOCK_DATA", false),
	}
}

func newApp(cfg config) (*app, error) {
	if cfg.MockData {
		return &app{cfg: cfg}, nil
	}

	db, isPG, err := openDB(cfg.SQLDSN)
	if err != nil {
		return nil, fmt.Errorf("open SQL_DSN: %w", err)
	}

	logDB := db
	if cfg.LogSQLDSN != "" && cfg.LogSQLDSN != cfg.SQLDSN {
		var logIsPG bool
		logDB, logIsPG, err = openDB(cfg.LogSQLDSN)
		if err != nil {
			if closeErr := db.Close(); closeErr != nil {
				log.Printf("close application database after log database failure: %v", closeErr)
			}
			return nil, fmt.Errorf("open LOG_SQL_DSN: %w", err)
		}
		isPG = logIsPG
	}

	history, err := openTokenHistory(cfg.HistoryDataPath, &sqlTokenLogSource{db: logDB, isPG: isPG})
	if err != nil {
		closeAppDatabases(db, logDB)
		return nil, fmt.Errorf("open token history: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.HistoryTimeout)
	err = history.Collect(ctx)
	cancel()
	if err != nil {
		if closeErr := history.Close(); closeErr != nil {
			log.Printf("close token history after initialization failure: %v", closeErr)
		}
		closeAppDatabases(db, logDB)
		return nil, fmt.Errorf("initialize token history: %w", err)
	}

	return &app{cfg: cfg, db: db, logDB: logDB, history: history, isPG: isPG}, nil
}

func openDB(rawDSN string) (*sqlx.DB, bool, error) {
	driver, isPG := driverForDSN(rawDSN)
	dsn := normalizeDSN(rawDSN)
	db, err := sqlx.Connect(driver, dsn)
	if err != nil {
		return nil, false, err
	}
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, isPG, nil
}

func closeAppDatabases(db, logDB *sqlx.DB) {
	if logDB != nil && logDB != db {
		if err := logDB.Close(); err != nil {
			log.Printf("close log database: %v", err)
		}
	}
	if db != nil {
		if err := db.Close(); err != nil {
			log.Printf("close application database: %v", err)
		}
	}
}

// startHistoryCollector must be called once before the app begins serving requests.
func (a *app) startHistoryCollector() {
	if a.history == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.collectorCancel = cancel
	a.collectorDone = make(chan struct{})
	go a.runHistoryCollector(ctx)
}

// runHistoryCollector owns the single background collection loop.
func (a *app) runHistoryCollector(ctx context.Context) {
	defer close(a.collectorDone)
	ticker := time.NewTicker(a.cfg.HistoryRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.collectHistory(ctx)
		}
	}
}

func (a *app) collectHistory(ctx context.Context) {
	collectCtx, cancel := context.WithTimeout(ctx, a.cfg.HistoryTimeout)
	defer cancel()
	if err := a.history.Collect(collectCtx); err != nil && ctx.Err() == nil {
		log.Printf("token history collection failed: %v", err)
	}
}

// close must be called after the HTTP server stops accepting new work.
func (a *app) close() {
	if a.collectorCancel != nil {
		a.collectorCancel()
		<-a.collectorDone
	}
	if a.history != nil {
		if err := a.history.Close(); err != nil {
			log.Printf("close token history: %v", err)
		}
	}
	closeAppDatabases(a.db, a.logDB)
}

func driverForDSN(dsn string) (string, bool) {
	lower := strings.ToLower(dsn)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") || strings.Contains(lower, "host=") {
		return "pgx", true
	}
	return "mysql", false
}

func normalizeDSN(dsn string) string {
	if strings.HasPrefix(strings.ToLower(dsn), "mysql://") {
		return strings.TrimPrefix(dsn, "mysql://")
	}
	return dsn
}

func (a *app) rebind(query string) string {
	if a.isPG {
		return sqlx.Rebind(sqlx.DOLLAR, query)
	}
	return query
}

func envDefault(key, fallback string) string {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return fallback
	}
	return val
}

func envBoolDefault(key string, fallback bool) bool {
	val := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch val {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envIntDefault(key string, fallback int) int {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return fallback
	}
	n, err := strconv.Atoi(val)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func envPositiveIntDefault(key string, fallback int) int {
	n := envIntDefault(key, fallback)
	if n <= 0 {
		return fallback
	}
	return n
}

func cleanBasePath(raw string) string {
	base := strings.TrimSpace(raw)
	if base == "" || base == "/" {
		return ""
	}
	return "/" + strings.Trim(base, "/")
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func roundRate(rate float64) float64 {
	return math.Round(rate*100) / 100
}

type modelOption struct {
	ModelName       string `json:"model_name" db:"model_name"`
	RequestCount24h int64  `json:"request_count_24h" db:"request_count_24h"`
}

type statusRequest struct {
	Models []string `json:"models"`
	Window string   `json:"window"`
}

type modelsRequest struct {
	Models []string `json:"models"`
}

type slotStatus struct {
	Slot          int     `json:"slot"`
	StartTime     int64   `json:"start_time"`
	EndTime       int64   `json:"end_time"`
	TotalRequests int64   `json:"total_requests"`
	TotalTokens   int64   `json:"total_tokens"`
	SuccessCount  int64   `json:"success_count"`
	FailureCount  int64   `json:"failure_count"`
	EmptyCount    int64   `json:"empty_count"`
	SuccessRate   float64 `json:"success_rate"`
	Status        string  `json:"status"`
}

type modelStatus struct {
	ModelName     string       `json:"model_name"`
	DisplayName   string       `json:"display_name"`
	TimeWindow    string       `json:"time_window"`
	TotalRequests int64        `json:"total_requests"`
	TotalTokens   int64        `json:"total_tokens"`
	SuccessCount  int64        `json:"success_count"`
	FailureCount  int64        `json:"failure_count"`
	EmptyCount    int64        `json:"empty_count"`
	SuccessRate   float64      `json:"success_rate"`
	CurrentStatus string       `json:"current_status"`
	SlotData      []slotStatus `json:"slot_data"`
}

const (
	logTypeConsume int64 = 2
	logTypeError   int64 = 5
)

type slotRow struct {
	ModelName   string `db:"model_name"`
	SlotIdx     int64  `db:"slot_idx"`
	LogType     int64  `db:"log_type"`
	Total       int64  `db:"total"`
	TotalTokens int64  `db:"total_tokens"`
	Success     int64  `db:"success"`
	Failure     int64  `db:"failure"`
	Empty       int64  `db:"empty"`
}

func (a *app) registerRoutes(r *gin.Engine) {
	mount := r.Group(a.cfg.BasePath)
	mount.Static("/static", "./static")
	mount.GET("/embed", func(c *gin.Context) {
		c.File("./static/embed.html")
	})
	mount.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, a.cfg.BasePath+"/embed")
	})

	api := mount.Group("/api")
	{
		api.GET("/health", a.handleHealth)
		api.GET("/config", a.handleConfig)
		api.GET("/models", a.handleModels)
		api.POST("/status", a.handleStatus)
		api.POST("/token-totals", a.handleTokenTotals)
	}
}

func (a *app) handleHealth(c *gin.Context) {
	if a.cfg.MockData {
		c.JSON(http.StatusOK, gin.H{"success": true, "mock": true})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if err := a.logDB.PingContext(ctx); err != nil {
		writeAPIError(c, http.StatusServiceUnavailable, "health check failed", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (a *app) handleConfig(c *gin.Context) {
	window := a.cfg.DefaultWindow
	if _, ok := timeWindows[window]; !ok {
		window = "24h"
	}
	c.JSON(http.StatusOK, gin.H{
		"success":         true,
		"title":           a.cfg.PublicTitle,
		"default_models":  a.cfg.DefaultModels,
		"default_window":  window,
		"time_windows":    []string{"1h", "6h", "12h", "24h"},
		"refresh_seconds": a.cfg.RefreshSeconds,
	})
}

func (a *app) handleModels(c *gin.Context) {
	if a.cfg.MockData {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": mockModelOptions()})
		return
	}

	models, err := a.availableModels(c.Request.Context())
	if err != nil {
		writeAPIError(c, http.StatusInternalServerError, "failed to load models", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": models})
}

func (a *app) handleTokenTotals(c *gin.Context) {
	var req modelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "invalid JSON body"})
		return
	}
	models := a.limitModels(uniqueModels(req.Models))
	if len(models) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "at least one model is required"})
		return
	}
	if a.cfg.MockData {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": mockTokenTotals(models), "mock": true})
		return
	}
	if a.history == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "error": "token history is unavailable"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), a.cfg.StatusTimeout)
	defer cancel()
	totals, err := a.history.Totals(ctx, models)
	if err != nil {
		writeAPIError(c, http.StatusInternalServerError, "failed to load token history", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": totals})
}

func (a *app) handleStatus(c *gin.Context) {
	var req statusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "invalid JSON body"})
		return
	}
	window := req.Window
	if _, ok := timeWindows[window]; !ok {
		window = a.cfg.DefaultWindow
	}
	if _, ok := timeWindows[window]; !ok {
		window = "24h"
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), a.cfg.StatusTimeout)
	defer cancel()

	if a.cfg.MockData {
		models := a.limitModels(uniqueModels(req.Models))
		if len(models) == 0 {
			models = a.limitModels(a.cfg.DefaultModels)
		}
		if len(models) == 0 {
			for _, item := range mockModelOptions() {
				models = append(models, item.ModelName)
			}
			models = a.limitModels(models)
		}
		statuses := mockStatuses(models, window)
		sortModelStatuses(statuses)
		c.JSON(http.StatusOK, gin.H{
			"success":     true,
			"data":        statuses,
			"time_window": window,
			"mock":        true,
		})
		return
	}

	models := a.limitModels(uniqueModels(req.Models))
	if len(models) == 0 {
		models = a.limitModels(a.cfg.DefaultModels)
	}
	if len(models) == 0 {
		available, err := a.availableModels(ctx)
		if err != nil {
			writeAPIError(c, http.StatusInternalServerError, "failed to load models", err)
			return
		}
		for _, item := range available {
			models = append(models, item.ModelName)
		}
		models = a.limitModels(models)
	}

	statuses, err := a.modelStatuses(ctx, models, window)
	if err != nil {
		writeAPIError(c, http.StatusInternalServerError, "failed to load model status", err)
		return
	}
	sortModelStatuses(statuses)

	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"data":        statuses,
		"time_window": window,
	})
}

func writeAPIError(c *gin.Context, status int, publicMessage string, err error) {
	if err != nil {
		log.Printf("api error status=%d path=%s method=%s message=%q detail=%v", status, c.FullPath(), c.Request.Method, publicMessage, err)
	}
	c.JSON(status, gin.H{"success": false, "error": publicMessage})
}

func (a *app) availableModels(ctx context.Context) ([]modelOption, error) {
	startTime := time.Now().Unix() - 86400
	query := a.rebind(`
		SELECT model_name, COUNT(*) as request_count_24h
		FROM logs
		WHERE type IN (2, 5) AND model_name IS NOT NULL AND model_name != '' AND created_at >= ?
		GROUP BY model_name
		ORDER BY request_count_24h DESC
		LIMIT ?`)

	rows := []modelOption{}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.logDB.SelectContext(ctx, &rows, query, startTime, a.cfg.MaxModels); err != nil {
		return nil, err
	}
	return rows, nil
}

func (a *app) modelStatuses(ctx context.Context, models []string, window string) ([]modelStatus, error) {
	if len(models) == 0 {
		return []modelStatus{}, nil
	}
	cfg := timeWindows[window]
	now := time.Now().Unix()
	startTime := now - cfg.totalSeconds

	query := fmt.Sprintf(`
		SELECT model_name,
			FLOOR((created_at - %d) / %d) as slot_idx,
			type as log_type,
			COUNT(*) as total,
			COALESCE(SUM(prompt_tokens), 0) + COALESCE(SUM(completion_tokens), 0) as total_tokens
		FROM logs
		WHERE model_name IN (?)
			AND created_at >= ? AND created_at < ?
			AND type IN (2, 5)
		GROUP BY model_name, FLOOR((created_at - %d) / %d), type`, startTime, cfg.slotSeconds, startTime, cfg.slotSeconds)
	query, args, err := sqlx.In(query, models, startTime, now)
	if err != nil {
		return nil, err
	}
	query = a.rebind(query)

	rows := []slotRow{}
	if err := a.logDB.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, err
	}

	byModel := make(map[string]map[int64]slotRow, len(models))
	for _, row := range rows {
		if row.SlotIdx < 0 || row.SlotIdx >= int64(cfg.numSlots) {
			continue
		}
		if _, ok := byModel[row.ModelName]; !ok {
			byModel[row.ModelName] = make(map[int64]slotRow)
		}
		current := byModel[row.ModelName][row.SlotIdx]
		merged, mergeErr := mergeSlotRow(current, row)
		if mergeErr != nil {
			return nil, mergeErr
		}
		byModel[row.ModelName][row.SlotIdx] = merged
	}

	statuses := make([]modelStatus, 0, len(models))
	for _, modelName := range models {
		statuses = append(statuses, buildModelStatus(modelName, window, cfg, startTime, byModel[modelName]))
	}
	return statuses, nil
}

// mergeSlotRow is safe for concurrent calls because it only mutates value copies.
func mergeSlotRow(current, row slotRow) (slotRow, error) {
	current.Total += row.Total
	current.TotalTokens += row.TotalTokens

	switch row.LogType {
	case logTypeConsume:
		current.Success += row.Total
	case logTypeError:
		current.Failure += row.Total
	default:
		return slotRow{}, fmt.Errorf("unsupported log type %d", row.LogType)
	}
	return current, nil
}

func buildModelStatus(modelName, window string, cfg timeWindowConfig, startTime int64, bySlot map[int64]slotRow) modelStatus {
	slots := make([]slotStatus, 0, cfg.numSlots)
	var totalReqs, totalTokens, totalSuccess, totalFailure, totalEmpty int64
	for i := 0; i < cfg.numSlots; i++ {
		row := bySlot[int64(i)]
		slotStart := startTime + int64(i)*cfg.slotSeconds
		rate := float64(100)
		if row.Total > 0 {
			rate = float64(row.Success) / float64(row.Total) * 100
		}
		slots = append(slots, slotStatus{
			Slot:          i,
			StartTime:     slotStart,
			EndTime:       slotStart + cfg.slotSeconds,
			TotalRequests: row.Total,
			TotalTokens:   row.TotalTokens,
			SuccessCount:  row.Success,
			FailureCount:  row.Failure,
			EmptyCount:    row.Empty,
			SuccessRate:   roundRate(rate),
			Status:        statusColor(rate, row.Total),
		})
		totalReqs += row.Total
		totalTokens += row.TotalTokens
		totalSuccess += row.Success
		totalFailure += row.Failure
		totalEmpty += row.Empty
	}

	overall := float64(100)
	if totalReqs > 0 {
		overall = float64(totalSuccess) / float64(totalReqs) * 100
	}

	return modelStatus{
		ModelName:     modelName,
		DisplayName:   modelName,
		TimeWindow:    window,
		TotalRequests: totalReqs,
		TotalTokens:   totalTokens,
		SuccessCount:  totalSuccess,
		FailureCount:  totalFailure,
		EmptyCount:    totalEmpty,
		SuccessRate:   roundRate(overall),
		CurrentStatus: statusColor(overall, totalReqs),
		SlotData:      slots,
	}
}

func statusColor(successRate float64, totalRequests int64) string {
	if totalRequests == 0 || successRate >= 95 {
		return "green"
	}
	if successRate >= 80 {
		return "yellow"
	}
	return "red"
}

func sortModelStatuses(statuses []modelStatus) {
	sort.SliceStable(statuses, func(i, j int) bool {
		left := statuses[i]
		right := statuses[j]
		leftRank := availabilityRank(left)
		rightRank := availabilityRank(right)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.SuccessRate != right.SuccessRate {
			return left.SuccessRate > right.SuccessRate
		}
		if left.FailureCount != right.FailureCount {
			return left.FailureCount < right.FailureCount
		}
		if left.TotalRequests != right.TotalRequests {
			return left.TotalRequests > right.TotalRequests
		}
		return strings.ToLower(left.ModelName) < strings.ToLower(right.ModelName)
	})
}

func availabilityRank(status modelStatus) int {
	if status.TotalRequests == 0 {
		return 3
	}
	switch status.CurrentStatus {
	case "green", "good":
		return 0
	case "yellow", "warn":
		return 1
	case "red", "bad":
		return 2
	default:
		return 2
	}
}

func uniqueModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := map[string]struct{}{}
	for _, raw := range models {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func (a *app) limitModels(models []string) []string {
	if a.cfg.MaxModels <= 0 || len(models) <= a.cfg.MaxModels {
		return models
	}
	return models[:a.cfg.MaxModels]
}

func mockTokenTotals(models []string) []modelTokenTotal {
	totals := make([]modelTokenTotal, 0, len(models))
	for i, model := range models {
		totals = append(totals, modelTokenTotal{
			ModelName:      model,
			RetainedTokens: int64(i+1) * 12_500_000,
		})
	}
	return totals
}

func mockModelOptions() []modelOption {
	return []modelOption{
		{ModelName: "deepseek-v4-flash-free", RequestCount24h: 3},
		{ModelName: "Qwen/Qwen3-Embedding-8B", RequestCount24h: 1},
	}
}

func mockStatuses(models []string, window string) []modelStatus {
	cfg := timeWindows[window]
	now := time.Now().Unix()
	startTime := now - cfg.totalSeconds
	result := make([]modelStatus, 0, len(models))

	for modelIndex, name := range models {
		slots := make([]slotStatus, 0, cfg.numSlots)
		var totalReqs, totalTokens, totalSuccess, totalFailure, totalEmpty int64

		for i := 0; i < cfg.numSlots; i++ {
			slotStart := startTime + int64(i)*cfg.slotSeconds
			total := int64(0)
			tokens := int64(0)
			success := int64(0)
			failure := int64(0)
			empty := int64(0)

			switch modelIndex % 5 {
			case 0:
				total = 0
				success = 0
				if i == 10 || i == 14 {
					total = 1
					success = 1
				}
			case 1:
				total = 0
				success = 0
				if i == 9 {
					total = 1
					success = 0
				}
			case 2:
				total = int64(35 + (i%6)*7)
				success = total - int64(8+(i%5)*3)
			case 3:
				if i%4 != 0 {
					total = int64(18 + (i%4)*5)
					success = total - int64(i%2)
				}
			default:
				total = 0
				success = 0
			}

			if total > 0 {
				if success < 0 {
					success = 0
				}
				failure = total - success
				empty = failure / 2
				tokens = total * int64(8_000+(modelIndex+1)*2_500+i*137)
			}

			rate := float64(100)
			if total > 0 {
				rate = float64(success) / float64(total) * 100
			}

			slots = append(slots, slotStatus{
				Slot:          i,
				StartTime:     slotStart,
				EndTime:       slotStart + cfg.slotSeconds,
				TotalRequests: total,
				TotalTokens:   tokens,
				SuccessCount:  success,
				FailureCount:  failure,
				EmptyCount:    empty,
				SuccessRate:   roundRate(rate),
				Status:        statusColor(rate, total),
			})

			totalReqs += total
			totalTokens += tokens
			totalSuccess += success
			totalFailure += failure
			totalEmpty += empty
		}

		overall := float64(100)
		if totalReqs > 0 {
			overall = float64(totalSuccess) / float64(totalReqs) * 100
		}

		result = append(result, modelStatus{
			ModelName:     name,
			DisplayName:   name,
			TimeWindow:    window,
			TotalRequests: totalReqs,
			TotalTokens:   totalTokens,
			SuccessCount:  totalSuccess,
			FailureCount:  totalFailure,
			EmptyCount:    totalEmpty,
			SuccessRate:   roundRate(overall),
			CurrentStatus: statusColor(overall, totalReqs),
			SlotData:      slots,
		})
	}

	return result
}
