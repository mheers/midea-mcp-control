package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	midea "github.com/thekondor/midea-porta-split"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// scrape returns the exporter's exposition text, which is the same bytes a
// Prometheus scraper reads. Tests assert against this rather than against the
// exporter's private gauge fields, so a refactor that keeps the output
// identical does not break them.
func scrape(t *testing.T, exporter *MetricsExporter) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	exporter.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", recorder.Code)
	}
	return recorder.Body.String()
}

// sample returns the value of one series in the exposition text. A series that
// is missing from the scrape fails the test rather than reading as zero, so a
// gauge that was never populated cannot pass unnoticed.
func sample(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, series+" ") {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimPrefix(line, series+" "), 64)
		if err != nil {
			t.Fatalf("parse value of %s: %v", series, err)
		}
		return value
	}
	t.Fatalf("series %s is absent from the scrape:\n%s", series, body)
	return 0
}

// failingClient always fails its poll, so the exporter's down-marking can be
// exercised independently of the happy-path fake in service_test.go.
type failingClient struct{ err error }

func (f failingClient) Connect(context.Context) error { return nil }
func (f failingClient) Close() error                  { return nil }
func (f failingClient) Poll(context.Context) (midea.Response, error) {
	return midea.Response{}, f.err
}
func (f failingClient) Update(context.Context, midea.Request) (midea.Response, error) {
	return midea.Response{}, f.err
}

func expectGauge(t *testing.T, collector prometheus.Collector, want float64) {
	t.Helper()
	if got := testutil.ToFloat64(collector); got != want {
		t.Errorf("gauge = %v, want %v", got, want)
	}
}

func TestMetricsExporterPublishesDeviceState(t *testing.T) {
	client := &fakeClient{polls: []midea.Response{{
		Power: true, Mode: midea.ModeCool, TargetTemp: 21.5,
		IndoorTemp: 24, OutdoorTemp: 30, Humidity: 55, FanSpeed: midea.FanLow,
	}}}
	service := testService(t, client)
	exporter := NewMetricsExporter(service, MetricsConfig{})

	exporter.Poll(context.Background())

	expectGauge(t, exporter.up.WithLabelValues("bedroom"), 1)
	expectGauge(t, exporter.power.WithLabelValues("bedroom"), 1)
	expectGauge(t, exporter.targetTemp.WithLabelValues("bedroom"), 21.5)
	expectGauge(t, exporter.indoorTemp.WithLabelValues("bedroom"), 24)
	expectGauge(t, exporter.outdoorTemp.WithLabelValues("bedroom"), 30)
	expectGauge(t, exporter.humidity.WithLabelValues("bedroom"), 55)
	expectGauge(t, exporter.errorState.WithLabelValues("bedroom"), 0)
	expectGauge(t, exporter.errorCode.WithLabelValues("bedroom"), 0)
	expectGauge(t, exporter.mode.WithLabelValues("bedroom", "cool"), 1)
	expectGauge(t, exporter.fanSpeed.WithLabelValues("bedroom", "low"), 1)
	// The happy-path fake reports a lifetime consumption of 6.9 kWh.
	expectGauge(t, exporter.totalEnergy.WithLabelValues("bedroom"), 6.9)
}

func TestMetricsExporterMarksFailedPollsDown(t *testing.T) {
	service := testService(t, failingClient{err: errors.New("no response")})
	exporter := NewMetricsExporter(service, MetricsConfig{})

	exporter.Poll(context.Background())

	expectGauge(t, exporter.up.WithLabelValues("bedroom"), 0)
}

// A mode or fan-speed change must not leave the previous label combination
// behind: exactly one series per device stays at 1.
func TestMetricsExporterDropsStaleInfoSeries(t *testing.T) {
	client := &fakeClient{polls: []midea.Response{
		{Mode: midea.ModeCool, FanSpeed: midea.FanLow},
		{Mode: midea.ModeHeat, FanSpeed: midea.FanHigh},
	}}
	service := testService(t, client)
	exporter := NewMetricsExporter(service, MetricsConfig{})

	exporter.Poll(context.Background())
	if got := testutil.CollectAndCount(exporter.mode); got != 1 {
		t.Fatalf("mode series after first poll = %d, want 1", got)
	}
	exporter.Poll(context.Background())

	expectGauge(t, exporter.mode.WithLabelValues("bedroom", "heat"), 1)
	expectGauge(t, exporter.fanSpeed.WithLabelValues("bedroom", "high"), 1)
	if got := testutil.CollectAndCount(exporter.mode); got != 1 {
		t.Errorf("mode series after mode change = %d, want 1 (stale series leaked)", got)
	}
	if got := testutil.CollectAndCount(exporter.fanSpeed); got != 1 {
		t.Errorf("fan-speed series after change = %d, want 1 (stale series leaked)", got)
	}
}

// noEnergyClient answers status polls but does not implement the optional
// energy interface, standing in for firmware that cannot answer the query.
type noEnergyClient struct{ poll midea.Response }

