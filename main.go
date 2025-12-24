package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ==========================================
// CONFIGURATION
// ==========================================

type Config struct {
	Port           string
	DBPath         string
	WorkerPoolSize int
	MetricTimeout  time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	IdleTimeout    time.Duration

	// Dynamic Threshold Configuration:
	// OutlierStdDevs controls the sensitivity of the dynamic threshold system.
	// It specifies how many standard deviations (σ) above the mean (μ) a value
	// must be to trigger an alert.
	//
	// Formula: Threshold = μ + (OutlierStdDevs × σ)
	//
	// Common values and their meanings:
	//   1.0 → Alert if value exceeds 84% of historical data (relaxed, many alerts)
	//   2.0 → Alert if value exceeds 97.5% of historical data (moderate)
	//   3.0 → Alert if value exceeds 99.85% of historical data (strict, fewer alerts)
	//
	// Default: 3.0 (3-sigma rule - catches true outliers while minimizing false positives)
	// Environment variable: OUTLIER_STD_DEVS
	OutlierStdDevs float64 // Number of std devs for outlier detection

	// Minimum samples before alert activation
	// Prevents false alerts when there's insufficient historical data
	MinSamplesForAlert int   // Minimum samples before alert activation
	MaxRequestBodySize int64 // Maximum request body size in bytes
}

func LoadConfig() *Config {
	return &Config{
		Port:               getEnv("PORT", "8080"),
		DBPath:             getEnv("DB_PATH", "./llm_logs.db"),
		WorkerPoolSize:     getEnvInt("WORKER_POOL_SIZE", 5),
		MetricTimeout:      getEnvDuration("METRIC_TIMEOUT", 30*time.Second),
		ReadTimeout:        getEnvDuration("READ_TIMEOUT", 10*time.Second),
		WriteTimeout:       getEnvDuration("WRITE_TIMEOUT", 10*time.Second),
		IdleTimeout:        getEnvDuration("IDLE_TIMEOUT", 60*time.Second),
		OutlierStdDevs:     getEnvFloat("OUTLIER_STD_DEVS", 3.0),
		MinSamplesForAlert: getEnvInt("MIN_SAMPLES_FOR_ALERT", 1),
		MaxRequestBodySize: int64(getEnvInt("MAX_REQUEST_BODY_SIZE", 10*1024*1024)), // 10MB default
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	}
	return defaultValue
}

// ==========================================
// DATA MODELS
// ==========================================

type Interaction struct {
	ID        int64     `json:"id"`
	Input     string    `json:"input"`
	Output    string    `json:"output"`
	CreatedAt time.Time `json:"created_at"`
}

type MetricResult struct {
	ID              int64     `json:"id"`
	InteractionID   int64     `json:"interaction_id"`
	MetricName      string    `json:"metric_name"`
	InputValue      *float64  `json:"input_value,omitempty"`
	OutputValue     *float64  `json:"output_value,omitempty"`
	CalculatedAt    time.Time `json:"calculated_at"`
	CalculationTime int64     `json:"calculation_time_ms"`
	Status          string    `json:"status"` // "pending", "completed", "failed"
	Error           string    `json:"error,omitempty"`
}

type Alert struct {
	ID             int64 `json:"id"`
	MetricResultID int64 `json:"metric_result_id"`
	InteractionID  int64 `json:"interaction_id"`

	Input  string `json:"input"`
	Output string `json:"output"`

	MetricName   string    `json:"metric_name"`
	AlertType    string    `json:"alert_type"` // "threshold", "outlier"
	Value        float64   `json:"value"`
	Threshold    *float64  `json:"threshold,omitempty"`
	Message      string    `json:"message"`
	CreatedAt    time.Time `json:"created_at"`
	Acknowledged bool      `json:"acknowledged"`
}

type LogRequest struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

type LogResponse struct {
	InteractionID int64  `json:"interaction_id"`
	Message       string `json:"message"`
}

type MetricsResponse struct {
	InteractionID int64          `json:"interaction_id"`
	Metrics       []MetricResult `json:"metrics"`
}

type AlertsResponse struct {
	Alerts []Alert `json:"alerts"`
	Count  int     `json:"count"`
}

type BulkResult struct {
	Index         int    `json:"index"`
	InteractionID *int64 `json:"interaction_id,omitempty"`
	Error         string `json:"error,omitempty"`
}

type BulkLogResponse struct {
	Message      string       `json:"message"`
	SuccessCount int          `json:"success_count"`
	FailureCount int          `json:"failure_count"`
	Results      []BulkResult `json:"results"`
}

// ==========================================
// METRIC INTERFACE
// ==========================================

// Metric is a generic interface for calculating metrics on text
type Metric interface {
	Name() string
	Calculate(ctx context.Context, text string) (float64, error)
}

// ==========================================
// METRIC IMPLEMENTATIONS
// ==========================================

// LengthMetric calculates the length of text
type LengthMetric struct{}

func (m *LengthMetric) Name() string {
	return "length"
}

func (m *LengthMetric) Calculate(ctx context.Context, text string) (float64, error) {
	// Simulate potentially long calculation with context support
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
		return float64(len(text)), nil
	}
}

// WordCountMetric counts words in text
type WordCountMetric struct{}

