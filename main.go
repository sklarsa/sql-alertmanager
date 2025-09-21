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

	"github.com/goccy/go-yaml"

	"github.com/go-openapi/strfmt"
	"github.com/prometheus/alertmanager/api/v2/client"
	"github.com/prometheus/alertmanager/api/v2/client/alert"
	"github.com/prometheus/alertmanager/api/v2/models"
)

type AlertRule struct {
	Query        string          `json:"query"`
	EvaluateFreq time.Duration   `json:"evaluateFreq"`
	Labels       models.LabelSet `json:"labelSet"`
	Annotations  models.LabelSet `json:"annotations"`
}

func (r AlertRule) baseArgs() []any {
	baseArgs := make([]any, 0, len(r.Labels)*2)
	for k, v := range r.Labels {
		baseArgs = append(baseArgs, k, v)
	}
	return baseArgs
}

func (r AlertRule) errorArgs(err error) []any {
	return append(r.baseArgs(), "error", err.Error())
}

func (r AlertRule) queryArgs() []any {
	return append(r.baseArgs(), "query", r.Query)
}

func main() {
	connStr := os.Args[1]
	driver := flag.String("driver", "pgx", "sql driver to use")
	amHost := flag.String("alertManagerHost", "localhost", "hostname for alert manager")
	amPath := flag.String("alertManagerPath", "/api/v2/", "alert manager v2 api path")
	configPath := flag.String("config", "./config.yaml", "path to config")
	maxPostTimeout := flag.Duration("maxPostTimeout", time.Second*5, "post to alertmanager timeout")
	flag.Parse()

	f, err := os.Open(*configPath)
	if err != nil {
		log.Fatalf("error opening config file: %s", err)
	}

	rules := []AlertRule{}

	decoder := yaml.NewDecoder(f, yaml.DisallowUnknownField(), yaml.UseJSONUnmarshaler())
	err = decoder.Decode(&rules)
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

	db, err := sql.Open(*driver, connStr)
	if err != nil {
		log.Fatalf(
			"error opening connection with %q driver: %s\n",
			*driver,
			err,
		)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("DB connection test failed: %s", err)
	}

	for _, r := range rules {
		wg.Add(1)
		go func(r AlertRule) {
			defer wg.Done()
			slog.Info("registered rule", r.baseArgs()...)

			ticker := time.NewTicker(r.EvaluateFreq)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					slog.Info("rule stopped", r.baseArgs()...)
					return
				case <-ticker.C:

					slog.Debug("executing query",
						r.queryArgs()...,
					)

					func() {
						rows, err := db.Query(r.Query)
						if err != nil {
							slog.Error("query failed", r.errorArgs(err)...)
							return
						}
						defer rows.Close()

						alerts := []*models.PostableAlert{}
						for rows.Next() {
							alerts = append(alerts, &models.PostableAlert{
								Annotations: r.Annotations,
								StartsAt:    strfmt.DateTime(time.Now()),
								Alert: models.Alert{
									Labels: r.Labels,
								},
							})
						}
						if err := rows.Err(); err != nil {
							slog.Error("row iteration error", r.errorArgs(err)...)
						}

						if len(alerts) == 0 {
							return
						}

						params := alert.NewPostAlertsParams().
							WithContext(ctx).
							WithAlerts(alerts).
							WithTimeout(*maxPostTimeout)
						resp, err := amClient.Alert.PostAlerts(params)
						if err != nil {
							slog.Error("failed to send alert", r.errorArgs(err)...)
							return
						}

						if resp.IsSuccess() {
							slog.Info("alert sent", r.baseArgs()...)
						} else {
							args := r.baseArgs()
							args = append(args,
								"code", resp.Code(),
								"error", resp.Error(),
							)
							slog.Error("failed to send alert", args...)
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
