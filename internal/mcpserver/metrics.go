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
	lastPoll    *prometheus.GaugeVec

	// mu guards the per-device bookkeeping: the "last reported" values that
	// keep the info gauges free of stale label combinations, and the set of
	// devices whose series have already been initialized.
	mu       sync.Mutex
	lastMode map[string]string
	lastFan  map[string]string
	seen     map[string]bool
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
		seen:     map[string]bool{},
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
		Help: "Current operating mode; exactly one mode series per device is 1. " +
			"A mode the controller cannot name is reported as unknown.",
	}, []string{"name", "mode"})
	exporter.fanSpeed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_fan_speed",
		Help: "Current fan speed; exactly one speed series per device is 1. " +
			"A speed the controller cannot name is reported as unknown.",
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
		Help: "Indoor relative humidity in percent, 0 on models without a sensor. " +
			"An impossible reading above 100 is reported as 0 rather than published.",
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
		Help: "Lifetime consumption in kWh as reported by the unit. A gauge rather than a counter: " +
			"it is a device register that resets on a power or firmware reset, so it can decrease. " +
			"Average kW over a one-hour window: clamp_min(delta(midea_total_energy_kwh[1h]), 0); " +
			"multiply by 1000 for watts.",
	}, []string{"name"})
	exporter.runEnergy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_current_run_energy_kwh",
		Help: "Consumption attributed to the current run in kWh. Resets to 0 when the unit is " +
			"powered off, so it is not a counter.",
	}, []string{"name"})
	exporter.lastPoll = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "midea_last_poll_timestamp_seconds",
		Help: "Unix time of the last successful poll. Every other gauge keeps its last value " +
			"across an outage, so alert on time() minus this value to notice a unit going stale.",
	}, []string{"name"})

	exporter.registry.MustRegister(
		exporter.up, exporter.power, exporter.mode, exporter.fanSpeed,
		exporter.targetTemp, exporter.indoorTemp, exporter.outdoorTemp,
		exporter.humidity, exporter.errorState, exporter.errorCode,
		exporter.totalEnergy, exporter.runEnergy, exporter.lastPoll,
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

	// Serialize on the same mutex the mode and fan-speed bookkeeping uses, so
	// a scrape can never observe half of a device's gauges from one poll and
	// half from another.
	e.mu.Lock()
	defer e.mu.Unlock()

	// Publish a zero for every gauge before writing the real values, so a unit
	// whose energy query fails still leaves a complete set of series in the
	// scrape. This must run first: the zero defaults would otherwise overwrite
	// the values set below.
	e.ensureSeriesLocked(name)

	e.up.WithLabelValues(name).Set(1)
	e.lastPoll.WithLabelValues(name).Set(float64(time.Now().Unix()))
	e.setBool(e.power, name, state.Power)
	e.setBool(e.errorState, name, state.Error)
	e.errorCode.WithLabelValues(name).Set(float64(state.ErrorCode))
	e.targetTemp.WithLabelValues(name).Set(state.TargetTemperature)
	e.indoorTemp.WithLabelValues(name).Set(state.IndoorTemperature)
	e.outdoorTemp.WithLabelValues(name).Set(state.OutdoorTemperature)
	e.humidity.WithLabelValues(name).Set(validHumidity(state.Humidity))
	e.setInfoLocked(e.mode, e.lastMode, name, knownMode(state.Mode))
	e.setInfoLocked(e.fanSpeed, e.lastFan, name, knownFan(state.FanSpeed))

	energyCtx, cancel := context.WithTimeout(ctx, e.Timeout)
	energy, err := e.Service.Energy(energyCtx, name)
	cancel()
	if err != nil {
		// The series are initialized to 0 by ensureSeries, so a unit that
		// cannot answer stays in the scrape rather than dropping out of a
		// fleet-wide sum.
		log.Printf("midea metrics: %s energy: %v", name, err)
		return
	}
	e.totalEnergy.WithLabelValues(name).Set(energy.Energy.TotalKWh)
	e.runEnergy.WithLabelValues(name).Set(energy.Energy.CurrentRunKWh)
}

// validHumidity reports a reading that is physically possible as a percentage.
//
// The protocol byte is masked to 7 bits and carries no "sensor absent"
// sentinel, so a unit with no humidity sensor reports 127. Publishing that
// would corrupt every average over the fleet, so an out-of-range reading is
// reported as 0, the same value a model that reports nothing produces.
func validHumidity(humidity int) float64 {
	if humidity < 0 || humidity > 100 {
		return 0
	}
	return float64(humidity)
}

// ensureSeriesLocked touches a device's series once, so a gauge that a later
// read fails to update is still present in the scrape. Callers must hold mu.
func (e *MetricsExporter) ensureSeriesLocked(name string) {
	if e.seen[name] {
		return
	}
	e.seen[name] = true
	e.power.WithLabelValues(name).Set(0)
	e.errorState.WithLabelValues(name).Set(0)
	e.errorCode.WithLabelValues(name).Set(0)
	e.targetTemp.WithLabelValues(name).Set(0)
	e.indoorTemp.WithLabelValues(name).Set(0)
	e.outdoorTemp.WithLabelValues(name).Set(0)
	e.humidity.WithLabelValues(name).Set(0)
	e.totalEnergy.WithLabelValues(name).Set(0)
	e.runEnergy.WithLabelValues(name).Set(0)
}

func (e *MetricsExporter) setBool(gauge *prometheus.GaugeVec, name string, value bool) {
	if value {
		gauge.WithLabelValues(name).Set(1)
		return
	}
	gauge.WithLabelValues(name).Set(0)
}

// knownMode and knownFan keep a value the controller did not recognize out of
// the metric labels. The controller falls back to a Go formatting helper
// ("mode(7)", "fan(0)") when it cannot name a reported value; that string
// would otherwise become a permanent label in the scrape, so an unrecognized
// value collapses to "unknown".
func knownMode(mode string) string {
	switch mode {
	case "auto", "cool", "dry", "heat", "fan", "smart_dry":
		return mode
	}
	return "unknown"
}

func knownFan(speed string) string {
	switch speed {
	case "auto", "silent", "low", "medium", "high", "full":
		return speed
	}
	return "unknown"
}

// setInfoLocked keeps an info-style gauge to one series per device: the
// previous label combination is deleted when the value changes, so only the
// current mode/fan speed stays at 1. Callers must hold mu.
func (e *MetricsExporter) setInfoLocked(gauge *prometheus.GaugeVec, last map[string]string, name, value string) {
	if previous, ok := last[name]; ok && previous != value {
		gauge.DeleteLabelValues(name, previous)
	}
	last[name] = value
	gauge.WithLabelValues(name, value).Set(1)
}
