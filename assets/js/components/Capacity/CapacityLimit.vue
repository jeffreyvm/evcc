<template>
	<div
		v-if="state"
		class="d-flex flex-wrap align-items-baseline column-gap-2 mb-3"
		:class="state.kind === 'shadow' ? 'small text-muted' : 'text-warning'"
		data-testid="capacity-limit"
	>
		<span :class="{ 'fw-bold': state.kind !== 'shadow' }">{{ title }}</span>
		<span v-if="detail" class="tabular">{{ detail }}</span>
	</div>
</template>

<script lang="ts">
import { defineComponent, type PropType } from "vue";
import store from "@/store";
import formatter, { POWER_UNIT } from "@/mixins/formatter";
import type { CHARGE_MODE, CapacityLimitReason, CapacityMode } from "@/types/evcc";
import {
	capacityLimitState,
	limitReasonKey,
	loadpointMaxPower,
	type CapacityLimitState,
} from "./capacity";

// capacity tariff limit of a loadpoint, e.g. "Limited by capacity tariff 3.4 of 7.4 kW · quarter-hour budget"
export default defineComponent({
	name: "CapacityLimit",
	mixins: [formatter],
	props: {
		mode: String as PropType<CHARGE_MODE>,
		maxCurrent: Number,
		phasesActive: Number,
		capacityLimited: Boolean,
		capacityLimitReason: String as PropType<CapacityLimitReason>,
		capacityMaxPower: Number,
		capacityEnforced: Boolean,
		// site mode, read from the store unless given
		capacityMode: String as PropType<CapacityMode>,
	},
	computed: {
		siteMode(): CapacityMode | undefined {
			return this.capacityMode ?? store.state.capacityMode;
		},
		state(): CapacityLimitState | null {
			return capacityLimitState({
				capacityMode: this.siteMode,
				mode: this.mode,
				capacityLimited: this.capacityLimited,
				capacityEnforced: this.capacityEnforced,
				capacityLimitReason: this.capacityLimitReason,
				capacityMaxPower: this.capacityMaxPower,
			});
		},
		title(): string {
			return this.$t(`capacity.loadpoint.${this.state?.kind}`);
		},
		power(): string {
			const max = loadpointMaxPower(this.maxCurrent, this.phasesActive);
			const limit = this.state?.maxPower ?? 0;
			if (max <= 0) return this.fmtW(limit, POWER_UNIT.KW);
			return this.$t("capacity.loadpoint.powerOfMax", {
				power: this.fmtW(limit, POWER_UNIT.KW, false),
				max: this.fmtW(max, POWER_UNIT.KW),
			});
		},
		detail(): string {
			const limit = this.state?.maxPower;
			if (limit === undefined) return "";
			const reason = this.state?.reason;
			const parts: string[] = [];
			if (limit > 0) {
				parts.push(this.power);
			} else if (reason !== "paused") {
				parts.push(this.$t("capacity.loadpoint.stopped"));
			}
			const key = limitReasonKey(reason);
			if (key) parts.push(this.$t(`capacity.reason.${key}`));
			return parts.join(" · ");
		},
	},
});
</script>

<style scoped>
.tabular {
	font-variant-numeric: tabular-nums;
}
</style>
