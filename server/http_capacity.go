package server

import (
	"github.com/evcc-io/evcc/core/site"
	"github.com/gorilla/mux"
)

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

	return res
}
