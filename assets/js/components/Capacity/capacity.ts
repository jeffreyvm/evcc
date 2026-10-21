import { CHARGE_MODE, type CapacityLimitReason, type CapacityMode } from "@/types/evcc";

export const SLOT_MS = 15 * 60 * 1000;
export const NOMINAL_VOLTAGE = 230;

// bounds of the capacity settings, as validated by the server
export const FLOOR_MIN = 2500;
export const FLOOR_MAX = 25000;
export const MARGIN_MIN = 0;
export const MARGIN_MAX = 1000;

export const CAPACITY_MODES: CapacityMode[] = ["off", "shadow", "on"];

export type CapacityLimitKind = "limited" | "shadow" | "fast";

export interface CapacityLimitInput {
  capacityMode?: CapacityMode;
  mode?: CHARGE_MODE;
  capacityLimited?: boolean;
  capacityEnforced?: boolean;
  capacityLimitReason?: CapacityLimitReason;
  capacityMaxPower?: number;
}

export interface CapacityLimitState {
  kind: CapacityLimitKind;
  // limit details, missing when fast mode is not limited
  reason?: CapacityLimitReason;
  maxPower?: number;
}

// what the loadpoint card shows about the capacity tariff, null for nothing
export function capacityLimitState(lp: CapacityLimitInput): CapacityLimitState | null {
  if (!lp.capacityMode || lp.capacityMode === "off") return null;

  const fast = lp.mode === CHARGE_MODE.NOW && lp.capacityMode === "on";

  if (lp.capacityLimited) {
    return {
      kind: lp.capacityEnforced ? (fast ? "fast" : "limited") : "shadow",
      reason: lp.capacityLimitReason || undefined,
      maxPower: lp.capacityMaxPower ?? 0,
    };
  }

  return fast ? { kind: "fast" } : null;
}

// i18n key below capacity.reason for a limit reason
export function limitReasonKey(reason?: CapacityLimitReason): string | undefined {
  switch (reason) {
    case "budget":
    case "cookingWindow":
    case "minCurrentHold":
    case "paused":
    case "meterUnavailable":
      return reason;
    default:
      return undefined;
  }
}

export function loadpointMaxPower(maxCurrent = 0, phases = 1): number {
  return maxCurrent * Math.max(1, phases) * NOMINAL_VOLTAGE;
}

// the peak the target is derived from: the control target plus the margin kept free below it
export function targetPeak(target = 0, margin = 0): number {
  return target + margin;
}

export interface SlotBar {
  // shares of the bar width in %
  used: number;
  projected: number;
  target: number;
}

// quarter-hour bar: energy used so far and projected, against the energy the target allows
export function slotBar(slotEnergy = 0, projection = 0, target = 0): SlotBar {
  const targetKWh = Math.max(0, target) / 4000;
  const projectedKWh = Math.max(0, projection) / 4000;
  const usedKWh = Math.max(0, slotEnergy);
  const scale = Math.max(targetKWh, projectedKWh, usedKWh);
  if (scale <= 0) return { used: 0, projected: 0, target: 100 };

  const pct = (v: number) => Math.round((v / scale) * 1000) / 10;
  return {
    used: pct(usedKWh),
    projected: pct(Math.max(usedKWh, projectedKWh)),
    target: pct(targetKWh),
  };
}

// slot energy in kWh the target allows
export function slotAllowance(target = 0): number {
  return Math.max(0, target) / 4000;
}

export interface CapacitySettings {
  mode: CapacityMode;
  floor: number; // kW, as edited
  margin: number; // W
}

// REST calls for the changed settings. The mode goes last, so switching on uses the new values.
export function capacitySettingsRequests(
  server: CapacitySettings,
  values: CapacitySettings
): string[] {
  const res: string[] = [];
  const floor = Math.round(values.floor * 1000);
  if (floor !== Math.round(server.floor * 1000)) {
    res.push(`capacityfloor/${encodeURIComponent(floor)}`);
  }
  const margin = Math.round(values.margin);
  if (margin !== Math.round(server.margin)) {
    res.push(`capacitymargin/${encodeURIComponent(margin)}`);
  }
  if (values.mode !== server.mode) {
    res.push(`capacitymode/${encodeURIComponent(values.mode)}`);
  }
  return res;
}
