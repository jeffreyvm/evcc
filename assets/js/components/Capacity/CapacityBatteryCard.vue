<template>
	<Card
		v-if="available"
		:title="$t('capacity.battery.title')"
		:subtitle="subtitle"
		data-testid="capacity-battery-card"
	>
		<template #actions>
			<button
				type="button"
				class="btn btn-sm btn-outline-secondary border-0 p-2 evcc-gray"
				:aria-label="$t('capacity.card.settings')"
				:title="$t('capacity.card.settings')"
				data-testid="capacity-battery-settings-button"
				@click="openSettings"
			>
				<shopicon-regular-adjust size="s"></shopicon-regular-adjust>
			</button>
		</template>

		<template v-if="active && status">
			<div class="values d-flex flex-wrap justify-content-between column-gap-4">
				<LabelAndValue
					:label="$t('capacity.battery.reserve')"
					:value="reserveValue"
					:extra-value="reserveDetail"
					align="start"
					data-testid="capacity-battery-reserve"
				/>
				<LabelAndValue
					:label="$t('capacity.battery.available')"
					:value="availableValue"
					:extra-value="$t('capacity.battery.availableDetail')"
					align="center"
					data-testid="capacity-battery-available"
				/>
				<LabelAndValue
					:label="$t('capacity.battery.gridCharge')"
					:value="gridChargeValue"
					:extra-value="gridChargeDetail"
					align="end"
					data-testid="capacity-battery-grid-charge"
				/>
			</div>

			<div
				class="reserve-bar position-relative rounded-pill overflow-hidden"
				role="img"
				:aria-label="barLabel"
				data-testid="capacity-battery-bar"
			>
				<div
					class="reserve-bar__stored position-absolute top-0 bottom-0 start-0"
					:class="bar.below ? 'bg-warning' : 'bg-primary'"
					:style="{ width: `${bar.stored}%` }"
				></div>
				<div
					v-if="bar.fastFloor > 0"
					class="reserve-bar__floor position-absolute top-0 bottom-0"
					:style="{ left: `calc(${bar.fastFloor}% - 1px)` }"
				></div>
				<div
					class="reserve-bar__reserve position-absolute top-0 bottom-0"
					:style="{ left: `calc(${bar.reserve}% - 2px)` }"
				></div>
			</div>
			<p class="small evcc-gray tabular mt-2 mb-3" data-testid="capacity-battery-plan">
				{{ planDetail }}
			</p>

			<ul class="list-unstyled mb-0" data-testid="capacity-battery-units">
				<li
					v-for="u in units"
					:key="u.name"
					class="unit d-flex flex-wrap align-items-baseline column-gap-2 py-2"
					:data-testid="`capacity-battery-unit-${u.name}`"
				>
					<span class="fw-semibold">{{ u.title || u.name }}</span>
					<span class="evcc-gray tabular">{{ fmtPercentage(u.soc) }}</span>
					<span class="badge rounded-pill" :class="`mode--${u.mode}`">
						{{ $t(`capacity.battery.modes.${u.mode}`) }}
					</span>
					<span class="small" :class="toneClass(u.rule)">
						{{ $t(`capacity.battery.rules.${u.rule}`) }}
					</span>
					<span v-if="u.failed" class="small text-danger w-100">
						{{ $t("capacity.battery.failed") }}
					</span>
					<span class="small evcc-gray w-100 tabular">
						{{ unitPowers(u) }}
					</span>
				</li>
			</ul>

			<p v-if="status.mode === 'shadow'" class="small text-muted mt-3 mb-0">
				{{ $t("capacity.battery.shadow") }}
			</p>
			<p
				v-else-if="status.control === 'external' || status.control === 'start'"
				class="small text-warning mt-3 mb-0"
				data-testid="capacity-battery-control"
			>
				{{ $t(`capacity.battery.control.${status.control}`) }}
			</p>
		</template>

		<div
			v-else
			class="d-flex flex-wrap align-items-center justify-content-between gap-3"
			data-testid="capacity-battery-off"
		>
			<p class="mb-0 text-muted">{{ offText }}</p>
			<button type="button" class="btn btn-sm btn-outline-primary" @click="openSettings">
				{{ $t("capacity.card.enable") }}
			</button>
		</div>

		<CapacityBatterySettingsModal
			ref="settings"
			:capacity-mode="capacityMode"
			:battery-mode="batteryMode"
			:fast-battery="fastBattery"
			:reserve-mode="reserveMode"
			:grid-charge="gridCharge"
			:powers="powers"
			:batteries="settingsBatteries"
		/>
	</Card>
</template>

<script lang="ts">
import "@h2d2/shopicons/es/regular/adjust";
import { defineComponent, type PropType } from "vue";
import formatter, { POWER_UNIT } from "@/mixins/formatter";
import type {
	CapacityBatteryRule,
	CapacityBatteryStatus,
	CapacityBatteryUnit,
	CapacityFastBattery,
	CapacityMode,
	CapacityReserveMode,
} from "@/types/evcc";
import Card from "../Helper/Card.vue";
import LabelAndValue from "../Helper/LabelAndValue.vue";
import CapacityBatterySettingsModal, {
	type BatteryOption,
} from "./CapacityBatterySettingsModal.vue";
import {
	chargeRanges,
	reserveBar,
	ruleTone,
	type ReserveBar,
	type SlotRange,
} from "./capacityBattery";

