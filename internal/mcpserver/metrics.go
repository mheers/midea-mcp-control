package mcpserver

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsConfig configures the background poller behind the Prometheus
// endpoint.
type MetricsConfig struct {
	// Interval is the delay between poll cycles. Zero means one minute.
	Interval time.Duration
	// Timeout is the per-device budget for one status or energy read. Zero
	// means 15 seconds.
	Timeout time.Duration
}

// MetricsExporter polls every configured device in the background and serves
// the latest readings as Prometheus metrics. Last-good values persist across
// failed polls: an unreachable unit keeps reporting its previous readings and
// midea_up drops to 0 instead of every series disappearing from the scrape.
//
// Every read goes through the shared Service controller, so the exporter
// serializes with MCP tool calls per device and no two sessions ever talk to
// one unit at the same time.
type MetricsExporter struct {
	// Service performs the device reads. Required.
	Service *Service
	// Interval and Timeout mirror MetricsConfig.
	Interval time.Duration
	Timeout  time.Duration

	registry *prometheus.Registry

	up          *prometheus.GaugeVec
	power       *prometheus.GaugeVec
	mode        *prometheus.GaugeVec
	fanSpeed    *prometheus.GaugeVec
	targetTemp  *prometheus.GaugeVec
	indoorTemp  *prometheus.GaugeVec
	outdoorTemp *prometheus.GaugeVec
	humidity    *prometheus.GaugeVec
	errorState  *prometheus.GaugeVec
	errorCode   *prometheus.GaugeVec
	totalEnergy *prometheus.GaugeVec
	runEnergy   *prometheus.GaugeVec

	// mu guards the two "last reported" maps that keep the info gauges free
	// of stale label combinations after a mode or fan-speed change.
	mu       sync.Mutex
	lastMode map[string]string
	lastFan  map[string]string
}

// NewMetricsExporter builds an exporter with its private registry.
func NewMetricsExporter(service *Service, config MetricsConfig) *MetricsExporter {
	exporter := &MetricsExporter{
		Service:  service,
		Interval: config.Interval,
		Timeout:  config.Timeout,
		registry: prometheus.NewRegistry(),
		lastMode: map[string]string{},
		lastFan:  map[string]string{},
	}
	if exporter.Interval <= 0 {
		exporter.Interval = time.Minute
	}
	if exporter.Timeout <= 0 {
		exporter.Timeout = 15 * time.Second
	}

	exporter.up = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_up",
		Help: "1 when the last poll of this device succeeded, 0 otherwise.",
	}, []string{"name"})
	exporter.power = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_power_on",
		Help: "1 when the unit is powered on, 0 otherwise.",
	}, []string{"name"})
	exporter.mode = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_mode",
		Help: "Current operating mode; exactly one mode series per device is 1.",
	}, []string{"name", "mode"})
	exporter.fanSpeed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_fan_speed",
		Help: "Current fan speed; exactly one speed series per device is 1.",
	}, []string{"name", "speed"})
	exporter.targetTemp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_target_temperature_celsius",
		Help: "Target temperature in Celsius.",
	}, []string{"name"})
	exporter.indoorTemp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_indoor_temperature_celsius",
		Help: "Indoor temperature in Celsius (the unit's own room sensor).",
	}, []string{"name"})
	exporter.outdoorTemp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_outdoor_temperature_celsius",
		Help: "Outdoor temperature in Celsius as reported by the unit (outdoor-unit side reading).",
	}, []string{"name"})
	exporter.humidity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_humidity_percent",
		Help: "Indoor relative humidity in percent; not every model reports a sensor (0 when absent).",
	}, []string{"name"})
	exporter.errorState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_error",
		Help: "1 when the unit reports an error, 0 otherwise.",
	}, []string{"name"})
	exporter.errorCode = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_error_code",
		Help: "Numeric error code reported by the unit (0 when no error).",
	}, []string{"name"})
	exporter.totalEnergy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_total_energy_kwh",
		Help: "Lifetime consumption in kWh as reported by the unit; monotonic while the unit runs.",
	}, []string{"name"})
	exporter.runEnergy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_current_run_energy_kwh",
		Help: "Consumption attributed to the current run in kWh.",
	}, []string{"name"})

	exporter.registry.MustRegister(
		exporter.up, exporter.power, exporter.mode, exporter.fanSpeed,
		exporter.targetTemp, exporter.indoorTemp, exporter.outdoorTemp,
		exporter.humidity, exporter.errorState, exporter.errorCode,
		exporter.totalEnergy, exporter.runEnergy,
	)
	return exporter
}

