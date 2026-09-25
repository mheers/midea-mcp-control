package controller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	midea "github.com/thekondor/midea-porta-split"

	"midea-control/internal/config"
)

func testDeviceConfig() config.Device {
	return config.Device{ID: "100000000000001", Name: "bedroom", IP: "192.0.2.1", Port: 6444}
}

func TestPollReadsAndDecodesState(t *testing.T) {
	client := &fakeClient{polls: []midea.Response{{
		Power: true, Mode: midea.ModeCool, TargetTemp: 21.5,
		IndoorTemp: 24, OutdoorTemp: 30, FanSpeed: midea.FanLow,
		SwingV: true, Eco: true,
	}}}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}

	state, err := controller.Poll(context.Background(), testDeviceConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Power || state.Mode != "cool" || state.TargetTemperature != 21.5 {
		t.Errorf("state = %+v", state)
	}
	if state.IndoorTemperature != 24 || state.OutdoorTemperature != 30 {
		t.Errorf("temperatures = %v/%v", state.IndoorTemperature, state.OutdoorTemperature)
	}
	if state.FanSpeed != "low" || !state.SwingVertical || !state.Eco {
		t.Errorf("state = %+v", state)
	}
	if client.connects != 1 || client.closes != 1 {
		t.Errorf("connects/closes = %d/%d, want 1/1", client.connects, client.closes)
	}
}

func TestPollClosesTheClientOnFailure(t *testing.T) {
	client := &fakeClient{polls: []midea.Response{{Power: false}}, pollErr: errors.New("no response")}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}
	if _, err := controller.Poll(context.Background(), testDeviceConfig()); err == nil {
		t.Fatal("Poll succeeded despite a read failure")
	}
	if client.closes != 1 {
		t.Errorf("client was not closed on failure: %d", client.closes)
	}
}

func TestPollPropagatesConnectFailure(t *testing.T) {
	client := &fakeClient{connectErr: errors.New("refused")}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}
	if _, err := controller.Poll(context.Background(), testDeviceConfig()); err == nil {
		t.Fatal("Poll ignored a connect failure")
	}
	if client.pollIndex != 0 {
		t.Error("a read was attempted after the connection failed")
	}
}

func TestPollRejectsNilController(t *testing.T) {
	var controller *Controller
	if _, err := controller.Poll(context.Background(), testDeviceConfig()); err == nil {
		t.Fatal("a nil controller accepted a read")
	}
}

func TestPollSerializesConcurrentReadsForOneDevice(t *testing.T) {
	// Two overlapping reads to the same physical unit can interleave frames on
	// one socket, so the controller must serialise them. Different devices
	// stay independent.
	var concurrent atomic.Int32
	var peak atomic.Int32
	client := &countingClient{
		fakeClient: fakeClient{polls: []midea.Response{{Power: false}, {Power: false}}},
		onPoll: func() {
			now := concurrent.Add(1)
			for {
				observed := peak.Load()
				if now <= observed || peak.CompareAndSwap(observed, now) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			concurrent.Add(-1)
		},
	}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}

	var wait sync.WaitGroup
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _ = controller.Poll(context.Background(), testDeviceConfig())
		}()
	}
	wait.Wait()
	if peak.Load() > 1 {
		t.Fatalf("%d reads were in flight at once for one device", peak.Load())
	}
}

func TestLockIsPerDevice(t *testing.T) {
	controller := New()
	first := config.Device{ID: "1", Name: "a", IP: "192.0.2.1", Port: 6444}
	second := config.Device{ID: "2", Name: "b", IP: "192.0.2.2", Port: 6444}

	// Hold the first device's lock.
	held := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		unlock := controller.lockDevice(first.ID)
		close(held)
		<-acquired
		unlock()
	}()
	<-held

	// A different id must be available immediately.
	otherDone := make(chan struct{})
	go func() {
		controller.lockDevice(second.ID)()
		close(otherDone)
	}()
	select {
	case <-otherDone:
	case <-time.After(time.Second):
		t.Fatal("a lock for a different device blocked")
	}

	// The same id must not be granted a second time while it is held.
	blocked := make(chan struct{})
	go func() {
		controller.lockDevice(first.ID)()
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("the same device lock was granted twice concurrently")
	case <-time.After(50 * time.Millisecond):
	}

	close(acquired)
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("the lock was never released")
	}
	_ = first
}

func TestCapabilitiesReportsFeatureFlags(t *testing.T) {
	client := &extendedClient{caps: midea.Capabilities{
		CustomFanSpeed: true, HumidityControl: true, SwingAngle: true,
		MinCoolTemp: 18, MaxCoolTemp: 30, MinHeatTemp: 17, MaxHeatTemp: 28,
	}}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}

	capabilities, err := controller.Capabilities(context.Background(), testDeviceConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.Reported || !capabilities.CustomFanSpeed || !capabilities.HumidityControl {
		t.Errorf("capabilities = %+v", capabilities)
	}
	if capabilities.MinCoolTemp != 18 || capabilities.MaxHeatTemp != 28 {
		t.Errorf("temperature limits = %+v", capabilities)
	}
}

