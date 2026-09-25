package controller

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	midea "github.com/thekondor/midea-porta-split"
)

// Conservative limits used when a device does not report its own range. The
// lower bound is 17 °C because that is where the protocol's primary
// temperature encoding ends; below it the device switches to an alternate
// field that this adapter does not validate.
const (
	fallbackMinTemp = 17.0
	fallbackMaxTemp = 30.0
	tempTolerance   = 0.05
)

// smartDry is reported by some firmware but is not modelled by the upstream
// client. It is decoded for display and accepted for writing, but it has not
// been exercised on the units this tool was validated against.
const smartDry midea.Mode = 6

var (
	modeByName = map[string]midea.Mode{
		"auto":      midea.ModeAuto,
		"cool":      midea.ModeCool,
		"dry":       midea.ModeDry,
		"heat":      midea.ModeHeat,
		"fan":       midea.ModeFan,
		"smart_dry": smartDry,
	}
	fanByName = map[string]midea.FanSpeed{
		"auto":   midea.FanAuto,
		"silent": midea.FanSilent,
		"low":    midea.FanLow,
		"medium": midea.FanMedium,
		"high":   midea.FanHigh,
		"full":   midea.FanFull,
	}
)

// Modes lists the accepted mode names.
func Modes() []string { return sortedKeys(modeByName) }

