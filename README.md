# SQL Alert Manager

A lightweight tool that executes SQL queries against a database and sends alerts to Prometheus AlertManager based on the query results. This enables database-driven alerting for any SQL-compatible data source.

## Features

- **SQL-based alerting**: Define alerts using standard SQL queries
- **Prometheus AlertManager integration**: Seamlessly integrates with existing AlertManager workflows
- **State persistence**: Tracks alert state between restarts to prevent alert flapping
- **Configurable evaluation frequency**: Set different evaluation intervals per alert rule
- **Alert deduplication**: Prevents duplicate alerts and properly handles alert resolution
- **Multi-rule support**: Run multiple alert rules concurrently
- **Graceful shutdown**: Handles SIGTERM/SIGINT for clean shutdowns

## Installation

### Prerequisites

- Go 1.25.1 or later
- Access to a SQL database (PostgreSQL, MySQL, etc.)
- Running Prometheus AlertManager instance

### Build from source

```bash
git clone https://github.com/sklarsa/sql-alertmanager
cd sql-alertmanager
go build -o sql-alertmanager .
```

## Configuration

### Command Line Options

```bash
./sql-alertmanager [options]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-driver` | `pgx` | SQL driver to use |
| `-alertManagerHost` | `localhost` | AlertManager hostname |
| `-alertManagerPath` | `/api/v2/` | AlertManager API path |
| `-config` | `./config.yaml` | Path to configuration file |
| `-maxRequestTimeout` | `5s` | AlertManager request timeout |
| `-state` | `./alertstate.json` | Path to state persistence file |
| `-debug` | `false` | Enable debug logging |

### Configuration File

Create a `config.yaml` file with your database connection and alert rules:

```yaml
# PostgreSQL example (using pgx driver)
db: "postgres://user:password@localhost/database?sslmode=disable"

# Alternative database examples:
# MySQL: "user:password@tcp(localhost:3306)/database"
# SQLite: "file:./database.db"
# SQL Server: "sqlserver://user:password@localhost:1433?database=mydb"
rules:
  - name: "high_error_rate"
    query: |
      SELECT
        service_name,
        error_count,
        'High error rate detected' as description
      FROM error_metrics
      WHERE error_count > 100
    evaluateFreq: "30s"
    for: "2m"
    labelCols:
      - "service_name"
    annotationCols:
      - "description"
      - "error_count"

  - name: "disk_usage_high"
    query: |
      SELECT
        hostname,
        disk_usage_percent,
        'Disk usage critical' as summary
      FROM disk_metrics
      WHERE disk_usage_percent > 90
    evaluateFreq: "1m"
    for: "5m"
    labelCols:
      - "hostname"
    annotationCols:
      - "summary"
      - "disk_usage_percent"
```

#### Rule Configuration

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Unique name for the alert rule (becomes `alertname` label) |
| `query` | string | SQL query to execute |
| `evaluateFreq` | duration | How often to run the query (e.g., "30s", "5m") |
| `for` | duration | How long condition must persist before firing |
| `labelCols` | []string | Query columns to use as Prometheus labels |
| `annotationCols` | []string | Query columns to use as Prometheus annotations |

## Usage

1. **Start AlertManager** (if not already running):
   ```bash
   alertmanager --config.file=alertmanager.yml
   ```

2. **Create your configuration file** (`config.yaml`) with database connection and rules

3. **Run the SQL Alert Manager**:
   ```bash
   ./sql-alertmanager -config=config.yaml -alertManagerHost=localhost:9093
   ```

The application will:
- Connect to your database using the specified driver
- Start evaluating each rule according to its `evaluateFreq`
- Send alerts to AlertManager when conditions are met
- Track alert state to handle proper resolution
- Continue running until stopped with SIGTERM/SIGINT

## How It Works

1. **Query Execution**: Each rule runs its SQL query at the specified frequency
2. **Result Processing**: Query results are converted to AlertManager alerts
   - Each row becomes a potential alert
   - `labelCols` become Prometheus labels (for grouping/routing)
   - `annotationCols` become annotations (for additional context)
3. **State Tracking**: Tracks when alerts first fired to implement the `for` duration
4. **Alert Management**:
   - New alerts are created when conditions first appear
   - Existing alerts are updated if still firing
   - Resolved alerts are sent with an end time when conditions disappear

## Database Drivers

The application includes built-in support for the 5 most common SQL drivers:

| Database | Driver Name | Connection String Example |
|----------|-------------|---------------------------|
| **PostgreSQL** | `pgx` (default) | `postgres://user:pass@localhost/db?sslmode=disable` |
| **PostgreSQL** | `postgres` | `postgres://user:pass@localhost/db?sslmode=disable` |
| **MySQL** | `mysql` | `user:pass@tcp(localhost:3306)/database` |
| **SQLite** | `sqlite3` | `file:database.db` or `/path/to/database.db` |
| **SQL Server** | `sqlserver` | `sqlserver://user:pass@localhost:1433?database=db` |

### Included Drivers

The following drivers are pre-imported and ready to use:

- `github.com/jackc/pgx/v5/stdlib` - PostgreSQL (pgx driver)
- `github.com/lib/pq` - PostgreSQL (postgres driver)
- `github.com/go-sql-driver/mysql` - MySQL
- `github.com/mattn/go-sqlite3` - SQLite
- `github.com/microsoft/go-mssqldb` - SQL Server

No additional installation is required - simply specify the appropriate driver name and connection string in your configuration.

## State Persistence

The application maintains state in a JSON file (default: `alertstate.json`) to track:
- When each unique alert condition was first seen
- Which alerts should fire based on the `for` duration

This prevents alert flapping during restarts and ensures consistent behavior.

## Examples

### Database Monitoring
```yaml
- name: "connection_pool_exhausted"
  query: |
    SELECT
      database_name,
      active_connections,
      max_connections
    FROM pg_stat_database
    WHERE active_connections > max_connections * 0.9
  evaluateFreq: "15s"
  for: "1m"
  labelCols: ["database_name"]
  annotationCols: ["active_connections", "max_connections"]
```

### Application Metrics
```yaml
- name: "response_time_high"
  query: |
    SELECT
      endpoint,
      avg_response_time,
      request_count
    FROM api_metrics
    WHERE avg_response_time > 5000
    AND request_count > 10
  evaluateFreq: "30s"
  for: "3m"
  labelCols: ["endpoint"]
  annotationCols: ["avg_response_time", "request_count"]
```

## TODO

- [ ] Use the actual alert timestamp for 'for' calculations instead of when it was evaluated
- [ ] Generate alert URLs based on the query. Can do this with QuestDB's REST API for example
- [ ] Batch state flushes for performance optimization
- [ ] Add config reload on SIGHUP signal
- [ ] Add metrics endpoint for Prometheus scraping
- [ ] Implement retry logic on API errors
- [ ] Add TestContainers-powered unit tests

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Contributing

Contributions are welcome! Please feel free to submit issues and pull requests.