func (n noEnergyClient) Connect(context.Context) error { return nil }
func (n noEnergyClient) Close() error                  { return nil }
func (n noEnergyClient) Poll(context.Context) (midea.Response, error) {
	return n.poll, nil
}
func (n noEnergyClient) Update(context.Context, midea.Request) (midea.Response, error) {
	return n.poll, nil
}

// A unit that cannot answer the energy query must still publish both energy
// series, reporting 0. Dropping them would leave a hole in the scrape, and a
// fleet-wide sum would under-count without any signal that it had.
func TestMetricsExporterPublishesEnergyForUnsupportedUnit(t *testing.T) {
	service := testService(t, noEnergyClient{poll: midea.Response{Mode: midea.ModeCool}})
	exporter := NewMetricsExporter(service, MetricsConfig{})
	exporter.Poll(context.Background())

	body := scrape(t, exporter)
	if got := sample(t, body, `midea_total_energy_kwh{name="bedroom"}`); got != 0 {
		t.Errorf("total energy for an unsupported unit = %v, want 0", got)
	}
	if got := sample(t, body, `midea_current_run_energy_kwh{name="bedroom"}`); got != 0 {
		t.Errorf("current run energy for an unsupported unit = %v, want 0", got)
	}
	// The unit did answer the status poll, so it is reachable and up.
	if got := sample(t, body, `midea_up{name="bedroom"}`); got != 1 {
		t.Errorf("up = %v, want 1", got)
	}
}

// Every other gauge keeps its last value across an outage, so a stale reading
// is indistinguishable from a fresh one unless the poll time is exposed.
// Alerting on time() - midea_last_poll_timestamp_seconds is what makes an
// unreachable unit visible before its readings quietly go stale.
func TestMetricsExporterExposesLastPollTime(t *testing.T) {
	client := &fakeClient{polls: []midea.Response{{Power: true}}}
	exporter := NewMetricsExporter(testService(t, client), MetricsConfig{})

	before := time.Now().Unix()
	exporter.Poll(context.Background())

	body := scrape(t, exporter)
	got := sample(t, body, `midea_last_poll_timestamp_seconds{name="bedroom"}`)
	if got < float64(before) || got > float64(time.Now().Unix()) {
		t.Errorf("last poll timestamp = %v, want a time within this test run (%v..%v)", got, before, time.Now().Unix())
	}
}

// A value the controller could not name must not leak the controller's Go
// formatting fallback ("fan(0)") into a metric label, where it would become a
// permanent value that dashboards and recording rules have to handle.
func TestMetricsExporterNormalizesUnrecognizedLabels(t *testing.T) {
	client := &fakeClient{polls: []midea.Response{{
		Mode: midea.ModeFan, FanSpeed: midea.FanSpeed(0),
	}}}
	exporter := NewMetricsExporter(testService(t, client), MetricsConfig{})
	exporter.Poll(context.Background())

	body := scrape(t, exporter)
	if got := sample(t, body, `midea_mode{mode="fan",name="bedroom"}`); got != 1 {
		t.Errorf("recognized mode = %v, want 1", got)
	}
	if got := sample(t, body, `midea_fan_speed{name="bedroom",speed="unknown"}`); got != 1 {
		t.Errorf("unrecognized fan speed = %v, want 1 (fell back to a Go-formatted label)", got)
	}
	if strings.Contains(body, `speed="fan(0)"`) {
		t.Errorf("scrape exposes a Go-formatted fan-speed label:\n%s", body)
	}
}

// The protocol has no "sensor absent" sentinel for humidity: the byte is
// masked to 7 bits, so a unit without a humidity sensor reports 127. That
// value is not a percentage and must not reach a scrape, where it would
// corrupt every average computed over the fleet.
func TestMetricsExporterRejectsImpossibleHumidity(t *testing.T) {
	for humidity, want := range map[int]float64{0: 0, 55: 55, 100: 100, 101: 0, 127: 0} {
		client := &fakeClient{polls: []midea.Response{{Humidity: humidity}}}
		exporter := NewMetricsExporter(testService(t, client), MetricsConfig{})
		exporter.Poll(context.Background())

		body := scrape(t, exporter)
		if got := sample(t, body, `midea_humidity_percent{name="bedroom"}`); got != want {
			t.Errorf("humidity %d published as %v, want %v", humidity, got, want)
		}
	}
}

// The default listen-address check guards the metrics listener exactly like
// the MCP listener: loopback unless explicitly allowed.
func TestCheckListenAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9103", "localhost:9103", "[::1]:9103"} {
		if err := CheckListenAddress(addr, false); err != nil {
			t.Errorf("loopback address %q was rejected: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:9103", "192.0.2.10:9103", ":9103"} {
		if err := CheckListenAddress(addr, false); err == nil {
			t.Errorf("non-loopback address %q was accepted without allowRemote", addr)
		}
	}
	if err := CheckListenAddress("0.0.0.0:9103", true); err != nil {
		t.Errorf("explicit non-loopback bind was rejected: %v", err)
	}
}
