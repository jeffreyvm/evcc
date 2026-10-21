<template>
	<Card
		v-if="available"
		:title="$t('capacity.card.title')"
		:subtitle="subtitle"
		data-testid="capacity-card"
	>
		<template #actions>
			<button
				type="button"
				class="btn btn-sm btn-outline-secondary border-0 p-2 evcc-gray"
				:aria-label="$t('capacity.card.settings')"
				:title="$t('capacity.card.settings')"
				data-testid="capacity-settings-button"
				@click="openSettings"
			>
				<shopicon-regular-adjust size="s"></shopicon-regular-adjust>
			</button>
		</template>

		<template v-if="active">
			<p
				v-if="status?.meterFallback"
				class="text-danger mb-3"
				data-testid="capacity-meter-fallback"
			>
				{{ $t("capacity.card.meterFallback") }}
			</p>
			<div class="values d-flex flex-wrap justify-content-between column-gap-4">
				<LabelAndValue
					:label="$t('capacity.card.monthPeak')"
					:value="monthPeak"
					:extra-value="monthPeakDetail"
					align="start"
					data-testid="capacity-month-peak"
				/>
				<LabelAndValue
					:label="$t('capacity.card.target')"
					:value="targetPeak"
					:extra-value="targetDetail"
					align="center"
					data-testid="capacity-target"
				/>
				<LabelAndValue
					:label="$t('capacity.card.quarter')"
					:value="slotState"
					:extra-value="slotTime"
					align="end"
					:class="slotClass"
					data-testid="capacity-slot"
				/>
			</div>

			<template v-if="hasSlot">
				<div
					class="slot-bar position-relative rounded-pill overflow-hidden"
					role="img"
					:aria-label="slotDetail"
					data-testid="capacity-slot-bar"
				>
					<div
						class="slot-bar__projected position-absolute top-0 bottom-0 start-0"
						:class="barClass"
						:style="{ width: `${bar.projected}%` }"
					></div>
					<div
						class="slot-bar__used position-absolute top-0 bottom-0 start-0"
						:class="barClass"
						:style="{ width: `${bar.used}%` }"
					></div>
					<div
						class="slot-bar__target position-absolute top-0 bottom-0"
						:style="{ left: `calc(${bar.target}% - 2px)` }"
					></div>
				</div>
				<p class="small evcc-gray tabular mt-2 mb-0" data-testid="capacity-slot-detail">
					{{ slotDetail }}
				</p>
			</template>
			<p v-else class="small evcc-gray mb-0">{{ $t("capacity.card.noData") }}</p>

			<p v-if="capacityMode === 'shadow'" class="small text-muted mt-3 mb-0">
				{{ $t("capacity.card.shadow") }}
			</p>
		</template>

		<div
			v-else
			class="d-flex flex-wrap align-items-center justify-content-between gap-3"
			data-testid="capacity-off"
		>
			<p class="mb-0 text-muted">{{ $t("capacity.card.off") }}</p>
			<button type="button" class="btn btn-sm btn-outline-primary" @click="openSettings">
				{{ $t("capacity.card.enable") }}
			</button>
		</div>

		<CapacitySettingsModal
			ref="settings"
			:capacity-mode="capacityMode"
			:capacity-floor="capacityFloor"
			:capacity-margin="capacityMargin"
		/>
	</Card>
</template>

<script lang="ts">
import "@h2d2/shopicons/es/regular/adjust";
import { defineComponent, type PropType } from "vue";
import formatter, { POWER_UNIT } from "@/mixins/formatter";
import type { CapacityMode, CapacityStatus } from "@/types/evcc";
import Card from "../Helper/Card.vue";
import LabelAndValue from "../Helper/LabelAndValue.vue";
import CapacitySettingsModal from "./CapacitySettingsModal.vue";
import { SLOT_MS, slotAllowance, slotBar, targetPeak, type SlotBar } from "./capacity";

