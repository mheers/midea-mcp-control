package mcpserver

import (
	"context"
	"errors"
	"testing"

	midea "github.com/thekondor/midea-porta-split"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

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
