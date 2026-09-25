package main

import (
	"testing"

	"midea-control/internal/controller"
)

func TestIsFlagPassed(t *testing.T) {
	flags := newTestFlagSet(t)
	cases := []struct {
		args []string
		name string
		want bool
	}{
		{[]string{"--temp", "21"}, "temp", true},
		{[]string{"--temp=21"}, "temp", true},
		{[]string{"--mode", "cool"}, "temp", false},
		{[]string{"-temp", "21"}, "temp", true},
		{[]string{}, "temp", false},
		// A flag whose value happens to match another flag's name must not
		// count as that flag being present.
		{[]string{"--mode", "--temp"}, "temp", false},
	}
	for _, testCase := range cases {
		if got := isFlagPassed(flags, testCase.args, testCase.name); got != testCase.want {
			t.Errorf("isFlagPassed(%v, %q) = %v, want %v",
				testCase.args, testCase.name, got, testCase.want)
		}
	}
}

// A zero value must be distinguishable from "not requested": `--temp 0` is a
// request (and is then rejected by validation), while omitting --temp must
// leave the field alone.
func TestZeroTemperatureIsStillARequest(t *testing.T) {
	if !isFlagPassed(newTestFlagSet(t), []string{"--temp", "0"}, "temp") {
		t.Fatal("--temp 0 was not recognised as a request")
	}
	if isFlagPassed(newTestFlagSet(t), []string{"--mode", "cool"}, "temp") {
		t.Fatal("an omitted --temp was treated as requested")
	}
}

func TestDescribeRangeFallsBackWhenUnreported(t *testing.T) {
	got := describeRange(controller.Capabilities{})
	if got == "" {
		t.Fatal("describeRange returned an empty description")
	}
}

func TestDescribeRangeUsesReportedBounds(t *testing.T) {
	got := describeRange(controller.Capabilities{MinCoolTemp: 18, MaxCoolTemp: 30})
	if got != "18.0-30.0°C" {
		t.Errorf("describeRange = %q, want the reported range", got)
	}
	// When cooling bounds are missing, heating bounds are the fallback.
	got = describeRange(controller.Capabilities{MinHeatTemp: 17, MaxHeatTemp: 28})
	if got != "17.0-28.0°C" {
		t.Errorf("describeRange = %q, want the heating range", got)
	}
}

func TestOptionalBoolStringReflectsState(t *testing.T) {
	var unset optionalBool
	if unset.String() != "" {
		t.Errorf("unset optionalBool = %q, want empty", unset.String())
	}
	if err := unset.Set("true"); err != nil {
		t.Fatal(err)
	}
	if unset.String() != "true" {
		t.Errorf("optionalBool = %q, want true", unset.String())
	}
}
