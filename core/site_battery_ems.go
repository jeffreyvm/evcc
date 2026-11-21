package core

// The EMS fork never sends a battery mode. The home batteries run on their own: self-consumption
// and the peak shaving built into the battery (Indevolt). evcc only reads their charge and power,
// and limits the loadpoints against the capacity target.
//
// Every regular path to a battery mode ends here: the optimizer in automatic mode, battery
// discharge control, the grid charge limit, grid discharge and an external mode via REST or MQTT.
// On an Indevolt both hold and charge are real-time control, in which the unit no longer stores
// the solar surplus, which is why none of them is sent.

// updateBatteryModeEms replaces updateBatteryMode in the site update and logs once that battery
// modes are not sent.
func (site *Site) updateBatteryModeEms() {
	if !site.batteryConfigured() || !site.hasBatteryControl() {
		return
	}

	if !site.batteryModeEmsLogged {
		site.batteryModeEmsLogged = true
		site.log.INFO.Println("battery mode: not sent, the batteries run on their own (ems fork)")
	}
}
