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

	return []setter{
		{"capacityMode", cs.SetCapacityMode},
		{"capacityFloor", floatSetter(cs.SetCapacityFloor)},
		{"capacityMargin", floatSetter(cs.SetCapacityMargin)},
	}
}
