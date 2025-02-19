// Copyright 2018 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collector

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cprobe/cprobe/lib/logger"
	"github.com/cprobe/cprobe/types"
	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
)

// Metric name parts.
const (
	// Subsystem(s).
	exporter = "exporter"
)

// SQL queries and parameters.
const (
	versionQuery = `SELECT @@version`

	// System variable params formatting.
	// See: https://github.com/go-sql-driver/mysql#system-variables
	sessionSettingsParam = `log_slow_filter=%27tmp_table_on_disk,filesort_on_disk%27`
	timeoutParam         = `lock_wait_timeout=%d`
)

var (
	versionRE = regexp.MustCompile(`^\d+\.\d+`)
)

// Tunable flags.
// var (
// 	slowLogFilter = kingpin.Flag(
// 		"exporter.log_slow_filter",
// 		"Add a log_slow_filter to avoid slow query logging of scrapes. NOTE: Not supported by Oracle MySQL.",
// 	).Default("false").Bool()
// )

// metric definition
var (
	mysqlScrapeCollectorSuccess = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, exporter, "collector_success"),
		"mysqld_exporter: Whether a collector succeeded.",
		[]string{"collector"},
		nil,
	)
	mysqlScrapeDurationSeconds = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, exporter, "collector_duration_seconds"),
		"Collector time duration.",
		[]string{"collector"}, nil,
	)
)

// Verify if Exporter implements prometheus.Collector
// var _ prometheus.Collector = (*Exporter)(nil)

// Exporter collects MySQL metrics. It implements prometheus.Collector.
type Exporter struct {
	ctx      context.Context
	dsn      string
	scrapers []Scraper
	ss       *types.Samples
	queries  []CustomQuery
}

// New returns a new MySQL exporter for the provided DSN.
func New(ctx context.Context, dsn string, scrapers []Scraper, ss *types.Samples, queries []CustomQuery, lockWaitTimeout int, logSlowFilter bool) *Exporter {
	// Setup extra params for the DSN, default to having a lock timeout.
	dsnParams := []string{fmt.Sprintf(timeoutParam, lockWaitTimeout)}

	if logSlowFilter {
		dsnParams = append(dsnParams, sessionSettingsParam)
	}

	if strings.Contains(dsn, "?") {
		dsn = dsn + "&"
	} else {
		dsn = dsn + "?"
	}
	dsn += strings.Join(dsnParams, "&")

	return &Exporter{
		ctx:      ctx,
		dsn:      dsn,
		scrapers: scrapers,
		ss:       ss,
		queries:  queries,
	}
}

// Describe implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- mysqlScrapeDurationSeconds
	ch <- mysqlScrapeCollectorSuccess
}

// Collect implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) error {
	return e.scrape(e.ctx, ch)
}

// scrape collects metrics from the target, returns an up metric value.
func (e *Exporter) scrape(ctx context.Context, ch chan<- prometheus.Metric) error {
	scrapeTime := time.Now()
	db, err := sql.Open("mysql", e.dsn)
	if err != nil {
		return fmt.Errorf("cannot opening connection to database: %s, error: %s", e.dsn, err)
	}

	defer db.Close()

	// By design exporter should use maximum one connection per request.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// Set max lifetime for a connection.
	db.SetConnMaxLifetime(1 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("cannot ping mysql %s, error: %s", e.getTargetFromDsn(), err)
	}

	ch <- prometheus.MustNewConstMetric(mysqlScrapeDurationSeconds, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "connection")

	version := getMySQLVersion(db)
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, scraper := range e.scrapers {
		if version < scraper.Version() {
			continue
		}

		wg.Add(1)
		go func(scraper Scraper) {
			defer wg.Done()
			label := "collect." + scraper.Name()
			scrapeTime := time.Now()
			collectorSuccess := 1.0
			if err := scraper.Scrape(ctx, db, ch); err != nil {
				logger.Errorf("cannot scrape: %s, target: %s, error: %s", scraper.Name(), e.getTargetFromDsn(), err)
				// level.Error(e.logger).Log("msg", "Error from scraper", "scraper", scraper.Name(), "target", e.getTargetFromDsn(), "err", err)
				collectorSuccess = 0.0
			}
			ch <- prometheus.MustNewConstMetric(mysqlScrapeCollectorSuccess, prometheus.GaugeValue, collectorSuccess, label)
			ch <- prometheus.MustNewConstMetric(mysqlScrapeDurationSeconds, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), label)
		}(scraper)
	}

	// 添加自定义采集的逻辑
	e.collectCustomQueries(ctx, db, e.ss, e.queries)

	return nil
}

func (e *Exporter) getTargetFromDsn() string {
	// Get target from DSN.
	dsnConfig, err := mysql.ParseDSN(e.dsn)
	if err != nil {
		logger.Errorf("Error parsing DSN: %s", err)
		// level.Error(e.logger).Log("msg", "Error parsing DSN", "err", err)
		return ""
	}
	return dsnConfig.Addr
}

func getMySQLVersion(db *sql.DB) float64 {
	var versionStr string
	var versionNum float64
	if err := db.QueryRow(versionQuery).Scan(&versionStr); err == nil {
		versionNum, _ = strconv.ParseFloat(versionRE.FindString(versionStr), 64)
	}
	// else {
	// 	level.Debug(logger).Log("msg", "Error querying version", "err", err)
	// }
	// If we can't match/parse the version, set it some big value that matches all versions.
	if versionNum == 0 {
		// level.Debug(logger).Log("msg", "Error parsing version string", "version", versionStr)
		versionNum = 999
	}
	return versionNum
}
