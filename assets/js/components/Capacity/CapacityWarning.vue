<template>
	<div
		v-if="exceeded || meterFallback || slowMeters"
		class="limit-stripe small rounded-4 py-2 px-3 mb-3 d-sm-flex flex-wrap align-items-baseline justify-content-between gap-sm-2"
		data-testid="capacity-warning"
	>
		<div class="fw-bold text-uppercase text-warning mb-2 mb-sm-0">
			{{ $t("capacity.warning.title") }}
		</div>
		<div class="d-flex flex-column gap-1 flex-sm-row gap-sm-4">
			<div
				v-if="meterFallback"
				class="fw-medium"
				data-testid="capacity-warning-meter-fallback"
			>
				{{ $t("capacity.warning.meterFallback") }}
			</div>
			<div
				v-if="slowMeters"
				class="fw-medium"
				data-testid="capacity-warning-slow-meters"
			>
				{{ $t("capacity.warning.slowMeters") }}
			</div>
			<div
				v-if="exceeded"
				class="d-flex align-items-baseline gap-2"
				data-testid="capacity-warning-exceeded"
			>
				<span class="fw-medium">{{ $t("capacity.warning.exceeded") }}</span>
				<span class="fw-bold tabular text-nowrap">{{ exceededValue }}</span>
			</div>
		</div>
	</div>
</template>

<script lang="ts">
import { defineComponent, type PropType } from "vue";
import formatter, { POWER_UNIT } from "@/mixins/formatter";
import type { CapacityMode, CapacityStatus } from "@/types/evcc";

// capacity tariff warnings on the main page: lost or slow grid meter and a quarter hour above target
export default defineComponent({
	name: "CapacityWarning",
	mixins: [formatter],
	props: {
		status: Object as PropType<CapacityStatus>,
		mode: String as PropType<CapacityMode>,
	},
	computed: {
		// shadow mode does not limit, so it never falls back to minimum current
		meterFallback(): boolean {
			return this.mode === "on" && !!this.status?.meterFallback;
		},
		slowMeters(): boolean {
			return this.mode === "on" && !this.meterFallback && !!this.status?.slowMeters;
		},
		exceeded(): boolean {
			return (
				(this.mode === "on" || this.mode === "shadow") &&
				!this.meterFallback &&
				!!this.status?.exceeded
			);
		},
		exceededValue(): string {
			const projection = this.fmtW(this.status?.projection ?? 0, POWER_UNIT.KW, true, 2);
			const target = this.fmtW(this.status?.target ?? 0, POWER_UNIT.KW, true, 2);
			return `${projection} > ${target}`;
		},
	},
});
</script>

<style scoped>
/* animated throttle stripe, as the external limit warning */
.limit-stripe {
	background-color: color-mix(in srgb, var(--evcc-orange) 9%, transparent);
	background-image: repeating-linear-gradient(
		-45deg,
		color-mix(in srgb, var(--evcc-orange) 20%, transparent) 0 8px,
		transparent 8px 20px
	);
	background-size: 28.28px 28.28px;
}

@media (prefers-reduced-motion: no-preference) {
	.limit-stripe {
		animation: limit-stripe-move 1.5s linear infinite;
	}
}

@keyframes limit-stripe-move {
	to {
		background-position: 28.28px 0;
	}
}

.tabular {
	font-variant-numeric: tabular-nums;
}
</style>
