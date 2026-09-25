package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	midea "github.com/thekondor/midea-porta-split"

	"midea-control/internal/config"
)

type fakeClient struct {
	polls      []midea.Response
	update     midea.Response
	pollIndex  int
	connects   int
	closes     int
	updates    []midea.Request
	connectErr error
	pollErr    error
}

func (f *fakeClient) Connect(context.Context) error {
	f.connects++
	return f.connectErr
}

func (f *fakeClient) Close() error {
	f.closes++
	return nil
}

func (f *fakeClient) Poll(context.Context) (midea.Response, error) {
	if f.pollErr != nil {
		return midea.Response{}, f.pollErr
	}
	if f.pollIndex >= len(f.polls) {
		return midea.Response{}, errors.New("no fake poll response")
	}
	response := f.polls[f.pollIndex]
	f.pollIndex++
	return response, nil
}

// extendedClient adds the optional capability and energy queries so the
// optional-extension paths can be exercised.
type extendedClient struct {
	fakeClient
	caps      midea.Capabilities
	capsErr   error
	energy    midea.Energy
	energyErr error
}

func (f *extendedClient) Capabilities(context.Context) (midea.Capabilities, error) {
	return f.caps, f.capsErr
}

func (f *extendedClient) Energy(context.Context) (midea.Energy, error) {
	return f.energy, f.energyErr
}

func (f *fakeClient) Update(_ context.Context, request midea.Request) (midea.Response, error) {
	f.updates = append(f.updates, request)
	return f.update, nil
}

func TestProtocolOptionsSeparateReadAndWriteRetries(t *testing.T) {
	if got := protocolOptions(ReadOperation).CmdMaxAttempts; got != 2 {
		t.Fatalf("read CmdMaxAttempts = %d, want 2", got)
	}
	if got := protocolOptions(WriteOperation).CmdMaxAttempts; got != 1 {
		t.Fatalf("write CmdMaxAttempts = %d, want 1", got)
	}
}

func TestSetPowerSeedsReadsAndVerifies(t *testing.T) {
	before := midea.Response{Power: false, Mode: midea.ModeHeat, TargetTemp: 22, FanSpeed: midea.FanAuto}
	after := midea.Response{Power: true, Mode: midea.ModeHeat, TargetTemp: 22, FanSpeed: midea.FanAuto}
	client := &fakeClient{polls: []midea.Response{before, after}, update: after}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}

	state, err := controller.SetPower(context.Background(), config.Device{Name: "bedroom"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Power || state.Mode != "heat" {
		t.Fatalf("state = %+v, want powered heat state", state)
	}
	// A separate connection is used for the write and for the read-back.
	if client.connects != 2 || client.closes != 2 {
		t.Fatalf("connect/close = %d/%d, want 2/2", client.connects, client.closes)
	}
	// One seed poll before the write, one verification poll after it.
	if client.pollIndex != 2 || len(client.updates) != 1 {
		t.Fatalf("polls/updates = %d/%d, want 2/1", client.pollIndex, len(client.updates))
	}
	if client.updates[0].Power == nil || !*client.updates[0].Power {
		t.Fatalf("power request = %+v, want true pointer", client.updates[0].Power)
	}
	if client.updates[0].Mode != nil || client.updates[0].TargetTemp != nil || client.updates[0].FanSpeed != nil {
		t.Fatalf("power request changed another field: %+v", client.updates[0])
	}
}

func TestSetPowerRejectsMismatchedReadback(t *testing.T) {
	client := &fakeClient{
		polls:  []midea.Response{{Power: false}, {Power: false}},
		update: midea.Response{Power: true},
	}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}
	if _, err := controller.SetPower(context.Background(), config.Device{Name: "bedroom"}, true); err == nil {
		t.Fatal("SetPower accepted a read-back with the wrong power state")
	}
}

func TestSetPowerReportsUnknownDeliveryOnReadbackFailure(t *testing.T) {
	client := &fakeClient{
		polls:  []midea.Response{{Power: false}},
		update: midea.Response{Power: true},
	}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}
	_, err := controller.SetPower(context.Background(), config.Device{Name: "bedroom"}, true)
	if err == nil {
		t.Fatal("SetPower succeeded despite failed read-back")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "delivery unknown") {
		t.Fatalf("error = %q, want delivery-unknown context", got)
	}
}
