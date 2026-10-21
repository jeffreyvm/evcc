import { mount, config } from "@vue/test-utils";
import { describe, expect, test } from "vite-plus/test";
import CapacityLimit from "./CapacityLimit.vue";
import { CHARGE_MODE } from "@/types/evcc";
import en from "../../../../i18n/en.json";

// minimal $t that walks en.json and fills placeholders, so tests assert on real English text
const lookup = (key: string): string | undefined => {
  const v = key.split(".").reduce<any>((o, k) => o?.[k], en);
  return typeof v === "string" ? v : undefined;
};
config.global.mocks["$t"] = (key: string, params: Record<string, string> = {}) =>
  (lookup(key) ?? key).replace(/\{(\w+)\}/g, (_, p) => params[p] ?? `{${p}}`);
config.global.mocks["$i18n"] = { locale: "en" };

const text = (props: InstanceType<typeof CapacityLimit>["$props"]) => {
  const el = mount(CapacityLimit, { props }).find("[data-testid=capacity-limit]");
  if (!el.exists()) return null;
  // title and detail are separate spans, spaced by the layout
  const spans = el.findAll("span").map((span) => span.text());
  return spans.join(" ");
};

const limited = {
  capacityMode: "on" as const,
  mode: CHARGE_MODE.SMART,
  maxCurrent: 32,
  phasesActive: 1,
  capacityLimited: true,
  capacityEnforced: true,
  capacityLimitReason: "budget" as const,
  capacityMaxPower: 3400,
};

describe("CapacityLimit", () => {
  test("hidden when off or not limited", () => {
    expect(text({ ...limited, capacityMode: "off" })).toBeNull();
    expect(text({ ...limited, capacityLimited: false })).toBeNull();
  });
  test("limited with power and reason", () => {
    expect(text(limited)).eq("Limited by capacity tariff 3.4 of 7.4 kW · quarter-hour budget");
  });
  test("reasons", () => {
    expect(text({ ...limited, capacityLimitReason: "cookingWindow" })).toContain("cooking window");
    expect(text({ ...limited, capacityLimitReason: "meterUnavailable" })).toContain(
      "grid meter unavailable"
    );
  });
  test("paused", () => {
    expect(text({ ...limited, capacityLimitReason: "paused", capacityMaxPower: 0 })).eq(
      "Limited by capacity tariff paused below minimum current"
    );
    expect(text({ ...limited, capacityMaxPower: 0 })).eq(
      "Limited by capacity tariff paused · quarter-hour budget"
    );
  });
  test("fast mode", () => {
    expect(text({ ...limited, mode: CHARGE_MODE.NOW, capacityLimited: false })).eq(
      "Fast within capacity tariff"
    );
    expect(text({ ...limited, mode: CHARGE_MODE.NOW })).eq(
      "Fast within capacity tariff 3.4 of 7.4 kW · quarter-hour budget"
    );
  });
  test("shadow mode", () => {
    expect(text({ ...limited, capacityMode: "shadow", capacityEnforced: false })).eq(
      "Capacity tariff would limit 3.4 of 7.4 kW · quarter-hour budget"
    );
  });
});
