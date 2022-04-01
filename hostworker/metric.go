package hostworker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/cretz/monitoral/monitoralpb"
	"github.com/cretz/monitoral/systemworker"
	"go.temporal.io/sdk/activity"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const metricTable = "metrics"

type metricManager struct {
	host *host

	configSeq   uint64
	metrics     map[string]*metric
	metricsLock sync.Mutex // Governs all fields above

	newMetrics chan map[string]*metric

	runCancel     context.CancelFunc
	runCancelLock sync.Mutex
}

func newMetricManager(host *host) *metricManager {
	return &metricManager{
		host:    host,
		metrics: map[string]*metric{},
		// Important to only have a buffer of 1 here w/ how it's used
		newMetrics: make(chan map[string]*metric, 1),
	}
}

func (m *metricManager) updateConfigs(configSeq uint64, configs []*monitoralpb.HostConfig) error {
	// Lock the metrics for the life of the update
	m.metricsLock.Lock()
	defer m.metricsLock.Unlock()

	// Make sure config seq is newer
	if m.configSeq >= configSeq {
		return nil
	}
	m.configSeq = configSeq

	// Also lock the run cancel for the life of the update so we can check if it's
	// running and we should start new metrics
	m.runCancelLock.Lock()
	defer m.runCancelLock.Unlock()

	// Go over every config, updating metrics if there or creating as needed
	errs := []string{}
	seenMetrics := map[string]bool{}
	newMetrics := map[string]*metric{}
	for _, hostConfig := range configs {
		for _, metricConfig := range hostConfig.Metrics {
			seenMetrics[metricConfig.Name] = true
			// If it already exists, just update config. Otherwise create and
			// set as new. This means if a config comes in twice for the same metric,
			// the latest config wins.
			var err error
			if metric := m.metrics[metricConfig.Name]; metric != nil {
				err = metric.updateConfig(configSeq, metricConfig)
			} else if metric, err = newMetric(m.host, configSeq, metricConfig); err == nil {
				newMetrics[metricConfig.Name] = metric
			}
			if err != nil {
				errs = append(errs, fmt.Sprintf("failed updating config for metric %q: %v", metricConfig.Name, err))
			}
		}
	}

	// Stop and delete any unseen metrics
	for name, metric := range m.metrics {
		if !seenMetrics[name] {
			delete(m.metrics, name)
			// Can just stop here, the error will be nil from run() if running
			metric.stop()
		}
	}

	// Drain the new new metrics in channel into our map and resend
	select {
	case otherNewMetrics := <-m.newMetrics:
		for k, v := range otherNewMetrics {
			newMetrics[k] = v
		}
	default:
	}
	select {
	case m.newMetrics <- newMetrics:
	default:
		// Should never happen
	}

	// Return errors
	// TODO(cretz): Include in more advanced error details w/ strong types
	if len(errs) > 0 {
		return fmt.Errorf("errors during config update: %v", strings.Join(errs, ", "))
	}
	return nil
}

// Does not return error on context close. Context is an activity context.
func (m *metricManager) run(ctx context.Context) error {
	// Make sure not running then set cancel callback
	m.runCancelLock.Lock()
	exists := m.runCancel != nil
	if !exists {
		ctx, m.runCancel = context.WithCancel(ctx)
	}
	m.runCancelLock.Unlock()
	if exists {
		return fmt.Errorf("already running")
	}
	defer m.stop()

	// Start all of the metrics in the background
	runMetric := func(name string, metric *metric) {
		if err := metric.run(ctx); err != nil && ctx.Err() == nil {
			// Metric failed, so we want to log and remove it from the metric set
			m.metricsLock.Lock()
			delete(m.metrics, name)
			m.metricsLock.Unlock()
			// TODO(cretz): Send system notification instead?
			activity.GetLogger(ctx).Warn("Metric failed, will not run again", "Metric", name, "Error", err)
		}
	}
	m.metricsLock.Lock()
	for name, metric := range m.metrics {
		go runMetric(name, metric)
	}
	// Also drain the channel
	select {
	case <-m.newMetrics:
	default:
	}
	m.metricsLock.Unlock()

	// Run until cancel or new metrics or notification
	for {
		select {
		case <-ctx.Done():
			return nil
		case newMetrics := <-m.newMetrics:
			// Start each new metric in the background
			for name, metric := range newMetrics {
				go runMetric(name, metric)
			}
		}
	}
}

func (m *metricManager) stop() {
	m.runCancelLock.Lock()
	if m.runCancel != nil {
		m.runCancel()
	}
	m.runCancel = nil
	m.runCancelLock.Unlock()
}

type metric struct {
	host *host

	runCancel     context.CancelFunc
	runCancelLock sync.Mutex

	lastConfig     *metricConfig
	lastConfigLock sync.Mutex
	configUpdated  chan *metricConfig
}

func newMetric(
	host *host,
	configSeq uint64,
	config *monitoralpb.HostConfig_Metric,
) (*metric, error) {
	m := &metric{
		host:          host,
		configUpdated: make(chan *metricConfig, 1),
	}
	if err := m.updateConfig(configSeq, config); err != nil {
		return nil, err
	}
	return m, nil
}