// Handler serves the exporter's registry as Prometheus text format.
func (e *MetricsExporter) Handler() http.Handler {
	return promhttp.HandlerFor(e.registry, promhttp.HandlerOpts{})
}

// Run polls on the configured interval until ctx is canceled. The first cycle
// runs immediately so the earliest scrape is already populated.
func (e *MetricsExporter) Run(ctx context.Context) {
	e.Poll(ctx)
	ticker := time.NewTicker(e.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.Poll(ctx)
		}
	}
}

// Poll runs one full cycle over every configured device. It is exported so
// callers (and tests) can trigger a cycle on demand.
func (e *MetricsExporter) Poll(ctx context.Context) {
	devices, err := e.Service.ListDevices()
	if err != nil {
		log.Printf("midea metrics: loading device inventory: %v", err)
		return
	}
	for _, device := range devices {
		if ctx.Err() != nil {
			return
		}
		e.pollDevice(ctx, device.Name)
	}
}

// pollDevice reads one device's state and energy. A status failure marks the
// device down; an energy failure only logs, because the two reads are
// independent and a model without energy support must still report its
// temperatures.
func (e *MetricsExporter) pollDevice(ctx context.Context, name string) {
	statusCtx, cancel := context.WithTimeout(ctx, e.Timeout)
	result, err := e.Service.Status(statusCtx, name)
	cancel()
	if err != nil {
		e.up.WithLabelValues(name).Set(0)
		log.Printf("midea metrics: %s: %v", name, err)
		return
	}
	state := result.State

	e.up.WithLabelValues(name).Set(1)
	e.setBool(e.power, name, state.Power)
	e.setBool(e.errorState, name, state.Error)
	e.errorCode.WithLabelValues(name).Set(float64(state.ErrorCode))
	e.targetTemp.WithLabelValues(name).Set(state.TargetTemperature)
	e.indoorTemp.WithLabelValues(name).Set(state.IndoorTemperature)
	e.outdoorTemp.WithLabelValues(name).Set(state.OutdoorTemperature)
	e.humidity.WithLabelValues(name).Set(float64(state.Humidity))
	e.setInfo(e.mode, e.lastMode, name, state.Mode)
	e.setInfo(e.fanSpeed, e.lastFan, name, state.FanSpeed)

	energyCtx, cancel := context.WithTimeout(ctx, e.Timeout)
	energy, err := e.Service.Energy(energyCtx, name)
	cancel()
	if err != nil {
		log.Printf("midea metrics: %s energy: %v", name, err)
		return
	}
	e.totalEnergy.WithLabelValues(name).Set(energy.Energy.TotalKWh)
	e.runEnergy.WithLabelValues(name).Set(energy.Energy.CurrentRunKWh)
}

func (e *MetricsExporter) setBool(gauge *prometheus.GaugeVec, name string, value bool) {
	if value {
		gauge.WithLabelValues(name).Set(1)
		return
	}
	gauge.WithLabelValues(name).Set(0)
}

// setInfo keeps an info-style gauge to one series per device: the previous
// label combination is deleted when the value changes, so only the current
// mode/fan speed stays at 1.
func (e *MetricsExporter) setInfo(gauge *prometheus.GaugeVec, last map[string]string, name, value string) {
	e.mu.Lock()
	if previous, ok := last[name]; ok && previous != value {
		gauge.DeleteLabelValues(name, previous)
	}
	last[name] = value
	e.mu.Unlock()
	gauge.WithLabelValues(name, value).Set(1)
}