func (m *WordCountMetric) Name() string {
	return "word_count"
}

func (m *WordCountMetric) Calculate(ctx context.Context, text string) (float64, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
		words := strings.Fields(text)
		return float64(len(words)), nil
	}
}

// SlowComplexityMetric simulates a slow metric calculation
type SlowComplexityMetric struct{}

func (m *SlowComplexityMetric) Name() string {
	return "complexity_score"
}

func (m *SlowComplexityMetric) Calculate(ctx context.Context, text string) (float64, error) {
	// Simulate expensive calculation
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	textLen := len(text)
	if textLen == 0 {
		return 0, nil
	}

	chunks := 10
	// Adjust chunks if text is too short
	if textLen < chunks {
		chunks = textLen
	}

	chunkSize := textLen / chunks
	if chunkSize == 0 {
		chunkSize = 1
	}

	score := 0.0
	for i := 0; i < chunks; i++ {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
			// Calculate complexity on chunk
			start := i * chunkSize
			end := start + chunkSize

			// Ensure we don't exceed text bounds
			if start >= textLen {
				break
			}
			if end > textLen {
				end = textLen
			}

			chunk := text[start:end]
			score += float64(len(strings.Fields(chunk))) * 0.5
		}
	}

	return score, nil
}

// ==========================================
// REPOSITORY LAYER
// ==========================================

type Repository struct {
	db     *sql.DB
	logger *slog.Logger
}

