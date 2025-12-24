# DeepChecks API - LLM Logging & Monitoring System

A high-performance Go-based REST API for logging LLM (Large Language Model) interactions with intelligent metric calculation and dynamic threshold-based alerting.

## Features

- **Interaction Logging**: Store LLM input/output pairs with timestamps
- **Asynchronous Metric Calculation**: Worker pool-based metric processing
- **Dynamic Threshold Alerts**: Statistical anomaly detection using 3-sigma rule
- **Bulk Operations**: Efficient batch logging with transaction support
- **Alert Management**: Acknowledgment system for tracking reviewed alerts
- **RESTful API**: Clean HTTP endpoints with comprehensive error handling
- **SQLite Database**: Lightweight, file-based storage with WAL mode

## Built-in Metrics

1. **Length**: Character count of input/output text
2. **Word Count**: Number of words in input/output text
3. **Complexity Score**: Simulated metric demonstrating async processing

## Dynamic Threshold System

The system automatically calculates thresholds based on historical data using statistical analysis:

**Formula**: `Threshold = Mean + (N × Standard Deviation)`

- **Mean (μ)**: Average of all historical values
- **Standard Deviation (σ)**: Measure of data variability
- **N**: Configurable multiplier (default: 3.0)

Example:
- Historical values: [100, 105, 98, 102, 103]
- Mean: 101.6, StdDev: 2.7
- Threshold: 101.6 + (3.0 × 2.7) = **109.7**
- A value of 115 triggers an alert

This approach provides:
- Self-adjusting thresholds that adapt to normal patterns
- Statistical validity (99.7% confidence interval)
- Reduced false positives
- Effective anomaly detection

## Quick Start

### Prerequisites

- Go 1.25.3 or higher
- SQLite3

### Installation

```bash
# Clone the repository
git clone https://github.com/YOUR_USERNAME/deepchecks_api.git
cd deepchecks_api

# Install dependencies
go mod download

# Run the application
go run main.go
```

The server will start on `http://localhost:8080` (configurable via environment variables).

### Building

```bash
# Build executable
go build -o llm_logs main.go

# Run the executable
./llm_logs
```

## Configuration

Configure the application using environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP server port |
| `DB_PATH` | `./llm_logs.db` | SQLite database file path |
| `WORKER_POOL_SIZE` | `5` | Number of metric calculation workers |
| `METRIC_TIMEOUT` | `30s` | Timeout for metric calculations |
| `READ_TIMEOUT` | `10s` | HTTP read timeout |
| `WRITE_TIMEOUT` | `10s` | HTTP write timeout |
| `IDLE_TIMEOUT` | `60s` | HTTP idle timeout |
| `OUTLIER_STD_DEVS` | `3.0` | Standard deviations for outlier detection |
| `MIN_SAMPLES_FOR_ALERT` | `1` | Minimum samples before triggering alerts |
| `MAX_REQUEST_BODY_SIZE` | `10485760` | Max request body size (10MB) |

Example:
```bash
PORT=9000 OUTLIER_STD_DEVS=2.5 go run main.go
```

## API Endpoints

### Log Interaction

```bash
POST /api/interactions
Content-Type: application/json

{
  "input": "What is the capital of France?",
  "output": "The capital of France is Paris."
}
```

Response:
```json
{
  "interaction_id": 1,
  "message": "interaction logged, metrics calculation in progress"
}
```

### Bulk Log Interactions

```bash
POST /api/interactions/bulk
Content-Type: application/json

[
  {
    "input": "Question 1",
    "output": "Answer 1"
  },
  {
    "input": "Question 2",
    "output": "Answer 2"
  }
]
```

Response:
```json
{
  "message": "bulk interactions processed",
  "success_count": 2,
  "failure_count": 0,
  "results": [
    {"index": 0, "interaction_id": 1},
    {"index": 1, "interaction_id": 2}
  ]
}
```

### Get Metrics

