package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
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
	maxPostTimeout := flag.Duration("maxPostTimeout", time.Second*5, "post to alertmanager timeout")
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

	f, err := os.Open(*configPath)
	if err != nil {
		log.Fatalf("error opening config file: %s", err)
	}
	defer f.Close()

	conf := Config{}

	decoder := yaml.NewDecoder(f, yaml.DisallowUnknownField(), yaml.UseJSONUnmarshaler())
	err = decoder.Decode(&conf)
	if err != nil && !errors.Is(err, io.EOF) {
		log.Fatalf("error parsing yaml: %s", err)
	}

	amClient := client.NewHTTPClientWithConfig(
		strfmt.Default,
		client.DefaultTransportConfig().
			WithHost(*amHost).
			WithBasePath(*amPath),
	)

	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	wg := &sync.WaitGroup{}

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
					// create a list of alerts.
					alerts := make([]*models.PostableAlert, 0, 32)
					func() {

						ctx, cancel := context.WithTimeout(ctx, r.EvaluateFreq/2)

						rows, err := db.QueryContext(ctx, r.Query)
						cancel()
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

							alerts = append(alerts, &models.PostableAlert{
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

					if len(alerts) == 0 {
						slog.Debug("alerts found",
							"name", r.Name,
							"count", len(alerts),
						)
						return
					}

					slog.Info("no alerts found",
						"name", r.Name,
					)

					// Post alerts to alertmanager
					func() {
						postCtx, cancel := context.WithTimeout(ctx, *maxPostTimeout)
						defer cancel()

						params := alert.NewPostAlertsParams().
							WithContext(postCtx).
							WithAlerts(alerts)

						resp, err := amClient.Alert.PostAlerts(params)
						if err != nil {
							slog.Error("failed to send alerts",
								"name", r.Name,
								"count", len(alerts))
							return
						}

						if resp.IsSuccess() {
							slog.Info("alerts sent",
								"name", r.Name,
								"count", len(alerts))
						} else {
							slog.Error("failed to send alerts",
								"name", r.Name,
								"count", len(alerts),
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
