package controller

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	midea "github.com/thekondor/midea-porta-split"

	"github.com/mheers/midea-mcp-control/internal/config"
)

func floatPtr(v float64) *float64 { return &v }
func boolPtrTest(v bool) *bool    { return &v }

func TestPatchRequestRejectsEmptyPatch(t *testing.T) {
	if _, _, err := (Patch{}).request(limits{}); err == nil {
		t.Fatal("an empty patch was accepted")
	}
}

func TestPatchRequestValidatesTemperature(t *testing.T) {
	cases := []struct {
		name    string
		temp    float64
		limits  limits
		wantErr string
	}{
		{"in range", 22.0, limits{}, ""},
		{"half step", 21.5, limits{}, ""},
		{"below range", 16.0, limits{}, "outside the supported range"},
		{"above range", 31.0, limits{}, "outside the supported range"},
		{"not a half step", 22.3, limits{}, "0.5° step"},
		{"nan", math.NaN(), limits{}, "outside the supported range"},
		{"device narrows the range", 25.0, limits{min: 18, max: 22}, "outside the supported range"},
		{"device range accepted", 20.0, limits{min: 18, max: 22}, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := (Patch{TargetTemp: floatPtr(testCase.temp)}).request(testCase.limits)
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, testCase.wantErr)
			}
		})
	}
}

func TestPatchRequestRejectsUnknownNames(t *testing.T) {
	if _, _, err := (Patch{Mode: "arctic"}).request(limits{}); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if _, _, err := (Patch{FanSpeed: "ludicrous"}).request(limits{}); err == nil {
		t.Fatal("an unknown fan speed was accepted")
	}
}

func TestPatchRequestIsSparse(t *testing.T) {
	request, want, err := Patch{TargetTemp: floatPtr(19.5)}.request(limits{})
	if err != nil {
		t.Fatal(err)
	}
	if request.TargetTemp == nil || *request.TargetTemp != 19.5 {
		t.Errorf("target temp = %v, want 19.5", request.TargetTemp)
	}
	// Unrequested fields must stay nil so the device keeps its current value.
	if request.Power != nil || request.Mode != nil || request.FanSpeed != nil ||
		request.SwingV != nil || request.SwingH != nil || request.Eco != nil ||
		request.Turbo != nil || request.Sleep != nil || request.Display != nil ||
		request.TargetHumidity != nil {
		t.Errorf("unrequested fields were populated: %+v", request)
	}
	if request.Beep == nil || *request.Beep {
		t.Error("buzzer was not explicitly disabled for a scripted write")
	}
	if want.temp == nil || *want.temp != 19.5 {
		t.Errorf("expectation not recorded: %+v", want)
	}
}

func TestPatchRequestMapsEveryBoolean(t *testing.T) {
	patch := Patch{
		SwingV: boolPtrTest(true), SwingH: boolPtrTest(false),
		Eco: boolPtrTest(true), Turbo: boolPtrTest(false),
		Sleep: boolPtrTest(true), Display: boolPtrTest(false),
	}
	request, want, err := patch.request(limits{})
	if err != nil {
		t.Fatal(err)
	}
	if request.SwingV == nil || !*request.SwingV {
		t.Error("swing_v not sent")
	}
	if request.SwingH == nil || *request.SwingH {
		t.Error("swing_h=false was not sent as an explicit false")
	}
	if want.eco == nil || !*want.eco || want.turbo == nil || *want.turbo {
		t.Errorf("boolean expectations wrong: %+v", want)
	}
}

func TestExpectationsDetectMismatch(t *testing.T) {
	state := State{Power: false, Mode: "cool", TargetTemperature: 22, FanSpeed: "auto"}
	_, want, err := Patch{
		Power: boolPtrTest(true), Mode: "cool", TargetTemp: floatPtr(21.5),
	}.request(limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got := want.mismatch(state); !strings.Contains(got, "power") {
		t.Errorf("mismatch = %q, want it to name power", got)
	}
	if got := want.mismatch(State{
		Power: true, Mode: "cool", TargetTemperature: 21.5, FanSpeed: "auto",
	}); got != "" {
		t.Errorf("mismatch = %q, want no mismatch", got)
	}
}

func TestExpectationsIgnoreUnrequestedFields(t *testing.T) {
	_, want, err := Patch{Mode: "heat"}.request(limits{})
	if err != nil {
		t.Fatal(err)
	}
	// Every other field differs from the request, but must not be reported.
	state := State{Power: false, Mode: "heat", TargetTemperature: 30, FanSpeed: "low"}
	if got := want.mismatch(state); got != "" {
		t.Errorf("mismatch = %q, want no mismatch for unrequested fields", got)
	}
}

func TestApplyReportsReadbackDisagreement(t *testing.T) {
	before := midea.Response{Power: false, Mode: midea.ModeHeat, TargetTemp: 22, FanSpeed: midea.FanAuto}
	// The device ignores the requested temperature and keeps 22.
	after := midea.Response{Power: false, Mode: midea.ModeHeat, TargetTemp: 22, FanSpeed: midea.FanAuto}
	client := &fakeClient{polls: []midea.Response{before, after}, update: after}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}

	_, err := controller.Apply(context.Background(), config.Device{Name: "bedroom"}, Patch{TargetTemp: floatPtr(19)})
	if err == nil {
		t.Fatal("Apply accepted a read-back that ignored the request")
	}
	if !strings.Contains(err.Error(), "read-back disagrees") {
		t.Errorf("error = %q, want a read-back disagreement", err)
	}
}