func TestCapabilitiesFailsWhenUnsupported(t *testing.T) {
	// A client that cannot answer capability queries must say so rather than
	// silently reporting an empty feature set as authoritative.
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) {
		return &plainClient{}, nil
	}}
	if _, err := controller.Capabilities(context.Background(), testDeviceConfig()); err == nil {
		t.Fatal("Capabilities reported success for a client that cannot answer")
	}
}

func TestCapabilitiesPropagatesQueryFailure(t *testing.T) {
	client := &extendedClient{capsErr: errors.New("busy")}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}
	if _, err := controller.Capabilities(context.Background(), testDeviceConfig()); err == nil {
		t.Fatal("Capabilities ignored a query failure")
	}
}

func TestEnergyReportsMeasurement(t *testing.T) {
	client := &extendedClient{energy: midea.Energy{
		TotalKWh: 6.9, CurrentRunKWh: 0.2, RealtimeKW: 0.85,
	}}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}

	energy, err := controller.Energy(context.Background(), testDeviceConfig())
	if err != nil {
		t.Fatal(err)
	}
	if energy.TotalKWh != 6.9 || energy.CurrentRunKWh != 0.2 || energy.RealtimeKW != 0.85 {
		t.Errorf("energy = %+v", energy)
	}
}

func TestEnergyFailsWhenUnsupported(t *testing.T) {
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) {
		return &plainClient{}, nil
	}}
	if _, err := controller.Energy(context.Background(), testDeviceConfig()); err == nil {
		t.Fatal("Energy reported success for a client that cannot measure")
	}
}

func TestEnergyPropagatesQueryFailure(t *testing.T) {
	client := &extendedClient{energyErr: errors.New("busy")}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}
	if _, err := controller.Energy(context.Background(), testDeviceConfig()); err == nil {
		t.Fatal("Energy ignored a query failure")
	}
}

func TestDeviceReportedLimitsNarrowTheAcceptedRange(t *testing.T) {
	// When a unit publishes its own range, validation must use it rather than
	// the conservative fallback.
	client := &extendedClient{
		fakeClient: fakeClient{polls: []midea.Response{
			// The rejected apply never polls, so both reads are for the
			// accepted one: a seed poll and a verification poll.
			{Power: false, Mode: midea.ModeCool, TargetTemp: 20, FanSpeed: midea.FanAuto},
			{Power: false, Mode: midea.ModeCool, TargetTemp: 20, FanSpeed: midea.FanAuto},
		}},
		caps: midea.Capabilities{MinCoolTemp: 19, MaxCoolTemp: 21, MinHeatTemp: 19, MaxHeatTemp: 21},
	}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}

	// 25 is inside the fallback range but outside the device's own.
	if _, err := controller.Apply(context.Background(), testDeviceConfig(),
		Patch{TargetTemp: floatPtr(25)}); err == nil {
		t.Fatal("a temperature outside the device's reported range was accepted")
	}
	// 20 is inside both.
	if _, err := controller.Apply(context.Background(), testDeviceConfig(),
		Patch{TargetTemp: floatPtr(20)}); err != nil {
		t.Fatalf("a temperature inside the device's range was refused: %v", err)
	}
}

func TestLimitHelpers(t *testing.T) {
	if got := minPositive(0, 0); got != 0 {
		t.Errorf("minPositive(0,0) = %v", got)
	}
	if got := minPositive(0, 18, 17); got != 17 {
		t.Errorf("minPositive(0,18,17) = %v", got)
	}
	if got := maxPositive(0, 18, 30); got != 30 {
		t.Errorf("maxPositive(0,18,30) = %v", got)
	}
	if got := maxPositive(); got != 0 {
		t.Errorf("maxPositive() = %v", got)
	}
}

// countingClient records overlapping polls.
type countingClient struct {
	fakeClient
	onPoll func()
}

func (c *countingClient) Poll(ctx context.Context) (midea.Response, error) {
	if c.onPoll != nil {
		c.onPoll()
	}
	return c.fakeClient.Poll(ctx)
}

// plainClient is the bare mandatory interface, so the optional capability and
// energy paths are genuinely unavailable. It deliberately does not embed a
// fake that would provide them.
type plainClient struct{}

func (p *plainClient) Connect(context.Context) error                { return nil }
func (p *plainClient) Close() error                                 { return nil }
func (p *plainClient) Poll(context.Context) (midea.Response, error) { return midea.Response{}, nil }
func (p *plainClient) Update(context.Context, midea.Request) (midea.Response, error) {
	return midea.Response{}, nil
}
