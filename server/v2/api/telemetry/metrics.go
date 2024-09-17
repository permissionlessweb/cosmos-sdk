package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/go-metrics"
	"github.com/hashicorp/go-metrics/datadog"
	metricsprom "github.com/hashicorp/go-metrics/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// GlobalLabels defines the set of global labels that will be applied to all
// metrics emitted using the telemetry package function wrappers.
var GlobalLabels = []metrics.Label{} //nolint: ignore // false positive

// NewLabel creates a new instance of Label with name and value
func NewLabel(name, value string) metrics.Label {
	return metrics.Label{Name: name, Value: value}
}

// Metrics supported format types.
const (
	FormatDefault    = ""
	FormatPrometheus = "prometheus"
	FormatText       = "text"
	ContentTypeText  = `text/plain; version=` + expfmt.TextVersion + `; charset=utf-8`

	MetricSinkInMem      = "mem"
	MetricSinkStatsd     = "statsd"
	MetricSinkDogsStatsd = "dogstatsd"
)

// DisplayableSink is an interface that defines a method for displaying metrics.
type DisplayableSink interface {
	DisplayMetrics(resp http.ResponseWriter, req *http.Request) (any, error)
}

// Metrics defines a wrapper around application telemetry functionality. It allows
// metrics to be gathered at any point in time. When creating a Metrics object,
// internally, a global metrics is registered with a set of sinks as configured
// by the operator. In addition to the sinks, when a process gets a SIGUSR1, a
// dump of formatted recent metrics will be sent to STDERR.
type Metrics struct {
	sink              metrics.MetricSink
	prometheusEnabled bool
}

// GatherResponse is the response type of registered metrics
type GatherResponse struct {
	Metrics     []byte
	ContentType string
}

// New creates a new instance of Metrics
func New(cfg Config) (_ *Metrics, rerr error) {
	if !cfg.Enabled {
		return nil, nil
	}

	if numGlobalLabels := len(cfg.GlobalLabels); numGlobalLabels > 0 {
		parsedGlobalLabels := make([]metrics.Label, numGlobalLabels)
		for i, gl := range cfg.GlobalLabels {
			parsedGlobalLabels[i] = NewLabel(gl[0], gl[1])
		}
		GlobalLabels = parsedGlobalLabels
	}

	metricsConf := metrics.DefaultConfig(cfg.ServiceName)
	metricsConf.EnableHostname = cfg.EnableHostname
	metricsConf.EnableHostnameLabel = cfg.EnableHostnameLabel

	var (
		sink metrics.MetricSink
		err  error
	)
	switch cfg.MetricsSink {
	case MetricSinkStatsd:
		sink, err = metrics.NewStatsdSink(cfg.StatsdAddr)
	case MetricSinkDogsStatsd:
		sink, err = datadog.NewDogStatsdSink(cfg.StatsdAddr, cfg.DatadogHostname)
	default:
		memSink := metrics.NewInmemSink(10*time.Second, time.Minute)
		sink = memSink
		inMemSig := metrics.DefaultInmemSignal(memSink)
		defer func() {
			if rerr != nil {
				inMemSig.Stop()
			}
		}()
	}

	if err != nil {
		return nil, err
	}

	m := &Metrics{sink: sink}
	fanout := metrics.FanoutSink{sink}

	if cfg.PrometheusRetentionTime > 0 {
		m.prometheusEnabled = true
		prometheusOpts := metricsprom.PrometheusOpts{
			Expiration: time.Duration(cfg.PrometheusRetentionTime) * time.Second,
		}

		promSink, err := metricsprom.NewPrometheusSinkFrom(prometheusOpts)
		if err != nil {
			return nil, err
		}

		fanout = append(fanout, promSink)
	}

	if _, err := metrics.NewGlobal(metricsConf, fanout); err != nil {
		return nil, err
	}

	return m, nil
}

// Gather collects all registered metrics and returns a GatherResponse where the
// metrics are encoded depending on the type. Metrics are either encoded via
// Prometheus or JSON if in-memory.
func (m *Metrics) Gather(format string) (GatherResponse, error) {
	switch format {
	case FormatPrometheus:
		return m.gatherPrometheus()

	case FormatText:
		return m.gatherGeneric()

	case FormatDefault:
		return m.gatherGeneric()

	default:
		return GatherResponse{}, fmt.Errorf("unsupported metrics format: %s", format)
	}
}

// gatherPrometheus collects Prometheus metrics and returns a GatherResponse.
// If Prometheus metrics are not enabled, it returns an error.
func (m *Metrics) gatherPrometheus() (GatherResponse, error) {
	if !m.prometheusEnabled {
		return GatherResponse{}, errors.New("prometheus metrics are not enabled")
	}

	metricsFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return GatherResponse{}, fmt.Errorf("failed to gather prometheus metrics: %w", err)
	}

	buf := &bytes.Buffer{}
	defer buf.Reset()

	e := expfmt.NewEncoder(buf, expfmt.NewFormat(expfmt.TypeTextPlain))

	for _, mf := range metricsFamilies {
		if err := e.Encode(mf); err != nil {
			return GatherResponse{}, fmt.Errorf("failed to encode prometheus metrics: %w", err)
		}
	}

	return GatherResponse{ContentType: ContentTypeText, Metrics: buf.Bytes()}, nil
}

// gatherGeneric collects generic metrics and returns a GatherResponse.
func (m *Metrics) gatherGeneric() (GatherResponse, error) {
	gm, ok := m.sink.(DisplayableSink)
	if !ok {
		return GatherResponse{}, errors.New("non in-memory metrics sink does not support generic format")
	}

	summary, err := gm.DisplayMetrics(nil, nil)
	if err != nil {
		return GatherResponse{}, fmt.Errorf("failed to gather in-memory metrics: %w", err)
	}

	content, err := json.Marshal(summary)
	if err != nil {
		return GatherResponse{}, fmt.Errorf("failed to encode in-memory metrics: %w", err)
	}

	return GatherResponse{ContentType: "application/json", Metrics: content}, nil
}
