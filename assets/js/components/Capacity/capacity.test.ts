import { describe, expect, test } from "vite-plus/test";
import { CHARGE_MODE } from "@/types/evcc";
import {
  capacityLimitState,
  capacitySettingsRequests,
  limitReasonKey,
  loadpointMaxPower,
  slotAllowance,
  slotBar,
  targetPeak,
} from "./capacity";

describe("capacityLimitState", () => {
  test("nothing without capacity tariff or when off", () => {
    expect(capacityLimitState({ capacityLimited: true, capacityEnforced: true })).toBeNull();
    expect(
      capacityLimitState({ capacityMode: "off", capacityLimited: true, capacityEnforced: true })
    ).toBeNull();
  });
  test("nothing when not limited in smart mode", () => {
    expect(capacityLimitState({ capacityMode: "on", mode: CHARGE_MODE.SMART })).toBeNull();
  });
  test("limited when enforced", () => {
    expect(
      capacityLimitState({
        capacityMode: "on",
        mode: CHARGE_MODE.SMART,
        capacityLimited: true,
        capacityEnforced: true,
        capacityLimitReason: "budget",
        capacityMaxPower: 3400,
      })
    ).toEqual({ kind: "limited", reason: "budget", maxPower: 3400 });
  });
  test("shadow when not enforced", () => {
    expect(
      capacityLimitState({
        capacityMode: "shadow",
        mode: CHARGE_MODE.NOW,
        capacityLimited: true,
        capacityEnforced: false,
        capacityLimitReason: "cookingWindow",
        capacityMaxPower: 1380,
      })
    ).toEqual({ kind: "shadow", reason: "cookingWindow", maxPower: 1380 });
  });
  test("fast within capacity tariff", () => {
    expect(capacityLimitState({ capacityMode: "on", mode: CHARGE_MODE.NOW })).toEqual({
      kind: "fast",
    });
    expect(capacityLimitState({ capacityMode: "shadow", mode: CHARGE_MODE.NOW })).toBeNull();
  });
  test("fast and limited keeps the limit details", () => {
    expect(
      capacityLimitState({
        capacityMode: "on",
        mode: CHARGE_MODE.NOW,
        capacityLimited: true,
        capacityEnforced: true,
        capacityLimitReason: "paused",
        capacityMaxPower: 0,
      })
    ).toEqual({ kind: "fast", reason: "paused", maxPower: 0 });
  });
  test("empty reason", () => {
    expect(
      capacityLimitState({
        capacityMode: "on",
        capacityLimited: true,
        capacityEnforced: true,
        capacityLimitReason: "",
      })
    ).toEqual({ kind: "limited", reason: undefined, maxPower: 0 });
  });
});

describe("limitReasonKey", () => {
  test("known reasons", () => {
    expect(limitReasonKey("budget")).eq("budget");
    expect(limitReasonKey("meterUnavailable")).eq("meterUnavailable");
  });
  test("unknown or empty", () => {
    expect(limitReasonKey("")).toBeUndefined();
    expect(limitReasonKey(undefined)).toBeUndefined();
    expect(limitReasonKey("batteryReserve" as any)).toBeUndefined();
  });
});

describe("power helpers", () => {
  test("loadpoint maximum", () => {
    expect(loadpointMaxPower(32, 1)).eq(7360);
    expect(loadpointMaxPower(16, 3)).eq(11040);
    expect(loadpointMaxPower(32, 0)).eq(7360);
    expect(loadpointMaxPower()).eq(0);
  });
  test("target peak adds the margin back", () => {
    expect(targetPeak(2350, 150)).eq(2500);
    expect(targetPeak(2350)).eq(2350);
  });
  test("slot allowance", () => {
    expect(slotAllowance(2350)).toBeCloseTo(0.5875);
    expect(slotAllowance(-1)).eq(0);
  });
});

describe("slotBar", () => {
  test("below target", () => {
    // 0.3 of 0.5875 kWh used, 1.9 kW projected
    expect(slotBar(0.3, 1900, 2350)).toEqual({ used: 51.1, projected: 80.9, target: 100 });
  });
  test("projection above target moves the target mark", () => {
    expect(slotBar(0.5, 2800, 2350)).toEqual({ used: 71.4, projected: 100, target: 83.9 });
  });
  test("used is never beyond projected", () => {
    expect(slotBar(0.4, 0, 2350).projected).eq(slotBar(0.4, 0, 2350).used);
  });
  test("no target", () => {
    expect(slotBar(0, 0, 0)).toEqual({ used: 0, projected: 0, target: 100 });
  });
});

describe("capacitySettingsRequests", () => {
  const server = { mode: "off" as const, floor: 2.5, margin: 150 };
  test("nothing changed", () => {
    expect(capacitySettingsRequests(server, { ...server })).toEqual([]);
  });
  test("floor in kW is sent in W", () => {
    expect(capacitySettingsRequests(server, { ...server, floor: 3.2 })).toEqual([
      "capacityfloor/3200",
    ]);
  });
  test("mode goes last", () => {
    expect(
      capacitySettingsRequests(server, { mode: "shadow", floor: 2.5, margin: 200 })
    ).toEqual(["capacitymargin/200", "capacitymode/shadow"]);
  });
});