// capacity tariff battery control: reserve, mode per battery and grid charge plan
export default defineComponent({
	name: "CapacityBatteryCard",
	components: { Card, LabelAndValue, CapacityBatterySettingsModal },
	mixins: [formatter],
	props: {
		capacityBattery: Object as PropType<CapacityBatteryStatus>,
		capacityMode: String as PropType<CapacityMode>,
		batteryMode: String as PropType<CapacityMode>,
		fastBattery: String as PropType<CapacityFastBattery>,
		reserveMode: String as PropType<CapacityReserveMode>,
		gridCharge: Boolean,
		powers: Object as PropType<Record<string, number>>,
		batteries: { type: Array as PropType<BatteryOption[]>, default: () => [] },
		now: { type: Date, default: () => new Date() },
	},
	data() {
		return { POWER_UNIT };
	},
	computed: {
		// missing on servers without battery control, and without a controllable battery
		available(): boolean {
			return !!this.batteryMode && this.batteries.length > 0;
		},
		status(): CapacityBatteryStatus | undefined {
			return this.capacityBattery;
		},
		active(): boolean {
			const mode = this.status?.mode;
			return mode === "shadow" || mode === "on";
		},
		subtitle(): string {
			return this.active ? this.$t(`capacity.settings.modes.${this.status?.mode}`) : "";
		},
		offText(): string {
			if (this.batteryMode && this.batteryMode !== "off" && this.capacityMode === "off") {
				return this.$t("capacity.battery.needsCapacity");
			}
			return this.$t("capacity.battery.off");
		},
		units(): CapacityBatteryUnit[] {
			return this.status?.units ?? [];
		},
		// batteries whose powers come from the meter need no setting
		settingsBatteries(): BatteryOption[] {
			const fromMeter = new Set(
				this.units.filter((u) => u.powerSource === "meter").map((u) => u.name)
			);
			return this.batteries.filter((b) => !fromMeter.has(b.name));
		},
		reserveValue(): string {
			const st = this.status;
			return this.$t("capacity.battery.reserveValue", {
				energy: this.fmtKWhShort(st?.energy ?? 0, false),
				reserve: this.fmtKWhShort(st?.reserve ?? 0, true),
			});
		},
		reserveDetail(): string {
			const deadline = this.status?.plan?.deadline;
			return deadline
				? this.$t("capacity.battery.deadline", {
						time: this.fmtHourMinute(new Date(deadline)),
					})
				: "";
		},
		availableValue(): string {
			return this.fmtW(this.status?.available ?? 0, POWER_UNIT.KW);
		},
		ranges(): SlotRange[] {
			return chargeRanges(this.status?.plan?.slots, this.now);
		},
		gridChargeValue(): string {
			if (!this.status?.gridCharge) return this.$t("capacity.battery.gridChargeOff");
			const first = this.ranges[0];
			if (!first) return this.$t("capacity.battery.gridChargeNone");
			return `${this.fmtHourMinute(first.start)}–${this.fmtHourMinute(first.end)}`;
		},
		gridChargeDetail(): string {
			const plan = this.status?.plan;
			if (!this.status?.gridCharge || !plan || plan.need <= 0) return "";
			return this.$t("capacity.battery.gridChargeEnergy", {
				energy: this.fmtKWhShort(plan.need, true),
			});
		},
		bar(): ReserveBar {
			return reserveBar(this.status);
		},
		barLabel(): string {
			return `${this.$t("capacity.battery.reserve")}: ${this.reserveValue}`;
		},
		planDetail(): string {
			const plan = this.status?.plan;
			if (!plan) return this.$t("capacity.card.noData");

			const season = this.fmtKWhShort(plan.season, true);
			const parts = [
				plan.fixed
					? this.$t("capacity.battery.planFixed", { season })
					: this.$t("capacity.battery.planDynamic", {
							season,
							peaks: this.fmtKWhShort(plan.peaks, true),
						}),
				this.$t("capacity.battery.planSolar", {
					energy: this.fmtKWhShort(plan.surplus, true),
				}),
			];
			if (this.status?.gridCharge && plan.shortage > 0.05) {
				parts.push(
					this.$t("capacity.battery.planShortage", {
						energy: this.fmtKWhShort(plan.shortage, true),
					})
				);
			}
			return parts.join(" · ");
		},
	},
	methods: {
		unitPowers(u: CapacityBatteryUnit): string {
			return this.$t(`capacity.battery.powers.${u.powerSource || "estimate"}`, {
				charge: this.fmtW(u.chargePower, POWER_UNIT.KW),
				discharge: this.fmtW(u.dischargePower, POWER_UNIT.KW),
			});
		},
		fmtKWhShort(kWh: number, withUnit: boolean): string {
			return this.fmtWh(kWh * 1000, POWER_UNIT.KW, withUnit, 1);
		},
		toneClass(rule: CapacityBatteryRule): string {
			switch (ruleTone(rule)) {
				case "warning":
					return "text-warning";
				case "danger":
					return "text-danger";
				default:
					return "evcc-gray";
			}
		},
		openSettings() {
			(
				this.$refs["settings"] as
					| InstanceType<typeof CapacityBatterySettingsModal>
					| undefined
			)?.open();
		},
	},
});
</script>

<style scoped>
.reserve-bar {
	height: 0.75rem;
	background-color: var(--evcc-gray-25);
}
.reserve-bar__stored {
	transition: width var(--evcc-transition-medium) linear;
}
.reserve-bar__reserve {
	width: 2px;
	background-color: var(--evcc-default-text);
}
.reserve-bar__floor {
	width: 1px;
	background-color: var(--evcc-gray);
}
.unit + .unit {
	border-top: 1px solid var(--evcc-gray-25);
}
.mode--normal {
	background: var(--evcc-gray-10);
	color: var(--evcc-default-text);
}
.mode--hold {
	background: var(--evcc-battery);
	color: var(--bs-white);
}
.mode--charge {
	background: var(--evcc-green);
	color: var(--evcc-darkest-green);
}
.tabular {
	font-variant-numeric: tabular-nums;
}
</style>