// capacity tariff: month peak, target peak and the running quarter hour
export default defineComponent({
	name: "CapacityCard",
	components: { Card, LabelAndValue, CapacitySettingsModal },
	mixins: [formatter],
	props: {
		capacity: Object as PropType<CapacityStatus>,
		capacityMode: String as PropType<CapacityMode>,
		capacityFloor: Number,
		capacityMargin: Number,
		gridConfigured: Boolean,
	},
	computed: {
		// missing on servers without capacity tariff support, switching on requires a grid meter
		available(): boolean {
			return !!this.capacityMode && (this.active || this.gridConfigured);
		},
		active(): boolean {
			return this.capacityMode === "shadow" || this.capacityMode === "on";
		},
		status(): CapacityStatus | undefined {
			return this.capacity;
		},
		subtitle(): string {
			return this.active ? this.$t(`capacity.settings.modes.${this.capacityMode}`) : "";
		},
		monthPeak(): string {
			const peak = this.status?.monthPeak ?? 0;
			return peak > 0
				? this.fmtW(peak, POWER_UNIT.KW)
				: this.$t("capacity.card.monthPeakNone");
		},
		monthPeakDetail(): string {
			const parts: string[] = [];
			const time = this.status?.monthPeakTime;
			if (time && (this.status?.monthPeak ?? 0) > 0) {
				const date = new Date(time);
				parts.push(`${this.fmtDayMonth(date)} ${this.fmtHourMinute(date)}`);
			}
			const source = this.status?.monthPeakSource;
			if (source) parts.push(this.$t(`capacity.card.source.${source}`));
			return parts.join(" · ");
		},
		target(): number {
			return this.status?.target ?? 0;
		},
		targetPeak(): string {
			return this.fmtW(targetPeak(this.target, this.capacityMargin), POWER_UNIT.KW);
		},
		targetDetail(): string {
			return this.$t("capacity.card.targetControl", {
				power: this.fmtW(this.target, POWER_UNIT.KW, true, 2),
			});
		},
		hasSlot(): boolean {
			return !!this.status?.slotStart && this.target > 0;
		},
		slotState(): string {
			if (!this.hasSlot) return "–";
			if (this.status?.exceeded) return this.$t("capacity.card.exceeded");
			if (this.status?.threat) return this.$t("capacity.card.threat");
			return this.$t("capacity.card.ok");
		},
		slotClass(): string {
			if (!this.hasSlot) return "";
			if (this.status?.exceeded) return "text-danger";
			if (this.status?.threat) return "text-warning";
			return "";
		},
		slotTime(): string {
			const start = this.status?.slotStart;
			return start ? this.fmtTimeSlot(new Date(start), SLOT_MS) : "";
		},
		bar(): SlotBar {
			return slotBar(this.status?.slotEnergy, this.status?.projection, this.target);
		},
		barClass(): string {
			if (this.status?.exceeded) return "bg-danger";
			if (this.status?.threat) return "bg-warning";
			return "bg-primary";
		},
		slotDetail(): string {
			const allowance = this.$t("capacity.card.allowance", {
				energy: this.fmtWh((this.status?.slotEnergy ?? 0) * 1000, POWER_UNIT.KW, false, 2),
				allowance: this.fmtWh(slotAllowance(this.target) * 1000, POWER_UNIT.KW, true, 2),
			});
			const projection = this.$t("capacity.card.projection", {
				power: this.fmtW(this.status?.projection ?? 0, POWER_UNIT.KW),
			});
			return `${allowance} · ${projection}`;
		},
	},
	methods: {
		openSettings() {
			(
				this.$refs["settings"] as InstanceType<typeof CapacitySettingsModal> | undefined
			)?.open();
		},
	},
});
</script>

<style scoped>
.slot-bar {
	height: 0.75rem;
	background-color: var(--evcc-gray-25);
}
.slot-bar__projected {
	opacity: 0.35;
}
.slot-bar__used,
.slot-bar__projected {
	transition: width var(--evcc-transition-medium) linear;
}
.slot-bar__target {
	width: 2px;
	background-color: var(--evcc-default-text);
}
.tabular {
	font-variant-numeric: tabular-nums;
}
</style>