func NewRepository(dbPath string, logger *slog.Logger) (*Repository, error) {
	logger.Info("initializing database connection",
		slog.String("db_path", dbPath),
		slog.String("journal_mode", "WAL"),
	)

	db, err := sql.Open("sqlite3", dbPath+"?cache=shared&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool for SQLite
	// SQLite with WAL mode supports multiple concurrent readers
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	logger.Info("database connection pool configured",
		slog.Int("max_open_conns", 5),
		slog.Int("max_idle_conns", 2),
		slog.Duration("conn_max_lifetime", 5*time.Minute),
	)

	repo := &Repository{
		db:     db,
		logger: logger,
	}

	if err := repo.initSchema(); err != nil {
		db.Close()
		return nil, err
	}

	logger.Info("database schema initialized successfully")

	return repo, nil
}

func (r *Repository) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS interactions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		input TEXT NOT NULL,
		output TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS metric_results (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		interaction_id INTEGER NOT NULL,
		metric_name TEXT NOT NULL,
		input_value REAL,
		output_value REAL,
		calculated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		calculation_time_ms INTEGER NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		error TEXT,
		FOREIGN KEY (interaction_id) REFERENCES interactions(id) ON DELETE CASCADE
	);

	-- Alerts table stores threshold and outlier alerts
	-- The 'acknowledged' field implements the alert acknowledgment system:
	--   - 0 (false): Alert is new and requires attention
	--   - 1 (true): Alert has been reviewed and acknowledged by an operator
	-- This allows operators to mark alerts as handled without deleting them,
	-- maintaining a full audit trail of all alerts while filtering out reviewed ones.
	CREATE TABLE IF NOT EXISTS alerts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		metric_result_id INTEGER NOT NULL,
		metric_name TEXT NOT NULL,
		alert_type TEXT NOT NULL,
		value REAL NOT NULL,
		threshold REAL,
		message TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		acknowledged INTEGER DEFAULT 0, -- 0 = unacknowledged (needs review), 1 = acknowledged (reviewed)
		FOREIGN KEY (metric_result_id) REFERENCES metric_results(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_interactions_created_at ON interactions(created_at);
	CREATE INDEX IF NOT EXISTS idx_metric_results_interaction ON metric_results(interaction_id);
	CREATE INDEX IF NOT EXISTS idx_metric_results_metric ON metric_results(metric_name);
	CREATE INDEX IF NOT EXISTS idx_metric_results_id ON metric_results(id);
	CREATE INDEX IF NOT EXISTS idx_alerts_acknowledged ON alerts(acknowledged);
	CREATE INDEX IF NOT EXISTS idx_alerts_created_at ON alerts(created_at);
	CREATE INDEX IF NOT EXISTS idx_alerts_metric_result ON alerts(metric_result_id);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_alerts_unique ON alerts(metric_result_id, alert_type);
	`

	_, err := r.db.Exec(schema)
	return err
}

func (r *Repository) CreateInteraction(input, output string) (int64, error) {
	r.logger.Debug("creating interaction",
		slog.Int("input_len", len(input)),
		slog.Int("output_len", len(output)),
	)

	result, err := r.db.Exec(
		"INSERT INTO interactions (input, output) VALUES (?, ?)",
		input, output,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to insert interaction: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	r.logger.Info("interaction created", slog.Int64("interaction_id", id))

	return id, nil
}

func (r *Repository) CreateInteractionsBulk(requests []LogRequest) ([]int64, error) {
	r.logger.Info("starting bulk interaction insert",
		slog.Int("count", len(requests)),
	)

	tx, err := r.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO interactions (input, output) VALUES (?, ?)")
	if err != nil {
		return nil, fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	var ids []int64
	for i, req := range requests {
		result, err := stmt.Exec(req.Input, req.Output)
		if err != nil {
			r.logger.Error("failed to insert interaction in bulk",
				slog.Int("index", i),
				slog.String("error", err.Error()),
			)
			return nil, fmt.Errorf("failed to insert interaction: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("failed to get last insert id: %w", err)
		}
		ids = append(ids, id)
	}

	if err := tx.Commit(); err != nil {
		r.logger.Error("failed to commit bulk transaction", slog.String("error", err.Error()))
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	r.logger.Info("bulk interactions created successfully",
		slog.Int("count", len(ids)),
		slog.Int64("first_id", ids[0]),
		slog.Int64("last_id", ids[len(ids)-1]),
	)

	return ids, nil
}

func (r *Repository) GetInteraction(id int64) (*Interaction, error) {
	var interaction Interaction
	err := r.db.QueryRow(
		"SELECT id, input, output, created_at FROM interactions WHERE id = ?",
		id,
	).Scan(&interaction.ID, &interaction.Input, &interaction.Output, &interaction.CreatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query interaction: %w", err)
	}

	return &interaction, nil
}

func (r *Repository) CreateMetricResult(result *MetricResult) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO metric_results
		(interaction_id, metric_name, input_value, output_value, calculation_time_ms, status, error)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		result.InteractionID,
		result.MetricName,
		result.InputValue,
		result.OutputValue,
		result.CalculationTime,
		result.Status,
		result.Error,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to insert metric result: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return id, nil
}

func (r *Repository) GetMetricResults(interactionID int64) ([]MetricResult, error) {
	rows, err := r.db.Query(
		`SELECT id, interaction_id, metric_name, input_value, output_value,
		calculated_at, calculation_time_ms, status, error
		FROM metric_results WHERE interaction_id = ? ORDER BY calculated_at DESC`,
		interactionID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query metric results: %w", err)
	}
	defer rows.Close()

	var results []MetricResult
	for rows.Next() {
		var r MetricResult
		err := rows.Scan(
			&r.ID, &r.InteractionID, &r.MetricName,
			&r.InputValue, &r.OutputValue,
			&r.CalculatedAt, &r.CalculationTime,
			&r.Status, &r.Error,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan metric result: %w", err)
		}
		results = append(results, r)
	}

	return results, nil
}

// GetMetricStats calculates statistical metrics for dynamic threshold calculation.
//
// This function is central to the dynamic threshold system. It computes:
//   - Mean (μ): Average value across all historical data points
//   - Standard Deviation (σ): Measure of data spread/variability
//   - Min/Max: Range boundaries for the metric
//
// The statistics are used in checkAlerts() to calculate dynamic thresholds:
//
//	Dynamic Threshold = Mean + (N × Standard Deviation)
//
// For example, with OutlierStdDevs=3.0 (3-sigma rule):
//
//	Threshold = μ + 3σ
//
// This catches values in the top ~0.15% of a normal distribution (99.7% rule),
// making alerts adaptive to the actual data distribution rather than fixed limits.
func (r *Repository) GetMetricStats(metricName string, targetType string) (*MetricStats, error) {
	var column string
	if targetType == "input" {
		column = "input_value"
	} else {
		column = "output_value"
	}

	// Calculate all stats in a single efficient SQL query
	// Uses Common Table Expressions (CTEs) to compute statistics in one database round-trip
	//
	// Statistical correctness: Uses sample standard deviation (n-1 denominator)
	// rather than population standard deviation (n denominator) because we're
	// working with a sample of data, not the entire population. This is known
	// as Bessel's correction and provides an unbiased estimator.
	//
	// Formula: σ = sqrt(Σ(x - μ)² / (n - 1))
	//
	query := fmt.Sprintf(
		`WITH stats AS (
			SELECT
				COUNT(*) as count,
				AVG(%s) as mean,
				MIN(%s) as min,
				MAX(%s) as max
			FROM metric_results
			WHERE metric_name = ? AND %s IS NOT NULL AND status = 'completed'
		),
		variance_calc AS (
			SELECT
				SUM((%s - (SELECT mean FROM stats)) * (%s - (SELECT mean FROM stats))) as sum_squared_diff,
				(SELECT count FROM stats) as n
			FROM metric_results
			WHERE metric_name = ? AND %s IS NOT NULL AND status = 'completed'
		)
		SELECT
			s.count,
			s.mean,
			s.min,
			s.max,
			CASE
				WHEN v.n <= 1 THEN 0
				ELSE v.sum_squared_diff / (v.n - 1)
			END as variance
		FROM stats s, variance_calc v`,
		column, column, column, column,
		column, column, column,
	)

	var stats MetricStats
	var variance float64
	err := r.db.QueryRow(query, metricName, metricName).Scan(
		&stats.Count, &stats.Mean, &stats.Min, &stats.Max, &variance,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query metric stats: %w", err)
	}

	// Calculate standard deviation from variance using Go's math.Sqrt
	stats.StdDev = math.Sqrt(variance)

	return &stats, nil
}

func (r *Repository) CreateAlert(alert *Alert) (int64, error) {
	r.logger.Debug("creating alert",
		slog.Int64("metric_result_id", alert.MetricResultID),
		slog.String("metric_name", alert.MetricName),
		slog.String("alert_type", alert.AlertType),
		slog.Float64("value", alert.Value),
	)

	res, err := r.db.Exec(
		`INSERT INTO alerts
		(metric_result_id, metric_name, alert_type, value, threshold, message)
		VALUES (?, ?, ?, ?, ?, ?)`,
		alert.MetricResultID,
		alert.MetricName,
		alert.AlertType,
		alert.Value,
		alert.Threshold,
		alert.Message,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to insert alert: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	r.logger.Info("alert created",
		slog.Int64("alert_id", id),
		slog.String("alert_type", alert.AlertType),
		slog.String("metric_name", alert.MetricName),
	)

	return id, nil
}

// helper function to convert bool to int
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetAlerts retrieves alerts with optional filtering by acknowledgment status.
//
// Alert Acknowledgment System:
// When acknowledgedOnly=false (default): Returns only unacknowledged alerts (acknowledged=0)
// When acknowledgedOnly=true: Returns ALL alerts including acknowledged ones
//
// This enables two use cases:
//  1. Operations Dashboard: Show only new/unacknowledged alerts requiring attention
//  2. Audit/History View: Show all alerts including previously reviewed ones
//
// The query uses a conditional WHERE clause:
//
//	(? = 1 OR a.acknowledged = 0)
//	- If acknowledgedOnly=true (1): First condition true, returns all alerts
//	- If acknowledgedOnly=false (0): Second condition filters to unacknowledged only
func (r *Repository) GetAlerts(acknowledgedOnly bool) ([]Alert, error) {
	query := `
	SELECT
		a.id,
		a.metric_result_id,
		mr.interaction_id,
		i.input,
		i.output,
		a.metric_name,
		a.alert_type,
		a.value,
		a.threshold,
		a.message,
		a.created_at,
		a.acknowledged
	FROM alerts a
	JOIN metric_results mr ON a.metric_result_id = mr.id
	JOIN interactions i ON mr.interaction_id = i.id
	WHERE (? = 1 OR a.acknowledged = 0)
	ORDER BY a.created_at DESC
	`

	rows, err := r.db.Query(query, boolToInt(acknowledgedOnly))
	if err != nil {
		return nil, fmt.Errorf("failed to query alerts: %w", err)
	}
	defer rows.Close()

	var alerts []Alert

	for rows.Next() {
		var a Alert
		var acknowledged int

		if err := rows.Scan(
			&a.ID,
			&a.MetricResultID,
			&a.InteractionID,
			&a.Input,
			&a.Output,
			&a.MetricName,
			&a.AlertType,
			&a.Value,
			&a.Threshold,
			&a.Message,
			&a.CreatedAt,
			&acknowledged,
		); err != nil {
			return nil, fmt.Errorf("failed to scan alert: %w", err)
		}

		a.Acknowledged = acknowledged == 1
		alerts = append(alerts, a)
	}

	return alerts, nil

}

// AcknowledgeAlert marks an alert as reviewed/handled by an operator.
//
// Alert Acknowledgment Workflow:
//  1. Alert is created with acknowledged=0 (new, requires attention)
//  2. Operator reviews alert in dashboard (GET /api/alerts)
//  3. After investigation/action, operator acknowledges it (POST /api/alerts/{id}/acknowledge)
//  4. Alert is marked as acknowledged=1 (reviewed, no longer shown by default)
//
// Benefits:
//   - Prevents alert fatigue: Operators don't see the same alerts repeatedly
//   - Maintains audit trail: All alerts remain in database for historical analysis
//   - Team coordination: Multiple operators can see which alerts are handled
//   - No data loss: Unlike deletion, acknowledgment preserves alert history
//
// The alert remains in the database and can be retrieved with ?all=true query parameter.
func (r *Repository) AcknowledgeAlert(id int64) error {
	_, err := r.db.Exec("UPDATE alerts SET acknowledged = 1 WHERE id = ?", id)
	return err
}

func (r *Repository) Close() error {
	return r.db.Close()
}

// ==========================================
// STATISTICS
// ==========================================

type MetricStats struct {
	Count  int
	Mean   float64
	StdDev float64
	Min    float64
	Max    float64
}

// ==========================================
// METRIC CALCULATION SERVICE
// ==========================================

type MetricJob struct {
	InteractionID int64
	Input         string
	Output        string
}

type MetricService struct {
	repo          *Repository
	metrics       []Metric
	jobQueue      chan MetricJob
	workerPool    int
	metricTimeout time.Duration
	config        *Config
	logger        *slog.Logger
	wg            sync.WaitGroup
	ctx           context.Context
	cancel        context.CancelFunc
}

func NewMetricService(
	repo *Repository,
	config *Config,
	logger *slog.Logger,
) *MetricService {
	ctx, cancel := context.WithCancel(context.Background())

	service := &MetricService{
		repo:          repo,
		metrics:       []Metric{},
		jobQueue:      make(chan MetricJob, 100),
		workerPool:    config.WorkerPoolSize,
		metricTimeout: config.MetricTimeout,
		config:        config,
		logger:        logger,
		ctx:           ctx,
		cancel:        cancel,
	}

	// Register default metrics
	service.RegisterMetric(&LengthMetric{})
	service.RegisterMetric(&WordCountMetric{})
	service.RegisterMetric(&SlowComplexityMetric{})

	return service
}

func (s *MetricService) RegisterMetric(metric Metric) {
	s.metrics = append(s.metrics, metric)
	s.logger.Info("metric registered", slog.String("metric", metric.Name()))
}

func (s *MetricService) Start() {
	s.logger.Info("starting metric workers", slog.Int("workers", s.workerPool))

	for i := 0; i < s.workerPool; i++ {
		s.wg.Add(1)
		go s.worker(i)
	}
}

func (s *MetricService) Stop() {
	s.logger.Info("stopping metric service")
	s.cancel()
	close(s.jobQueue)
	s.wg.Wait()
	s.logger.Info("metric service stopped")
}

func (s *MetricService) SubmitJob(job MetricJob) {
	queueDepth := len(s.jobQueue)
	select {
	case s.jobQueue <- job:
		s.logger.Debug("job submitted",
			slog.Int64("interaction_id", job.InteractionID),
			slog.Int("queue_depth", queueDepth),
			slog.Int("queue_capacity", cap(s.jobQueue)),
		)
	case <-s.ctx.Done():
		s.logger.Warn("job rejected, service stopping",
			slog.Int64("interaction_id", job.InteractionID))
	}
}

func (s *MetricService) worker(id int) {
	defer s.wg.Done()

	s.logger.Info("worker started", slog.Int("worker_id", id))

	for {
		select {
		case job, ok := <-s.jobQueue:
			if !ok {
				s.logger.Info("worker stopped", slog.Int("worker_id", id))
				return
			}
			s.processJob(job)

		case <-s.ctx.Done():
			s.logger.Info("worker cancelled", slog.Int("worker_id", id))
			return
		}
	}
}

func (s *MetricService) processJob(job MetricJob) {
	s.logger.Info("processing job",
		slog.Int64("interaction_id", job.InteractionID),
		slog.Int("input_len", len(job.Input)),
		slog.Int("output_len", len(job.Output)),
		slog.Int("metric_count", len(s.metrics)),
	)

	for _, metric := range s.metrics {
		// Calculate on input
		s.calculateMetric(job.InteractionID, metric, job.Input, "input")

		// Calculate on output
		s.calculateMetric(job.InteractionID, metric, job.Output, "output")
	}

	s.logger.Debug("job processing completed",
		slog.Int64("interaction_id", job.InteractionID),
	)
}

func (s *MetricService) calculateMetric(
	interactionID int64,
	metric Metric,
	text string,
	targetType string,
) {
	s.logger.Debug("calculating metric",
		slog.String("metric", metric.Name()),
		slog.Int64("interaction_id", interactionID),
		slog.String("target", targetType),
		slog.Int("text_len", len(text)),
	)

	ctx, cancel := context.WithTimeout(s.ctx, s.metricTimeout)
	defer cancel()

	start := time.Now()

	value, err := metric.Calculate(ctx, text)
	calculationTime := time.Since(start).Milliseconds()

	result := &MetricResult{
		InteractionID:   interactionID,
		MetricName:      metric.Name(),
		CalculationTime: calculationTime,
	}

	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		s.logger.Error("metric calculation failed",
			slog.String("metric", metric.Name()),
			slog.Int64("interaction_id", interactionID),
			slog.String("target", targetType),
			slog.Int64("calculation_time_ms", calculationTime),
			slog.String("error", err.Error()),
		)
	} else {
		result.Status = "completed"
		if targetType == "input" {
			result.InputValue = &value
		} else {
			result.OutputValue = &value
		}
		s.logger.Debug("metric calculated successfully",
			slog.String("metric", metric.Name()),
			slog.Int64("interaction_id", interactionID),
			slog.String("target", targetType),
			slog.Float64("value", value),
			slog.Int64("calculation_time_ms", calculationTime),
		)
	}

	// Save result to database
	resultID, err := s.repo.CreateMetricResult(result)
	if err != nil {
		s.logger.Error("failed to save metric result",
			slog.String("metric", metric.Name()),
			slog.Int64("interaction_id", interactionID),
			slog.String("error", err.Error()),
		)
		return
	}

	// Check for alerts if calculation was successful
	if result.Status == "completed" {
		s.checkAlerts(resultID, metric.Name(), value, targetType)
	}
}

// checkAlerts implements the Dynamic Threshold Calculation system.
//
// DYNAMIC THRESHOLD CALCULATION:
// Instead of using fixed/hardcoded thresholds, this system calculates thresholds
// dynamically based on the statistical properties of historical data.
//
// Formula: Dynamic Threshold = μ + (N × σ)
// Where:
//
//	μ (mu) = Mean of all historical metric values
//	σ (sigma) = Standard deviation (measure of data spread)
//	N = Number of standard deviations (configured via OutlierStdDevs)
//
// Example with real data:
//
//	Historical values: [100, 105, 98, 102, 103]
//	Mean (μ) = 101.6
//	StdDev (σ) = 2.7
//	OutlierStdDevs (N) = 3.0
//	Dynamic Threshold = 101.6 + (3.0 × 2.7) = 109.7
//
//	New value of 115 → ALERT (exceeds 109.7)
//	New value of 104 → No alert (within normal range)
//
// Why Dynamic Thresholds?
//  1. Self-adjusting: Adapts to normal operating patterns automatically
//  2. Context-aware: Different metrics have different natural ranges
//  3. Statistical validity: Based on 3-sigma rule (99.7% confidence interval)
//  4. Reduces false positives: Won't alert on normal fluctuations
//  5. Catches true anomalies: Detects genuine outliers effectively
//
// The 3-Sigma Rule (OutlierStdDevs=3.0):
//
//	In a normal distribution:
//	- 68% of values fall within μ ± 1σ
//	- 95% of values fall within μ ± 2σ
//	- 99.7% of values fall within μ ± 3σ
//
//	So values beyond μ + 3σ are in the top ~0.15% (truly exceptional)
func (s *MetricService) checkAlerts(resultID int64, metricName string, value float64, targetType string) {
	// Fetch historical statistics for this metric
	stats, err := s.repo.GetMetricStats(metricName, targetType)
	if err != nil {
		s.logger.Error("failed to get metric stats", slog.String("error", err.Error()))
		return
	}

	// Require minimum data points before alerting (prevent early false positives)
	if stats.Count < s.config.MinSamplesForAlert {
		return
	}

	// Edge case: If all values are identical (σ=0), dynamic threshold calculation
	// is meaningless. Skip alerts to avoid division by zero or infinite thresholds.
	if stats.StdDev == 0 {
		s.logger.Debug("skipping alerts, no variance in data",
			slog.String("metric", metricName),
			slog.String("target", targetType),
		)
		return
	}

	// ========================================
	// CALCULATE DYNAMIC THRESHOLD
	// ========================================
	// Threshold = Mean + (N × Standard Deviation)
	// This creates an adaptive upper bound based on historical data distribution
	dynamicThreshold := stats.Mean + (s.config.OutlierStdDevs * stats.StdDev)

	// Threshold alert: Check if current value exceeds the dynamic threshold
	if value >= dynamicThreshold {
		alert := &Alert{
			MetricResultID: resultID,
			MetricName:     metricName,
			AlertType:      "threshold",
			Value:          value,
			Threshold:      &dynamicThreshold, // Store the calculated threshold for reference
			Message: fmt.Sprintf(
				"Metric '%s' (%s) value %.2f exceeds dynamic threshold %.2f (mean: %.2f, stddev: %.2f)",
				metricName, targetType, value, dynamicThreshold, stats.Mean, stats.StdDev,
			),
		}

		if _, err := s.repo.CreateAlert(alert); err != nil {
			// Ignore duplicate alert errors (from unique constraint)
			if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
				s.logger.Error("failed to create threshold alert", slog.String("error", err.Error()))
			}
		} else {
			s.logger.Warn("threshold alert created",
				slog.String("metric", metricName),
				slog.Float64("value", value),
				slog.Float64("threshold", dynamicThreshold),
			)
		}
	}

	// Outlier alert: Check if value is statistically abnormal (far from mean in either direction)
	// This catches both extremely high AND extremely low values
	if math.Abs(value-stats.Mean) > (s.config.OutlierStdDevs * stats.StdDev) {
		alert := &Alert{
			MetricResultID: resultID,
			MetricName:     metricName,
			AlertType:      "outlier",
			Value:          value,
			Message: fmt.Sprintf(
				"Metric '%s' (%s) value %.2f is an outlier (%.1f std devs from mean %.2f)",
				metricName, targetType, value, math.Abs(value-stats.Mean)/stats.StdDev, stats.Mean,
			),
		}

		if _, err := s.repo.CreateAlert(alert); err != nil {
			// Ignore duplicate alert errors (from unique constraint)
			if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
				s.logger.Error("failed to create outlier alert", slog.String("error", err.Error()))
			}
		} else {
			s.logger.Warn("outlier alert created",
				slog.String("metric", metricName),
				slog.Float64("value", value),
				slog.Float64("mean", stats.Mean),
			)
		}
	}
}

// ==========================================
// HTTP HANDLER LAYER
// ==========================================

type Handler struct {
	repo    *Repository
	service *MetricService
	logger  *slog.Logger
}

func NewHandler(repo *Repository, service *MetricService, logger *slog.Logger) *Handler {
	return &Handler{
		repo:    repo,
		service: service,
		logger:  logger,
	}
}

// POST /api/interactions
func (h *Handler) LogInteraction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Validate Content-Type
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" && !strings.HasPrefix(contentType, "application/json;") {
		respondError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	var req LogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	if req.Input == "" || req.Output == "" {
		respondError(w, http.StatusBadRequest, "input and output are required")
		return
	}

	// Save interaction
	interactionID, err := h.repo.CreateInteraction(req.Input, req.Output)
	if err != nil {
		h.logger.Error("failed to create interaction", slog.String("error", err.Error()))
		respondError(w, http.StatusInternalServerError, "failed to save interaction")
		return
	}

	// Submit async job for metric calculation
	h.service.SubmitJob(MetricJob{
		InteractionID: interactionID,
		Input:         req.Input,
		Output:        req.Output,
	})

	respondJSON(w, http.StatusCreated, LogResponse{
		InteractionID: interactionID,
		Message:       "interaction logged, metrics calculation in progress",
	})
}

// POST /api/interactions/bulk
func (h *Handler) BulkLogInteractions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Validate Content-Type
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" && !strings.HasPrefix(contentType, "application/json;") {
		respondError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	var requests []LogRequest
	if err := json.NewDecoder(r.Body).Decode(&requests); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON array: %v", err))
		return
	}

	if len(requests) == 0 {
		respondError(w, http.StatusBadRequest, "empty request array")
		return
	}

	if len(requests) > 100 {
		respondError(w, http.StatusBadRequest, "maximum 100 interactions per bulk request")
		return
	}

	// Validate and filter requests
	var validRequests []LogRequest
	var results []BulkResult

	for i, req := range requests {
		if req.Input == "" || req.Output == "" {
			results = append(results, BulkResult{
				Index: i,
				Error: "input and output are required",
			})
			continue
		}
		validRequests = append(validRequests, req)
	}

	// Use bulk insert with transaction
	if len(validRequests) > 0 {
		interactionIDs, err := h.repo.CreateInteractionsBulk(validRequests)
		if err != nil {
			h.logger.Error("failed to create bulk interactions", slog.String("error", err.Error()))
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save interactions: %v", err))
			return
		}

		// Track which valid request index we're on
		validIdx := 0
		for i := range requests {
			// Check if this index was skipped (in results as error)
			wasSkipped := false
			for _, res := range results {
				if res.Index == i {
					wasSkipped = true
					break
				}
			}

			if !wasSkipped {
				interactionID := interactionIDs[validIdx]
				results = append(results, BulkResult{
					Index:         i,
					InteractionID: &interactionID,
				})

				// Submit async job for metric calculation
				h.service.SubmitJob(MetricJob{
					InteractionID: interactionID,
					Input:         validRequests[validIdx].Input,
					Output:        validRequests[validIdx].Output,
				})

				validIdx++
			}
		}
	}

	// Calculate success/failure counts
	successCount := 0
	for _, res := range results {
		if res.Error == "" {
			successCount++
		}
	}

	respondJSON(w, http.StatusCreated, BulkLogResponse{
		Message:      "bulk interactions processed",
		SuccessCount: successCount,
		FailureCount: len(results) - successCount,
		Results:      results,
	})
}

// GET /api/interactions/{id}/metrics
func (h *Handler) GetMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/interactions/")

	if !strings.HasSuffix(path, "/metrics") {
		respondError(w, http.StatusNotFound, "not found")
		return
	}

	idStr := strings.TrimSuffix(path, "/metrics")

	// Validate no extra slashes remain
	if strings.Contains(idStr, "/") {
		respondError(w, http.StatusNotFound, "not found")
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid interaction ID")
		return
	}

	// Check if interaction exists
	interaction, err := h.repo.GetInteraction(id)
	if err != nil {
		h.logger.Error("failed to get interaction", slog.String("error", err.Error()))
		respondError(w, http.StatusInternalServerError, "failed to retrieve interaction")
		return
	}

	if interaction == nil {
		respondError(w, http.StatusNotFound, "interaction not found")
		return
	}

	// Get metrics
	metrics, err := h.repo.GetMetricResults(id)
	if err != nil {
		h.logger.Error("failed to get metrics", slog.String("error", err.Error()))
		respondError(w, http.StatusInternalServerError, "failed to retrieve metrics")
		return
	}

	if metrics == nil {
		metrics = []MetricResult{}
	}

	respondJSON(w, http.StatusOK, MetricsResponse{
		InteractionID: id,
		Metrics:       metrics,
	})
}

// GET /api/alerts - Retrieve alerts with acknowledgment filtering
//
// API Endpoint for Alert Acknowledgment System:
//
// Usage:
//
//	GET /api/alerts           → Returns only unacknowledged alerts (default behavior)
//	GET /api/alerts?all=true  → Returns all alerts including acknowledged ones
//
// Use Cases:
//  1. Operations Dashboard: Default call shows only alerts requiring attention
//  2. Audit/Compliance: Use ?all=true to review all historical alerts
//  3. Team Handoff: Check which alerts colleagues have already handled
//
// Response includes 'acknowledged' boolean field for each alert:
//   - false: Alert needs review
//   - true: Alert has been acknowledged via POST /api/alerts/{id}/acknowledge
func (h *Handler) GetAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Query parameter: show all or only unacknowledged
	showAll := r.URL.Query().Get("all") == "true"

	alerts, err := h.repo.GetAlerts(showAll)
	if err != nil {
		h.logger.Error("failed to get alerts", slog.String("error", err.Error()))
		respondError(w, http.StatusInternalServerError, "failed to retrieve alerts")
		return
	}

	if alerts == nil {
		alerts = []Alert{}
	}

	respondJSON(w, http.StatusOK, AlertsResponse{
		Alerts: alerts,
		Count:  len(alerts),
	})
}

// POST /api/alerts/{id}/acknowledge - Mark an alert as reviewed
//
// API Endpoint for Alert Acknowledgment:
//
// Usage:
//
//	POST /api/alerts/123/acknowledge
//
// This endpoint implements the acknowledgment action in the alert workflow:
//  1. Operator reviews an alert from GET /api/alerts
//  2. Investigates the issue (checks logs, metrics, system state)
//  3. Takes appropriate action (scales resources, fixes bug, etc.)
//  4. Calls this endpoint to mark the alert as handled
//  5. Alert disappears from default alert list but remains in database
//
// Example Workflow:
//
//	GET /api/alerts
//	→ Returns: [{id: 123, message: "CPU usage high", acknowledged: false}, ...]
//
//	[Operator investigates and resolves issue]
//
//	POST /api/alerts/123/acknowledge
//	→ Returns: {message: "alert acknowledged"}
//
//	GET /api/alerts
//	→ Alert 123 no longer appears (only unacknowledged alerts shown)
//
//	GET /api/alerts?all=true
//	→ Returns: [{id: 123, message: "CPU usage high", acknowledged: true}, ...]
func (h *Handler) AcknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/alerts/")

	if !strings.HasSuffix(path, "/acknowledge") {
		respondError(w, http.StatusNotFound, "not found")
		return
	}

	idStr := strings.TrimSuffix(path, "/acknowledge")

	// Validate no extra slashes remain
	if strings.Contains(idStr, "/") {
		respondError(w, http.StatusNotFound, "not found")
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid alert ID")
		return
	}

	if err := h.repo.AcknowledgeAlert(id); err != nil {
		h.logger.Error("failed to acknowledge alert", slog.String("error", err.Error()))
		respondError(w, http.StatusInternalServerError, "failed to acknowledge alert")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"message": "alert acknowledged",
	})
}

// GET /health
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{
		"status": "healthy",
	})
}

// ==========================================
// MIDDLEWARE
// ==========================================

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func LoggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			wrapped := &responseWriter{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
			}

			next.ServeHTTP(wrapped, r)

			logger.Info("request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", wrapped.statusCode),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

func RecoveryMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					logger.Error("panic recovered",
						slog.Any("error", err),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
					)
					respondError(w, http.StatusInternalServerError, "internal server error")
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}

func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func LimitRequestBody(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Limit request body size to prevent resource exhaustion
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// ==========================================
// ROUTER
// ==========================================

func NewRouter(handler *Handler, logger *slog.Logger, cfg *Config) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", handler.Health)
	mux.HandleFunc("/api/interactions/bulk", handler.BulkLogInteractions)
	mux.HandleFunc("/api/interactions", handler.LogInteraction)
	mux.HandleFunc("/api/interactions/", handler.GetMetrics)
	mux.HandleFunc("/api/alerts", handler.GetAlerts)
	mux.HandleFunc("/api/alerts/", handler.AcknowledgeAlert)

	var h http.Handler = mux
	h = CORSMiddleware(h)
	h = LimitRequestBody(cfg.MaxRequestBodySize)(h)
	h = RecoveryMiddleware(logger)(h)
	h = LoggingMiddleware(logger)(h)

	return h
}

// ==========================================
// HELPER FUNCTIONS
// ==========================================

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{
		"error": message,
	})
}

// ==========================================
// MAIN - START SERVER
// ==========================================

func main() {
	// Initialize logger
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Load config
	cfg := LoadConfig()

	logger.Info("==============================================")
	logger.Info("🚀 Starting LLM Logging System")
	logger.Info("==============================================")
	logger.Info("configuration loaded",
		slog.String("port", cfg.Port),
		slog.String("db_path", cfg.DBPath),
		slog.Int("worker_pool_size", cfg.WorkerPoolSize),
		slog.Duration("metric_timeout", cfg.MetricTimeout),
		slog.Duration("read_timeout", cfg.ReadTimeout),
		slog.Duration("write_timeout", cfg.WriteTimeout),
		slog.Float64("outlier_std_devs", cfg.OutlierStdDevs),
		slog.Int("min_samples_for_alert", cfg.MinSamplesForAlert),
		slog.Int64("max_request_body_size", cfg.MaxRequestBodySize),
	)

	// Initialize repository
	repo, err := NewRepository(cfg.DBPath, logger)
	if err != nil {
		logger.Error("failed to initialize repository", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer repo.Close()

	// Initialize metric service
	metricService := NewMetricService(repo, cfg, logger)
	metricService.Start()
	defer metricService.Stop()

	// Initialize handler
	handler := NewHandler(repo, metricService, logger)

	// Create router
	router := NewRouter(handler, logger, cfg)

	// Configure server
	server := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	// Start server in goroutine
	go func() {
		logger.Info("==============================================")
		logger.Info("📡 HTTP Server Ready",
			slog.String("address", "http://localhost:"+cfg.Port),
		)
		logger.Info("==============================================")
		logger.Info("📋 Available Endpoints:")
		logger.Info("  POST   /api/interactions           - Log a new interaction")
		logger.Info("  POST   /api/interactions/bulk      - Log multiple interactions")
		logger.Info("  GET    /api/interactions/{id}/metrics - Get metrics for an interaction")
		logger.Info("  GET    /api/alerts                  - Get all alerts (add ?all=true for acknowledged)")
		logger.Info("  POST   /api/alerts/{id}/acknowledge - Acknowledge an alert")
		logger.Info("  GET    /health                      - Health check")
		logger.Info("==============================================")

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("❌ server failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("==============================================")
	logger.Info("🛑 Shutdown signal received, gracefully stopping...")
	logger.Info("==============================================")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("❌ shutdown failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	logger.Info("==============================================")
	logger.Info("✅ Server exited successfully")
	logger.Info("==============================================")
}
