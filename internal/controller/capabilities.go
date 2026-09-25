package controller

import midea "github.com/thekondor/midea-porta-split"

// Capabilities is the credential-free feature report a device returns.
type Capabilities struct {
	// CustomFanSpeed reports whether the unit accepts arbitrary fan values
	// beyond the fixed speeds this tool exposes.
	CustomFanSpeed bool `json:"custom_fan_speed"`
	// HumidityControl reports whether a target humidity can be set.
	HumidityControl bool `json:"humidity_control"`
	// SwingAngle reports whether the vertical louver angle is controllable.
	SwingAngle bool `json:"swing_angle"`
	// FreshAir, Breeze, Purifier and OutSilent mirror the device's flags.
	FreshAir  bool `json:"fresh_air"`
	Breeze    bool `json:"breeze"`
	Purifier  bool `json:"purifier"`
	OutSilent bool `json:"out_silent"`
	// Temperature bounds. A zero value means the device did not report a
	// limit, which is the case for all units validated here.
	MinCoolTemp float64 `json:"min_cool_temp"`
	MaxCoolTemp float64 `json:"max_cool_temp"`
	MinHeatTemp float64 `json:"min_heat_temp"`
	MaxHeatTemp float64 `json:"max_heat_temp"`
	// Reported reports whether the unit answered the capability query at all.
	Reported bool `json:"reported"`
}

func fromProtocolCapabilities(c midea.Capabilities) Capabilities {
	return Capabilities{
		CustomFanSpeed:  c.CustomFanSpeed,
		HumidityControl: c.HumidityControl,
		SwingAngle:      c.SwingAngle,
		FreshAir:        c.FreshAir,
		Breeze:          c.Breeze,
		Purifier:        c.Purifier,
		OutSilent:       c.OutSilent,
		MinCoolTemp:     c.MinCoolTemp,
		MaxCoolTemp:     c.MaxCoolTemp,
		MinHeatTemp:     c.MinHeatTemp,
		MaxHeatTemp:     c.MaxHeatTemp,
		Reported:        true,
	}
}