```bash
GET /api/interactions/{id}/metrics
```

Response:
```json
{
  "interaction_id": 1,
  "metrics": [
    {
      "id": 1,
      "interaction_id": 1,
      "metric_name": "length",
      "input_value": 32,
      "output_value": 35,
      "calculated_at": "2025-12-24T10:30:00Z",
      "calculation_time_ms": 5,
      "status": "completed"
    }
  ]
}
```

### Get Alerts

```bash
# Get unacknowledged alerts only (default)
GET /api/alerts

# Get all alerts including acknowledged
GET /api/alerts?all=true
```

Response:
```json
{
  "alerts": [
    {
      "id": 1,
      "metric_result_id": 5,
      "interaction_id": 3,
      "input": "Sample input",
      "output": "Sample output",
      "metric_name": "length",
      "alert_type": "threshold",
      "value": 150.5,
      "threshold": 120.3,
      "message": "Metric 'length' (output) value 150.50 exceeds dynamic threshold 120.30",
      "created_at": "2025-12-24T10:35:00Z",
      "acknowledged": false
    }
  ],
  "count": 1
}
```

### Acknowledge Alert

```bash
POST /api/alerts/{id}/acknowledge
```

Response:
```json
{
  "message": "alert acknowledged"
}
```

### Health Check

```bash
GET /health
```

Response:
```json
{
  "status": "healthy"
}
```

## Architecture

### Components

1. **Repository Layer**: Database operations with SQLite + WAL mode
2. **Metric Service**: Asynchronous worker pool for metric calculations
3. **Handler Layer**: HTTP request/response handling
4. **Middleware**: Logging, recovery, CORS, request size limiting

### Database Schema

**interactions**
- Stores LLM input/output pairs

**metric_results**
- Stores calculated metrics for each interaction

**alerts**
- Stores threshold and outlier alerts with acknowledgment status

### Worker Pool

Metrics are calculated asynchronously using a worker pool pattern:
- Configurable number of workers (default: 5)
- Job queue with 100-item capacity
- Context-based cancellation support
- Graceful shutdown handling

## Alert Types

### Threshold Alert
Triggered when a metric value exceeds the dynamic threshold (μ + Nσ)

### Outlier Alert
Triggered when a metric value is statistically abnormal (far from mean in either direction)

## Development

### Project Structure

```
deepchecks_api/
├── main.go           # Main application code
├── go.mod            # Go module dependencies
├── go.sum            # Dependency checksums
├── llm_logs.db       # SQLite database (auto-generated)
└── README.md         # This file
```

### Adding Custom Metrics

Implement the `Metric` interface:

```go
type CustomMetric struct{}

func (m *CustomMetric) Name() string {
    return "custom_metric"
}

func (m *CustomMetric) Calculate(ctx context.Context, text string) (float64, error) {
    // Your calculation logic here
    return someValue, nil
}

// Register in NewMetricService
service.RegisterMetric(&CustomMetric{})
```

## Testing

Example using curl:

```bash
# Log an interaction
curl -X POST http://localhost:8080/api/interactions \
  -H "Content-Type: application/json" \
  -d '{"input":"Hello","output":"Hi there!"}'

# Get metrics (replace {id} with interaction_id from previous response)
curl http://localhost:8080/api/interactions/1/metrics

# Get alerts
curl http://localhost:8080/api/alerts

# Acknowledge an alert
curl -X POST http://localhost:8080/api/alerts/1/acknowledge
```

## Production Considerations

1. **Database**: Consider PostgreSQL/MySQL for production workloads
2. **Monitoring**: Integrate with your observability stack (Prometheus, Grafana)
3. **Authentication**: Add API key or OAuth authentication
4. **Rate Limiting**: Implement rate limiting middleware
5. **HTTPS**: Use TLS/SSL in production
6. **Scaling**: Deploy multiple instances behind a load balancer

## License

[Your License Here]

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.

## Author

[Your Name]
