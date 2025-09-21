package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/goccy/go-yaml"
	"github.com/prometheus/alertmanager/api/v2/client"
	"github.com/prometheus/alertmanager/api/v2/client/alert"
	"github.com/prometheus/alertmanager/api/v2/models"
)

type AlertRule struct {
	Name           string        `yaml:"name"`
	Query          string        `yaml:"query"`
	EvaluateFreq   time.Duration `yaml:"evaluateFreq"`
	LabelCols      []string      `yaml:"labelCols"`
	AnnotationCols []string      `yaml:"annotationCols"`
	For            time.Duration `yaml:"for"`
}

type Config struct {
	Db    string      `yaml:"db"`
	Rules []AlertRule `yaml:"rules"`
}

func main() {
	driver := flag.String("driver", "pgx", "sql driver to use")
	amHost := flag.String("alertManagerHost", "localhost", "hostname for alert manager")
	amPath := flag.String("alertManagerPath", "/api/v2/", "alert manager v2 api path")
	configPath := flag.String("config", "./config.yaml", "path to config")
	maxRequestTimeout := flag.Duration("maxRequestTimeout", time.Second*5, "request to alertmanager timeout")
	statePath := flag.String("state", "./alertstate.json", "path of local state file")
	debug := flag.Bool("debug", false, "show debug logs")
	flag.Parse()

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: func() slog.Leveler {
			if *debug {
				return slog.LevelDebug
			}
			return slog.LevelInfo
		}(),
	})
	slog.SetDefault(slog.New(handler))

	// Load the state store
	store := NewManager(*statePath)
	if err := store.Load(); err != nil {
		slog.Error("error loading state file",
			"path", *statePath,
			"error", err,
		)
	}

	// Unmarshal the config file
	conf := Config{}
	f, err := os.Open(*configPath)
	if err != nil {
		log.Fatalf("error opening config file: %s", err)
	}

	decoder := yaml.NewDecoder(f, yaml.DisallowUnknownField(), yaml.UseJSONUnmarshaler())
	err = decoder.Decode(&conf)
	f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		log.Fatalf("error parsing yaml: %s", err)
	}

	// Set up the alertmanager client
	amClient := client.NewHTTPClientWithConfig(
		strfmt.Default,
		client.DefaultTransportConfig().
			WithHost(*amHost).
			WithBasePath(*amPath),
	)

	// Set up signal-based cancellation behavior
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	wg := &sync.WaitGroup{}

	// Open and configure a connection to the target database
	db, err := sql.Open(*driver, conf.Db)
	if err != nil {
		log.Fatalf(
			"error opening connection with %q driver: %s\n",
			*driver,
			err,
		)
	}
	defer db.Close()

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Fatalf("DB connection test failed: %s", err)
	}

	// Create a goroutine-per-rule that will evaluate each rule
	// and post updates to alertmanager as state changes
	for _, r := range conf.Rules {
		wg.Add(1)
		go func(r AlertRule) {
			defer wg.Done()
			slog.Info("registered rule", "name", r.Name)

			ticker := time.NewTicker(r.EvaluateFreq)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					slog.Info("rule stopped", "name", r.Name)
					return
				case <-ticker.C:
					slog.Debug("executing query",
						"name", r.Name,
						"query", r.Query,
					)

					// Execute the rule query and parse results to
					// create a list of firingAlerts.
					firingAlerts := models.PostableAlerts{}
					func() {
						queryCtx, cancel := context.WithTimeout(ctx, r.EvaluateFreq/2)
						defer cancel()

						rows, err := db.QueryContext(queryCtx, r.Query)
						if err != nil {
							slog.Error("query failed",
								"name", r.Name,
								"error", err.Error())
							return
						}
						defer rows.Close()

						cols, err := rows.Columns()
						if err != nil {
							slog.Error("error getting columns",
								"name", r.Name,
								"error", err)
							return
						}

						colSet := map[string]struct{}{}
						for _, c := range cols {
							colSet[c] = struct{}{}
						}
						for _, c := range append(r.LabelCols, r.AnnotationCols...) {
							if _, ok := colSet[c]; !ok {
								slog.Warn("column missing from query result", "name", r.Name, "column", c)
							}
						}

						for rows.Next() {
							raw := make([]sql.RawBytes, len(cols))
							ptrs := make([]any, len(cols))
							for i := range raw {
								ptrs[i] = &raw[i]
							}

							if err = rows.Scan(ptrs...); err != nil {
								slog.Error("error scanning row",
									"name", r.Name,
									"error", err.Error())
								return
							}

							row := make(map[string]string, len(cols))
							for i, col := range cols {
								if raw[i] == nil {
									row[col] = ""
								} else {
									row[col] = string(raw[i])
								}
							}

							annotations := map[string]string{}
							labels := map[string]string{}

							for _, col := range r.AnnotationCols {
								val := row[col]
								if val != "" {
									annotations[col] = val
								}
							}

							for _, col := range r.LabelCols {
								val := row[col]
								if val != "" {
									labels[col] = val
								}
							}
							labels["alertname"] = r.Name

							firingAlerts = append(firingAlerts, &models.PostableAlert{
								Annotations: annotations,
								StartsAt:    strfmt.DateTime(time.Now()),
								Alert: models.Alert{
									Labels: labels,
								},
							})
						}

						err = rows.Err()
						if err != nil {
							slog.Error("error processing query data",
								"name", r.Name,
								"error", err.Error(),
							)
							return
						}
					}()

					if len(firingAlerts) == 0 {
						slog.Debug("no alerts found",
							"name", r.Name,
						)
						return
					}

					slog.Info("alerts found",
						"name", r.Name,
						"count", len(firingAlerts),
					)

					// Mark active alerts in store and
					// drop any alerts that have not hit the "for" threshold
					filteredAlerts := models.PostableAlerts{}
					for _, alert := range firingAlerts {
						k := key(alert.Labels)
						store.MarkActive(k)
						if store.ShouldFire(k, r.For) {
							filteredAlerts = append(filteredAlerts, alert)
						}
					}

					// Get existing alerts from alertmanager
					getCtx, cancel := context.WithTimeout(ctx, *maxRequestTimeout)

					active := true
					params := alert.NewGetAlertsParams().
						WithContext(getCtx).
						WithActive(&active).
						WithFilter([]string{
							fmt.Sprintf("alertname=%s", r.Name),
						})

					resp, err := amClient.Alert.GetAlerts(params)
					cancel()
					if err != nil {
						slog.Error("failed to get active alerts",
							"name", r.Name,
							"error", err)
						return
					}
					existingAlerts := resp.Payload

					// We care about alerts in 3 buckets:
					// 1. New alerts
					// 2. Old alerts that are still firing
					// 3. Old alerts that have stopped firing
					//
					// We already have alerts from buckets 1 and 2 in the
					// firingAlerts slice. We need to iterate through each
					// existingAlert, compare it to each firingAlert,
					// and append it to firingAlert (with an end time) if it
					// is no longer firing.
					for _, existing := range existingAlerts {
						var found bool
						for idx := range firingAlerts {
							// todo: use key func here once we can cache the results
							if reflect.DeepEqual(existing.Labels, firingAlerts[idx].Labels) {
								// If we find that the alert is still firing, we
								// need to ensure that its StartsAt matches the old alert
								// for consistency.
								firingAlerts[idx].StartsAt = *existing.StartsAt
								found = true
								break
							}
						}
						if !found {
							filteredAlerts = append(filteredAlerts, &models.PostableAlert{
								Alert:    existing.Alert,
								StartsAt: *existing.StartsAt,
								EndsAt:   strfmt.DateTime(time.Now()),
							})
							store.MarkResolved(key(existing.Labels))
						}
					}

					// Post all alerts to alertmanager
					func() {
						postCtx, cancel := context.WithTimeout(ctx, *maxRequestTimeout)
						defer cancel()

						params := alert.NewPostAlertsParams().
							WithContext(postCtx).
							WithAlerts(filteredAlerts)

						resp, err := amClient.Alert.PostAlerts(params)
						if err != nil {
							slog.Error("failed to send alerts",
								"name", r.Name,
								"count", len(filteredAlerts))
							return
						}

						if resp.IsSuccess() {
							slog.Info("alerts sent",
								"name", r.Name,
								"count", len(filteredAlerts))
						} else {
							slog.Error("failed to send alerts",
								"name", r.Name,
								"count", len(filteredAlerts),
								"code", resp.Code(),
								"error", resp.Error(),
							)
						}
					}()
				}
			}

		}(r)
	}

	sig := <-sigs
	log.Println("\nReceived signal:", sig)
	log.Println("Shutting down gracefully...")
	cancel()
	wg.Wait()

}
