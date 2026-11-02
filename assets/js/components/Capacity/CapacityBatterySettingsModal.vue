<template>
	<GenericModal
		id="capacityBatterySettingsModal"
		ref="modal"
		:title="$t('capacity.battery.settings.title')"
		data-testid="capacity-battery-settings-modal"
		@open="reset"
	>
		<p>{{ $t("capacity.battery.settings.description") }}</p>
		<form class="container mx-0 px-0" @submit.prevent="save">
			<FormRow
				id="capacityBatteryMode"
				:label="$t('capacity.settings.mode')"
				:help="$t(`capacity.battery.settings.modeHelp.${values.mode}`)"
			>
				<SelectGroup
					id="capacityBatteryMode"
					v-model="values.mode"
					:options="modeOptions"
					equal-width
					transparent
				/>
			</FormRow>
			<p
				v-if="values.mode !== 'off' && capacityMode !== 'on'"
				class="small text-warning mb-3"
				data-testid="capacity-battery-needs-capacity"
			>
				{{ $t("capacity.battery.needsCapacity") }}
			</p>

			<FormRow
				id="capacityReserveMode"
				:label="$t('capacity.battery.settings.reserveMode')"
				:help="$t(`capacity.battery.settings.reserveModeHelp.${values.reserveMode}`)"
			>
				<SelectGroup
					id="capacityReserveMode"
					v-model="values.reserveMode"
					:options="reserveModeOptions"
					equal-width
					transparent
				/>
			</FormRow>

			<div class="form-check form-switch mb-1">
				<input
					id="capacityGridCharge"
					v-model="values.gridCharge"
					class="form-check-input"
					type="checkbox"
					role="switch"
				/>
				<label class="form-check-label" for="capacityGridCharge">
					{{ $t("capacity.battery.settings.gridCharge") }}
				</label>
			</div>
			<p class="small text-muted mb-4">
				{{ $t("capacity.battery.settings.gridChargeHelp") }}
			</p>

			<FormRow
				v-for="b in batteries"
				:id="`capacityBatteryPower-${b.name}`"
				:key="b.name"
				:label="$t('capacity.battery.settings.power', { battery: b.title })"
				:help="
					$t('capacity.battery.settings.powerHelp', {
						estimate: fmtW((b.capacity || 0) * BATTERY_POWER_PER_KWH, POWER_UNIT.W),
					})
				"
			>
				<div class="input-group input-width">
					<input
						:id="`capacityBatteryPower-${b.name}`"
						v-model.number="values.powers[b.name]"
						type="number"
						min="0"
						:max="BATTERY_POWER_MAX"
						step="50"
						:aria-describedby="`capacityBatteryPowerUnit-${b.name}`"
						class="form-control text-end"
					/>
					<span :id="`capacityBatteryPowerUnit-${b.name}`" class="input-group-text"
						>W</span
					>
				</div>
			</FormRow>

			<div class="mt-4 d-flex justify-content-between gap-2 flex-column flex-sm-row">
				<button
					type="button"
					class="btn btn-link text-muted btn-cancel"
					data-bs-dismiss="modal"
				>
					{{ $t("config.general.cancel") }}
				</button>
				<button
					type="submit"
					class="btn btn-primary order-1 order-sm-2 flex-grow-1 flex-sm-grow-0 px-4"
					:disabled="saving || nothingChanged"
					data-testid="capacity-battery-settings-save"
				>
					<span
						v-if="saving"
						class="spinner-border spinner-border-sm"
						role="status"
						aria-hidden="true"
					></span>
					{{ $t("config.general.save") }}
				</button>
			</div>
		</form>
	</GenericModal>
</template>

<script lang="ts">
import { defineComponent, type PropType } from "vue";
import api from "@/api";
import formatter, { POWER_UNIT } from "@/mixins/formatter";
import type { CapacityMode, CapacityReserveMode } from "@/types/evcc";
import GenericModal from "../Helper/GenericModal.vue";
import SelectGroup from "../Helper/SelectGroup.vue";
import FormRow from "../Config/FormRow.vue";
import { CAPACITY_MODES } from "./capacity";
import {
	BATTERY_POWER_MAX,
	BATTERY_POWER_PER_KWH,
	RESERVE_MODES,
	capacityBatterySettingsRequests,
	type CapacityBatterySettings,
} from "./capacityBattery";

export interface BatteryOption {
	name: string;
	title: string;
	capacity: number; // kWh
}

// capacity tariff battery control: mode, reserve, grid charging, power per battery
export default defineComponent({
	name: "CapacityBatterySettingsModal",
	components: { GenericModal, SelectGroup, FormRow },
	mixins: [formatter],
	props: {
		capacityMode: String as PropType<CapacityMode>,
		batteryMode: String as PropType<CapacityMode>,
		reserveMode: String as PropType<CapacityReserveMode>,
		gridCharge: Boolean,
		powers: Object as PropType<Record<string, number>>,
		batteries: { type: Array as PropType<BatteryOption[]>, default: () => [] },
	},
	data() {
		return {
			saving: false,
			values: {
				mode: "off",
				reserveMode: "dynamic",
				gridCharge: false,
				powers: {},
			} as CapacityBatterySettings,
			BATTERY_POWER_MAX,
			BATTERY_POWER_PER_KWH,
			POWER_UNIT,
		};
	},
	computed: {
		modeOptions() {
			return CAPACITY_MODES.map((value) => ({
				value,
				name: this.$t(`capacity.settings.modes.${value}`),
			}));
		},
		reserveModeOptions() {
			return RESERVE_MODES.map((value) => ({
				value,
				name: this.$t(`capacity.battery.settings.reserveModes.${value}`),
			}));
		},
		serverValues(): CapacityBatterySettings {
			const powers: Record<string, number> = {};
			for (const b of this.batteries) {
				powers[b.name] = this.powers?.[b.name] ?? 0;
			}
			return {
				mode: this.batteryMode ?? "off",
				reserveMode: this.reserveMode ?? "dynamic",
				gridCharge: this.gridCharge,
				powers,
			};
		},
		requests(): string[] {
			return capacityBatterySettingsRequests(this.serverValues, this.values);
		},
		nothingChanged(): boolean {
			return this.requests.length === 0;
		},
	},
	methods: {
		open() {
			(this.$refs["modal"] as InstanceType<typeof GenericModal> | undefined)?.open();
		},
		reset() {
			this.saving = false;
			this.values = { ...this.serverValues, powers: { ...this.serverValues.powers } };
		},
		async save() {
			this.saving = true;
			try {
				// errors are shown by the api client
				for (const url of this.requests) {
					await api.post(url);
				}
				(this.$refs["modal"] as InstanceType<typeof GenericModal> | undefined)?.close();
			} catch (e) {
				console.error(e);
			} finally {
				this.saving = false;
			}
		},
	},
});
</script>

<style scoped>
.input-width {
	width: 140px;
}
</style>
