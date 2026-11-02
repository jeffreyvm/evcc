import type {
  CapacityBatteryRule,
  CapacityBatteryStatus,
  CapacityChargeSlot,
  CapacityMode,
  CapacityReserveMode,
} from "@/types/evcc";
import { SLOT_MS } from "./capacity";

export const RESERVE_MODES: CapacityReserveMode[] = ["dynamic", "fixed"];

// bound of a battery power, as validated by the server
export const BATTERY_POWER_MAX = 10000;

// power the server assumes per kWh of capacity for a battery without a configured power
export const BATTERY_POWER_PER_KWH = 200;

// tone of a rule: why a battery does what it does
export type RuleTone = "normal" | "info" | "warning" | "danger";

export function ruleTone(rule?: CapacityBatteryRule): RuleTone {
  switch (rule) {
    case "gridCharge":
      return "info";
    case "meterUnavailable":
    case "unreachable":
      return "warning";
    case "watchdog":
      return "danger";
    default:
      return "normal";
  }
}

export interface ReserveBar {
  stored: number; // % of capacity
  reserve: number; // % of capacity, marker
  below: boolean; // stored at or below the reserve
}

const pct = (v: number, capacity: number) =>
  capacity > 0 ? Math.min(100, Math.max(0, (v / capacity) * 100)) : 0;

// stored energy against the reserve, in % of the capacity
export function reserveBar(st?: CapacityBatteryStatus): ReserveBar {
  const capacity = st?.capacity ?? 0;
  const reserve = st?.reserve ?? 0;
  return {
    stored: pct(st?.energy ?? 0, capacity),
    reserve: pct(reserve, capacity),
    below: capacity > 0 && (st?.energy ?? 0) <= reserve + 0.005,
  };
}

export interface SlotRange {
  start: Date;
  end: Date;
  units: string[]; // device names, as in the slots
}

// joins consecutive planned slots with the same batteries into ranges, slots in the past are dropped
export function chargeRanges(
  slots: CapacityChargeSlot[] | null | undefined,
  now: Date
): SlotRange[] {
  const res: SlotRange[] = [];
  for (const s of slots ?? []) {
    const start = new Date(s.start);
    const end = new Date(start.getTime() + SLOT_MS);
    if (end.getTime() <= now.getTime()) continue;

    const last = res[res.length - 1];
    const units = [...s.units].sort();
    if (last && last.end.getTime() === start.getTime() && last.units.join() === units.join()) {
      last.end = end;
    } else {
      res.push({ start, end, units });
    }
  }
  return res;
}

export interface CapacityBatterySettings {
  mode: CapacityMode;
  reserveMode: CapacityReserveMode;
  gridCharge: boolean;
  powers: Record<string, number>; // W, 0 to estimate
}

// requests to send for the changed battery settings, the mode last so the new values apply at once
export function capacityBatterySettingsRequests(
  server: CapacityBatterySettings,
  values: CapacityBatterySettings
): string[] {
  const res: string[] = [];
  if (values.reserveMode !== server.reserveMode) {
    res.push(`capacityreservemode/${values.reserveMode}`);
  }
  if (values.gridCharge !== server.gridCharge) {
    res.push(`capacitygridcharge/${values.gridCharge}`);
  }
  for (const [name, power] of Object.entries(values.powers)) {
    const value = Math.round(Math.min(BATTERY_POWER_MAX, Math.max(0, power || 0)));
    if (value !== Math.round(server.powers[name] ?? 0)) {
      res.push(`capacitybatterypower/${encodeURIComponent(name)}/${value}`);
    }
  }
  if (values.mode !== server.mode) {
    res.push(`capacitybatterymode/${values.mode}`);
  }
  return res;
}