// FanSpeeds lists the accepted fan-speed names.
func FanSpeeds() []string { return sortedKeys(fanByName) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func modeName(mode midea.Mode) string {
	for name, value := range modeByName {
		if value == mode {
			return name
		}
	}
	return mode.String()
}

func fanName(speed midea.FanSpeed) string {
	for name, value := range fanByName {
		if value == speed {
			return name
		}
	}
	return speed.String()
}

// Patch is a sparse set of requested state changes. A nil pointer or empty
// string leaves that field untouched; the command is seeded with the device's
// current state so unmentioned fields are preserved.
type Patch struct {
	Power      *bool
	Mode       string
	TargetTemp *float64
	FanSpeed   string
	SwingV     *bool
	SwingH     *bool
	Eco        *bool
	Turbo      *bool
	Sleep      *bool
	Display    *bool

	// limits carries the device-reported temperature bounds into validation.
	// It is filled in by the controller, never by callers.
	limits limits
}

// Empty reports whether the patch would change nothing.
func (p Patch) Empty() bool {
	return p.Power == nil && p.Mode == "" && p.TargetTemp == nil && p.FanSpeed == "" &&
		p.SwingV == nil && p.SwingH == nil && p.Eco == nil && p.Turbo == nil &&
		p.Sleep == nil && p.Display == nil
}

// FieldNames lists the fields the patch changes, for error messages and logs.
func (p Patch) FieldNames() []string {
	fields := make([]string, 0, 10)
	if p.Power != nil {
		fields = append(fields, "power")
	}
	if p.Mode != "" {
		fields = append(fields, "mode")
	}
	if p.TargetTemp != nil {
		fields = append(fields, "temperature")
	}
	if p.FanSpeed != "" {
		fields = append(fields, "fan_speed")
	}
	for name, value := range map[string]*bool{
		"swing_vertical": p.SwingV, "swing_horizontal": p.SwingH,
		"eco": p.Eco, "turbo": p.Turbo, "sleep": p.Sleep, "display": p.Display,
	} {
		if value != nil {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields
}

// limits narrows the accepted temperature range when the device reports one.
type limits struct {
	min, max float64
}

func (l limits) clamp(fallbackMin, fallbackMax float64) (float64, float64) {
	if l.min > 0 {
		return l.min, l.max
	}
	return fallbackMin, fallbackMax
}

// request validates the patch and converts it into an upstream request plus
// the values that must be observed in the read-back.
func (p Patch) request(deviceLimits limits) (midea.Request, expectations, error) {
	if p.Empty() {
		return midea.Request{}, expectations{}, errors.New("no fields to change")
	}

	request := midea.Request{Beep: boolPtr(false)}
	want := expectations{}
	if p.Power != nil {
		request.Power = p.Power
		want.power = p.Power
	}
	if p.Mode != "" {
		mode, ok := modeByName[strings.ToLower(p.Mode)]
		if !ok {
			return midea.Request{}, expectations{}, fmt.Errorf(
				"unknown mode %q (valid: %s)", p.Mode, strings.Join(Modes(), ", "))
		}
		request.Mode = &mode
		want.mode = mode
	}
	if p.TargetTemp != nil {
		min, max := deviceLimits.clamp(fallbackMinTemp, fallbackMaxTemp)
		temp := *p.TargetTemp
		if math.IsNaN(temp) || temp < min || temp > max {
			return midea.Request{}, expectations{}, fmt.Errorf(
				"temperature %.1f°C is outside the supported range %.1f–%.1f°C",
				temp, min, max)
		}
		if math.Abs(temp*2-math.Round(temp*2)) > 1e-9 {
			return midea.Request{}, expectations{}, fmt.Errorf("temperature %.2f°C is not a 0.5° step", temp)
		}
		request.TargetTemp = &temp
		want.temp = &temp
	}
	if p.FanSpeed != "" {
		speed, ok := fanByName[strings.ToLower(p.FanSpeed)]
		if !ok {
			return midea.Request{}, expectations{}, fmt.Errorf(
				"unknown fan speed %q (valid: %s)", p.FanSpeed, strings.Join(FanSpeeds(), ", "))
		}
		request.FanSpeed = &speed
		want.fan = speed
	}
	if p.SwingV != nil {
		request.SwingV, want.swingV = p.SwingV, p.SwingV
	}
	if p.SwingH != nil {
		request.SwingH, want.swingH = p.SwingH, p.SwingH
	}
	if p.Eco != nil {
		request.Eco, want.eco = p.Eco, p.Eco
	}
	if p.Turbo != nil {
		request.Turbo, want.turbo = p.Turbo, p.Turbo
	}
	if p.Sleep != nil {
		request.Sleep, want.sleep = p.Sleep, p.Sleep
	}
	if p.Display != nil {
		request.Display, want.display = p.Display, p.Display
	}
	return request, want, nil
}

// expectations records what the read-back must show for a patch.
type expectations struct {
	power   *bool
	mode    midea.Mode
	hasMode bool
	temp    *float64
	fan     midea.FanSpeed
	hasFan  bool
	swingV  *bool
	swingH  *bool
	eco     *bool
	turbo   *bool
	sleep   *bool
	display *bool
}

// mismatch describes the first field that did not take the requested value.
// It returns "" when the read-back matches everything that was asked for.
func (e expectations) mismatch(state State) string {
	if e.power != nil && state.Power != *e.power {
		return fmt.Sprintf("power is %t, requested %t", state.Power, *e.power)
	}
	if e.hasMode && state.Mode != modeName(e.mode) {
		return fmt.Sprintf("mode is %q, requested %q", state.Mode, modeName(e.mode))
	}
	if e.temp != nil && math.Abs(state.TargetTemperature-*e.temp) > tempTolerance {
		return fmt.Sprintf("target temperature is %.1f°C, requested %.1f°C",
			state.TargetTemperature, *e.temp)
	}
	if e.hasFan && state.FanSpeed != fanName(e.fan) {
		return fmt.Sprintf("fan speed is %q, requested %q", state.FanSpeed, fanName(e.fan))
	}
	for _, check := range []struct {
		name     string
		expected *bool
		actual   bool
	}{
		{"swing_vertical", e.swingV, state.SwingVertical},
		{"swing_horizontal", e.swingH, state.SwingHorizontal},
		{"eco", e.eco, state.Eco},
		{"turbo", e.turbo, state.Turbo},
		{"sleep", e.sleep, state.Sleep},
		{"display", e.display, state.DisplayOn},
	} {
		if check.expected != nil && check.actual != *check.expected {
			return fmt.Sprintf("%s is %t, requested %t", check.name, check.actual, *check.expected)
		}
	}
	return ""
}

func boolPtr(value bool) *bool { return &value }