func TestApplySendsOnlyRequestedFields(t *testing.T) {
	after := midea.Response{Power: false, Mode: midea.ModeCool, TargetTemp: 19, FanSpeed: midea.FanLow}
	client := &fakeClient{polls: []midea.Response{after, after}, update: after}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}

	mode := "cool"
	fan := "low"
	state, err := controller.Apply(context.Background(), config.Device{Name: "bedroom"}, Patch{
		Mode: mode, FanSpeed: fan, TargetTemp: floatPtr(19),
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != "cool" || state.FanSpeed != "low" || state.TargetTemperature != 19 {
		t.Errorf("state = %+v", state)
	}
	if len(client.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(client.updates))
	}
	request := client.updates[0]
	if request.Mode == nil || *request.Mode != midea.ModeCool {
		t.Errorf("mode = %v, want cool", request.Mode)
	}
	if request.Power != nil {
		t.Error("power was sent even though it was not requested")
	}
	if request.Eco != nil || request.Sleep != nil {
		t.Error("unrequested toggles were sent")
	}
}

func TestApplyUsesWriteRetryPolicyOnlyForTheWrite(t *testing.T) {
	var operations []Operation
	after := midea.Response{Power: true, Mode: midea.ModeHeat, TargetTemp: 22, FanSpeed: midea.FanAuto}
	client := &fakeClient{polls: []midea.Response{after, after}, update: after}
	controller := &Controller{NewClient: func(_ config.Device, op Operation) (Client, error) {
		operations = append(operations, op)
		return client, nil
	}}
	if _, err := controller.Apply(context.Background(), config.Device{Name: "x"}, Patch{Power: boolPtrTest(true)}); err != nil {
		t.Fatal(err)
	}
	// The connection that carries the set frame must be single-attempt; the
	// read-back connection may retry because it never re-sends a command.
	if len(operations) != 2 {
		t.Fatalf("client creations = %v, want one write and one read", operations)
	}
	if operations[0] != WriteOperation {
		t.Errorf("write client used %v, want WriteOperation", operations[0])
	}
	if operations[1] != ReadOperation {
		t.Errorf("read-back client used %v, want ReadOperation", operations[1])
	}
}

func TestApplyRejectsDeviceErrorResponse(t *testing.T) {
	after := midea.Response{Power: false, Error: true, ErrCode: 3}
	client := &fakeClient{polls: []midea.Response{after}, update: after}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}
	_, err := controller.Apply(context.Background(), config.Device{Name: "x"}, Patch{Power: boolPtrTest(true)})
	if err == nil || !strings.Contains(err.Error(), "error code 3") {
		t.Fatalf("error = %v, want the device error code to be surfaced", err)
	}
}

func TestApplyPropagatesUpdateFailure(t *testing.T) {
	client := &failingUpdateClient{}
	controller := &Controller{NewClient: func(config.Device, Operation) (Client, error) { return client, nil }}
	_, err := controller.Apply(context.Background(), config.Device{Name: "x"}, Patch{Power: boolPtrTest(true)})
	if err == nil || !errors.Is(err, errUpdateFailed) {
		t.Fatalf("error = %v, want the update failure", err)
	}
}

func TestModeAndFanNamesMatchTheReferenceTable(t *testing.T) {
	// Both msmart-ng and the upstream Go client use this table; a mismatch
	// here would mean writes and reads disagree with every other client.
	cases := map[midea.Mode]string{
		1: "auto", 2: "cool", 3: "dry", 4: "heat", 5: "fan", 6: "smart_dry",
	}
	for value, want := range cases {
		if got := modeName(midea.Mode(value)); got != want {
			t.Errorf("modeName(%d) = %q, want %q", value, got, want)
		}
	}
	fans := map[midea.FanSpeed]string{
		102: "auto", 20: "silent", 40: "low", 60: "medium", 80: "high", 100: "full",
	}
	for value, want := range fans {
		if got := fanName(midea.FanSpeed(value)); got != want {
			t.Errorf("fanName(%d) = %q, want %q", value, got, want)
		}
	}
}

var errUpdateFailed = errors.New("update failed")

type failingUpdateClient struct{}

func (f *failingUpdateClient) Connect(context.Context) error { return nil }
func (f *failingUpdateClient) Close() error                  { return nil }
func (f *failingUpdateClient) Poll(context.Context) (midea.Response, error) {
	return midea.Response{}, nil
}
func (f *failingUpdateClient) Update(context.Context, midea.Request) (midea.Response, error) {
	return midea.Response{}, errUpdateFailed
}
