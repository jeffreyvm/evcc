# Optimizer

Optimizer uses mixed integer linear programming (MILP) to minize a cost function. The optimizer model implementation honors:

- energy consumption/ feed-in costs
- solar forecast
- base load (aka home) energy demand
- end of forecast commercial value
- strategy- either "charge before export" (charge loads as soon as possible) or attenuating grid peaks on the demand side, the feed-in side or both
- home battery or loadpoint/vehicle...
  - capacity, soc and charge goals
  - charge/discharge power limits and efficiency

Optimization spans N slots with N being minimum of available forecast data.

## Parameters

### Energy consumption/ feed-in costs/ Solar forecast

Grid/ feed-in/ solar tariff data.

TODO

- [ ] make feed-in optional
- [ ] make solar optional

### Base load energy demand

Collected 15min energy profile over the last 4 weeks, averaged per slot. A per-slot percentile (e.g. 50 = median, robust against a few heavy days) can be configured via `/api/profilepercentile` instead.

### Measured value blending

The solar forecast and the base load profile are anchored to the current situation
using the last completed 15min metrics slot, decaying linearly over 4 slots:

- base load: the measured home consumption replaces the first slot and decays into the profile
- solar: the scale factor measured production/forecasted production is applied to the first slot and decays towards 1

### End of forecast commercial value

Use minimum of energy consumption cost.

### Home Battery

### Loadpoint and Vehicles

- home battery or loadpoint/vehicle...
  - capacity, soc and charge goals
  - charge/discharge power limits and efficiency

Without vehicle capacity or soc a configured session energy limit is modelled instead:
state is the session's charged energy, goal is the limit. Loadpoints with neither are
not modelled at all- their power is added to the base load.

### Capacity tariff

With the capacity target enforced the grid import is capped at the target. The optimizer prices a
missed goal like import above a limit, so goals that do not fit within the target are lowered first
(earliest goal first, house and solar as forecast): the plan never counts on a peak the limiter
would not allow.

In automatic mode the capacity reserve becomes the goal of the home batteries at its deadline,
filled from solar first and from the grid within the target. Batteries whose combined fixed grid charge
power exceeds the target only store solar in the plan. A grid charge the optimizer decides only
starts with room in the slot budget for that power, and stops for the rest of the slot once the
slot is projected above target. Instead the batteries stay in self-consumption, storing the
surplus, or hold while a vehicle charges. The capacity battery control
cannot be on together with automatic mode.
