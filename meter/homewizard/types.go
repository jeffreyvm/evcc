package homewizard

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // p1Location must load on hosts without zoneinfo
)

// ApiResponse returns allows you to get basic information from the HomeWizard Energy Socket
// https://api-documentation.homewizard.com/docs/v1/api/
type ApiResponse struct {
	ProductType string `json:"product_type"`
	ApiVersion  string `json:"api_version"`
}

// StateResponse returns the actual state of the HomeWizard Energy Socket
// https://api-documentation.homewizard.com/docs/v1/state/
type StateResponse struct {
	PowerOn bool `json:"power_on"`
}

// DataResponse returns the most recent measurements from the HomeWizard device
// https://api-documentation.homewizard.com/docs/v1/measurement/
type DataResponse struct {
	ActivePowerW              float64       `json:"active_power_w"`
	ActivePowerL1W            float64       `json:"active_power_l1_w"`
	ActivePowerL2W            float64       `json:"active_power_l2_w"`
	ActivePowerL3W            float64       `json:"active_power_l3_w"`
	MonthlyPowerPeakW         *float64      `json:"montly_power_peak_w"` // sic, as spelled by the API
	MonthlyPowerPeakTimestamp PeakTimestamp `json:"montly_power_peak_timestamp"`
	TotalPowerImportT1kWh     float64       `json:"total_power_import_t1_kwh"`
	TotalPowerImportT2kWh     float64       `json:"total_power_import_t2_kwh"`
	TotalPowerImportT3kWh     float64       `json:"total_power_import_t3_kwh"`
	TotalPowerImportT4kWh     float64       `json:"total_power_import_t4_kwh"`
	TotalPowerExportT1kWh     float64       `json:"total_power_export_t1_kwh"`
	TotalPowerExportT2kWh     float64       `json:"total_power_export_t2_kwh"`
	TotalPowerExportT3kWh     float64       `json:"total_power_export_t3_kwh"`
	TotalPowerExportT4kWh     float64       `json:"total_power_export_t4_kwh"`
	ActiveCurrentA            float64       `json:"active_current_a"`
	ActiveVoltageV            float64       `json:"active_voltage_v"`
	ActiveCurrentL1A          *float64      `json:"active_current_l1_a"`
	ActiveCurrentL2A          *float64      `json:"active_current_l2_a"`
	ActiveCurrentL3A          *float64      `json:"active_current_l3_a"`
	ActiveVoltageL1V          *float64      `json:"active_voltage_l1_v"`
	ActiveVoltageL2V          *float64      `json:"active_voltage_l2_v"`
	ActiveVoltageL3V          *float64      `json:"active_voltage_l3_v"`
}

// p1Location is the time zone of P1 meter timestamps. DSMR meters in the Netherlands, Belgium
// and Luxembourg register in local time (CET/CEST), whatever the time zone of the host.
var p1Location = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Brussels")
	if err != nil {
		return time.Local
	}
	return loc
}()

// PeakTimestamp is a P1 meter timestamp YYMMDDhhmmss in local time, sent as number or string
type PeakTimestamp string

// UnmarshalJSON accepts a number or a numeric string. Anything else counts as no timestamp,
// so an unexpected format never breaks the other readings.
func (t *PeakTimestamp) UnmarshalJSON(data []byte) error {
	var n json.Number
	if json.Unmarshal(data, &n) != nil {
		n = ""
	}
	*t = PeakTimestamp(n)
	return nil
}

// Time returns the timestamp in loc. The zero time means no peak registered.
func (t PeakTimestamp) Time(loc *time.Location) (time.Time, error) {
	s := string(t)
	if s == "" || strings.Trim(s, "0") == "" {
		return time.Time{}, nil
	}

	res, err := time.ParseInLocation("060102150405", s, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid peak timestamp: %s", string(t))
	}
	return res, nil
}
