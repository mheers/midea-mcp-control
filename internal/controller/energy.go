package controller

import midea "github.com/thekondor/midea-porta-split"

// Energy is a device's power measurement. It is the only objective way to tell
// what an operating mode is actually doing, since the protocol reports no
// self-describing mode name.
type Energy struct {
	// TotalKWh is the lifetime consumption reported by the unit.
	TotalKWh float64 `json:"total_kwh"`
	// CurrentRunKWh is the consumption attributed to the current run.
	CurrentRunKWh float64 `json:"current_run_kwh"`
	// RealtimeKW is the instantaneous draw. A compressor draws roughly an
	// order of magnitude more than a bare fan.
	RealtimeKW float64 `json:"realtime_kw"`
}

func fromProtocolEnergy(e midea.Energy) Energy {
	return Energy{
		TotalKWh:      e.TotalKWh,
		CurrentRunKWh: e.CurrentRunKWh,
		RealtimeKW:    e.RealtimeKW,
	}
}
