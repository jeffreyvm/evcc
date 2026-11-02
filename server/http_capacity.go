package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/evcc-io/evcc/core/site"
	"github.com/gorilla/mux"
)

var errInvalidBatteryPower = errors.New("invalid battery power, expected name=W")

// registerCapacityRoutes registers the capacity tariff routes if the site supports them
func registerCapacityRoutes(api *mux.Router, s site.API) {
	cs, ok := s.(site.CapacityAPI)
	if !ok {
		return
	}

	for _, r := range []route{
		{"POST", "/capacitymode/{value:[a-z]+}", stringHandler(cs.SetCapacityMode, cs.GetCapacityMode)},
		{"POST", "/capacityfloor/{value:[0-9.]+}", floatHandler(cs.SetCapacityFloor, cs.GetCapacityFloor)},
		{"POST", "/capacitymargin/{value:[0-9.]+}", floatHandler(cs.SetCapacityMargin, cs.GetCapacityMargin)},
		{"POST", "/capacitycooking/{value:[a-z]+}", boolHandler(cs.SetCapacityCooking, cs.GetCapacityCooking)},
		{"POST", "/capacitycookingweekday/{value:[0-9:,-]+|none}", stringHandler(cs.SetCapacityCookingWeekday, cs.GetCapacityCookingWeekday)},
		{"POST", "/capacitycookingweekend/{value:[0-9:,-]+|none}", stringHandler(cs.SetCapacityCookingWeekend, cs.GetCapacityCookingWeekend)},
	} {
		api.Methods(r.Methods()...).Path(r.Pattern).Handler(r.HandlerFunc)
	}

	bs, ok := s.(site.CapacityBatteryAPI)
	if !ok {
		return
	}

	for _, r := range []route{
		{"POST", "/capacitybatterymode/{value:[a-z]+}", stringHandler(bs.SetCapacityBatteryMode, bs.GetCapacityBatteryMode)},
		{"POST", "/capacityreservemode/{value:[a-z]+}", stringHandler(bs.SetCapacityReserveMode, bs.GetCapacityReserveMode)},
		{"POST", "/capacitygridcharge/{value:[a-z]+}", boolHandler(bs.SetCapacityGridCharge, bs.GetCapacityGridCharge)},
		{"POST", "/capacitybatterypower/{name:[a-zA-Z0-9_.:-]+}/{value:[0-9.]+}", capacityBatteryPowerHandler(bs)},
	} {
		api.Methods(r.Methods()...).Path(r.Pattern).Handler(r.HandlerFunc)
	}
}

// capacityBatteryPowerHandler sets the power of one battery, named in the path
func capacityBatteryPowerHandler(bs site.CapacityBatteryAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)

		power, err := parseFloat(vars["value"])
		if err == nil {
			err = bs.SetCapacityBatteryPower(vars["name"], power)
		}
		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		jsonWrite(w, bs.GetCapacityBatteryPowers())
	}
}

// capacityBatteryPowerSetter parses name=W, as sent to capacityBatteryPower/set
func capacityBatteryPowerSetter(bs site.CapacityBatteryAPI) func(string) error {
	return func(payload string) error {
		name, value, ok := strings.Cut(payload, "=")
		if !ok {
			return errInvalidBatteryPower
		}
		power, err := parseFloat(strings.TrimSpace(value))
		if err != nil {
			return err
		}
		return bs.SetCapacityBatteryPower(strings.TrimSpace(name), power)
	}
}

// capacitySetters returns the capacity tariff mqtt setters if the site supports them
func capacitySetters(s site.API) []setter {
	cs, ok := s.(site.CapacityAPI)
	if !ok {
		return nil
	}

	res := []setter{
		{"capacityMode", cs.SetCapacityMode},
		{"capacityFloor", floatSetter(cs.SetCapacityFloor)},
		{"capacityMargin", floatSetter(cs.SetCapacityMargin)},
		{"capacityCooking", boolSetter(cs.SetCapacityCooking)},
		{"capacityCookingWeekday", cs.SetCapacityCookingWeekday},
		{"capacityCookingWeekend", cs.SetCapacityCookingWeekend},
	}

	if bs, ok := s.(site.CapacityBatteryAPI); ok {
		res = append(res,
			setter{"capacityBatteryMode", bs.SetCapacityBatteryMode},
			setter{"capacityReserveMode", bs.SetCapacityReserveMode},
			setter{"capacityGridCharge", boolSetter(bs.SetCapacityGridCharge)},
			setter{"capacityBatteryPower", capacityBatteryPowerSetter(bs)},
		)
	}

	return res
}
