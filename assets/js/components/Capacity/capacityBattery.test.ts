import { describe, expect, test } from "vite-plus/test";
import type { CapacityBatteryStatus } from "@/types/evcc";
import {
  capacityBatterySettingsRequests,
  chargeRanges,
  reserveBar,
  ruleTone,
  type CapacityBatterySettings,
} from "./capacityBattery";

const status = (energy: number, reserve: number, fastFloor: number): CapacityBatteryStatus => ({
  mode: "on",
  control: "on",
  energy,
  capacity: 10,
  reserve,
  available: 2000,
  fastFloor,
  gridCharge: true,
  units: [],
  plan: null,
});

describe("reserveBar", () => {
  test("stored against reserve and fast floor", () => {
    expect(reserveBar(status(5, 3.5, 1.5))).toEqual({
      stored: 50,
      reserve: 35,
      fastFloor: 15,
      below: false,
    });
  });
  test("no fast floor marker when it equals the reserve", () => {
    expect(reserveBar(status(3.5, 3.5, 3.5)).fastFloor).toBe(0);
  });
  test("below at the reserve", () => {
    expect(reserveBar(status(3.5, 3.5, 1.5)).below).toBe(true);
  });
  test("empty without status", () => {
    expect(reserveBar(undefined)).toEqual({ stored: 0, reserve: 0, fastFloor: 0, below: false });
  });
});

describe("chargeRanges", () => {
  const slot = (start: string, units: string[]) => ({ start, units, power: 0 });
  const now = new Date("2026-11-03T05:50:00+01:00");

  test("joins consecutive slots with the same batteries, drops past ones", () => {
    const ranges = chargeRanges(
      [
        slot("2026-11-03T05:30:00+01:00", ["garage", "zolder"]),
        slot("2026-11-03T06:00:00+01:00", ["zolder", "garage"]),
        slot("2026-11-03T06:15:00+01:00", ["garage", "zolder"]),
        slot("2026-11-03T06:30:00+01:00", ["zolder"]),
        slot("2026-11-03T07:00:00+01:00", ["zolder"]),
      ],
      now
    );
    expect(ranges.map((r) => [r.start.toISOString(), r.end.toISOString(), r.units])).toEqual([
      ["2026-11-03T05:00:00.000Z", "2026-11-03T05:30:00.000Z", ["garage", "zolder"]],
      ["2026-11-03T05:30:00.000Z", "2026-11-03T05:45:00.000Z", ["zolder"]],
      ["2026-11-03T06:00:00.000Z", "2026-11-03T06:15:00.000Z", ["zolder"]],
    ]);
  });
  test("nothing without slots", () => {
    expect(chargeRanges(null, now)).toEqual([]);
  });
});

describe("ruleTone", () => {
  test("watchdog is an error, unreachable a warning", () => {
    expect(ruleTone("watchdog")).toBe("danger");
    expect(ruleTone("unreachable")).toBe("warning");
    expect(ruleTone("reserve")).toBe("info");
    expect(ruleTone("normal")).toBe("normal");
  });
});

describe("capacityBatterySettingsRequests", () => {
  const server: CapacityBatterySettings = {
    mode: "off",
    fastBattery: "buffer",
    reserveMode: "dynamic",
    gridCharge: false,
    powers: { garage: 0, zolder: 1200 },
  };

  test("nothing changed", () => {
    expect(
      capacityBatterySettingsRequests(server, { ...server, powers: { ...server.powers } })
    ).toEqual([]);
  });
  test("changed values, the mode last", () => {
    expect(
      capacityBatterySettingsRequests(server, {
        mode: "shadow",
        fastBattery: "reserve",
        reserveMode: "fixed",
        gridCharge: true,
        powers: { garage: 800, zolder: 1200 },
      })
    ).toEqual([
      "capacityfastbattery/reserve",
      "capacityreservemode/fixed",
      "capacitygridcharge/true",
      "capacitybatterypower/garage/800",
      "capacitybatterymode/shadow",
    ]);
  });
  test("an emptied power estimates again", () => {
    expect(
      capacityBatterySettingsRequests(server, {
        ...server,
        powers: { garage: 0, zolder: NaN },
      })
    ).toEqual(["capacitybatterypower/zolder/0"]);
  });
});