// Does not return error on context close
func (m *metric) run(ctx context.Context) error {
	// Make sure not running then set cancel callback
	m.runCancelLock.Lock()
	exists := m.runCancel != nil
	if !exists {
		ctx, m.runCancel = context.WithCancel(ctx)
	}
	m.runCancelLock.Unlock()
	if exists {
		return fmt.Errorf("already running")
	}
	defer m.stop()

	// Create ticker
	m.lastConfigLock.Lock()
	config := m.lastConfig
	// Drain config updated
	select {
	case <-m.configUpdated:
	default:
	}
	m.lastConfigLock.Unlock()
	ticker := time.NewTicker(config.ExecFreq.AsDuration())
	defer ticker.Stop()

	// Run until stopped
	// TODO(cretz): How best to stop it from alerting when complete?
	currentlyAlerting := false
	for {
		// Wait for tick
		select {
		case <-ctx.Done():
			return nil
		case config = <-m.configUpdated:
			// Reset ticker
			ticker.Reset(config.ExecFreq.AsDuration())
		case <-ticker.C:
			// Collect metric
			alerting, err := m.execMetric(ctx, config)
			if err != nil {
				return err
			}
			// If alerting status changed, send notification
			if alerting != currentlyAlerting {
				currentlyAlerting = alerting
				change := &monitoralpb.Notification_AlertChange{Description: config.Description}
				notification := &monitoralpb.Notification{
					Host:      m.host.hostname,
					Name:      config.Name,
					StartedOn: timestamppb.Now(),
				}
				if currentlyAlerting {
					notification.Detail = &monitoralpb.Notification_AlertStarted{AlertStarted: change}
				} else {
					notification.Detail = &monitoralpb.Notification_AlertStopped{AlertStopped: change}
				}
				err := m.host.client.SystemUpdateNotifications(
					ctx,
					systemworker.SystemWorkflowID,
					"",
					&monitoralpb.SystemUpdateNotificationsRequest{Notifications: []*monitoralpb.Notification{notification}},
				)
				if err != nil {
					return fmt.Errorf("failed sending alert notification: %w", err)
				}
			}
		}
	}
}

func (m *metric) stop() {
	m.runCancelLock.Lock()
	if m.runCancel != nil {
		m.runCancel()
	}
	m.runCancel = nil
	m.runCancelLock.Unlock()
}

func (m *metric) updateConfig(configSeq uint64, config *monitoralpb.HostConfig_Metric) error {
	// TODO(cretz): Validate config

	// Lock for the rest of the call
	m.lastConfigLock.Lock()
	defer m.lastConfigLock.Unlock()

	// If not newer or not changed, do nothing
	if configSeq <= m.lastConfig.seq || proto.Equal(config, m.lastConfig.HostConfig_Metric) {
		return nil
	}

	// Set last config
	newConf, err := newMetricConfig(config, configSeq)
	if err != nil {
		return fmt.Errorf("invalid metric config: %w", err)
	}
	m.lastConfig = newConf

	// Continually attempt to drain-and-set the config until we can
	for {
		select {
		case m.configUpdated <- newConf:
			return nil
		case <-m.configUpdated:
		}
	}
}

func (m *metric) execMetric(ctx context.Context, config *metricConfig) (alerting bool, err error) {
	query, err := config.buildQuery()
	if err != nil {
		return false, err
	}

	// Exec all queries
	db := m.host.dbPool.Get(ctx)
	defer m.host.dbPool.Put(db)
	var lastStmtHadRow bool
	for {
		query = strings.TrimSpace(query)
		if query == "" {
			break
		}
		stmt, trailingBytes, err := db.PrepareTransient(query)
		if err != nil {
			return false, fmt.Errorf("query creation failed: %w", err)
		}
		lastStmtHadRow, err = stmt.Step()
		stmt.Finalize()
		if err != nil {
			return false, fmt.Errorf("query execution failed: %w", err)
		}
		query = query[len(query)-trailingBytes:]
	}
	// If it has a row it's alerting
	return lastStmtHadRow, nil
}

type metricConfig struct {
	*monitoralpb.HostConfig_Metric
	seq          uint64
	template     *template.Template
	templateData *metricTemplateData
}

func newMetricConfig(config *monitoralpb.HostConfig_Metric, seq uint64) (*metricConfig, error) {
	if config.GetExec().GetSqlQueryTemplate() == "" {
		return nil, fmt.Errorf("missing SQL query template")
	} else if config.ExecFreq == nil {
		return nil, fmt.Errorf("missing exec frequency")
	}
	ret := &metricConfig{
		HostConfig_Metric: config,
		seq:               seq,
		templateData: &metricTemplateData{
			MetricName:  config.Name,
			MetricTable: metricTable,
			Psutil:      psutilData,
		},
	}
	var err error
	ret.template, err = template.New("query").
		Funcs(template.FuncMap{
			// Only concerned about single quotes atm
			"sqlstring": func(v string) string { return strings.ReplaceAll(v, "'", "''") },
		}).
		Parse(config.GetExec().GetSqlQueryTemplate())
	if err != nil {
		return nil, fmt.Errorf("invalid query template: %w", err)
	}
	return ret, nil
}

func (m *metricConfig) buildQuery() (string, error) {
	var bld strings.Builder
	if err := m.template.Execute(&bld, m.templateData); err != nil {
		return "", fmt.Errorf("building query failed: %w", err)
	}
	return bld.String(), nil
}

type metricTemplateData struct {
	MetricName  string
	MetricTable string
	Psutil      map[string]map[string]interface{}
}
