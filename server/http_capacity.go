package server

import (
	"github.com/evcc-io/evcc/core/site"
	"github.com/gorilla/mux"
)

// capacityRoutes returns the capacity tariff routes if the site supports them
func capacityRoutes(s site.API) map[string]route {
	cs, ok := s.(site.CapacityAPI)
	if !ok {
		return nil
	}

	return map[string]route{
		"capacitymode":   {"POST", "/capacitymode/{value:[a-z]+}", stringHandler(cs.SetCapacityMode, cs.GetCapacityMode)},
		"capacityfloor":  {"POST", "/capacityfloor/{value:[0-9.]+}", floatHandler(cs.SetCapacityFloor, cs.GetCapacityFloor)},
		"capacitymargin": {"POST", "/capacitymargin/{value:[0-9.]+}", floatHandler(cs.SetCapacityMargin, cs.GetCapacityMargin)},
	}
}

func registerCapacityRoutes(api *mux.Router, s site.API) {
	for _, r := range capacityRoutes(s) {
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
