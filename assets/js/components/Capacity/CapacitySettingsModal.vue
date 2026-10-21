<template>
	<GenericModal
		id="capacitySettingsModal"
		ref="modal"
		:title="$t('capacity.settings.title')"
		data-testid="capacity-settings-modal"
		@open="reset"
	>
		<p>{{ $t("capacity.settings.description") }}</p>
		<form class="container mx-0 px-0" @submit.prevent="save">
			<FormRow
				id="capacityMode"
				:label="$t('capacity.settings.mode')"
				:help="$t(`capacity.settings.modeHelp.${values.mode}`)"
			>
				<SelectGroup
					id="capacityMode"
					v-model="values.mode"
					:options="modeOptions"
					equal-width
					transparent
				/>
			</FormRow>

			<FormRow
				id="capacityFloor"
				:label="$t('capacity.settings.floor')"
				:help="
					$t('capacity.settings.floorHelp', {
						min: fmtW(FLOOR_MIN, POWER_UNIT.KW),
						max: fmtW(FLOOR_MAX, POWER_UNIT.KW, true, 0),
					})
				"
			>
				<div class="input-group input-width">
					<input
						id="capacityFloor"
						v-model.number="values.floor"
						type="number"
						:min="FLOOR_MIN / 1000"
						:max="FLOOR_MAX / 1000"
						step="0.05"
						required
						aria-describedby="capacityFloorUnit"
						class="form-control text-end"
					/>
					<span id="capacityFloorUnit" class="input-group-text">kW</span>
				</div>
			</FormRow>

			<FormRow
				id="capacityMargin"
				:label="$t('capacity.settings.margin')"
				:help="
					$t('capacity.settings.marginHelp', {
						min: fmtW(MARGIN_MIN, POWER_UNIT.W),
						max: fmtW(MARGIN_MAX, POWER_UNIT.W),
					})
				"
			>
				<div class="input-group input-width">
					<input
						id="capacityMargin"
						v-model.number="values.margin"
						type="number"
						:min="MARGIN_MIN"
						:max="MARGIN_MAX"
						step="10"
						required
						aria-describedby="capacityMarginUnit"
						class="form-control text-end"
					/>
					<span id="capacityMarginUnit" class="input-group-text">W</span>
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
					data-testid="capacity-settings-save"
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
import type { CapacityMode } from "@/types/evcc";
import GenericModal from "../Helper/GenericModal.vue";
import SelectGroup from "../Helper/SelectGroup.vue";
import FormRow from "../Config/FormRow.vue";
import {
	CAPACITY_MODES,
	FLOOR_MAX,
	FLOOR_MIN,
	MARGIN_MAX,
	MARGIN_MIN,
	capacitySettingsRequests,
	type CapacitySettings,
} from "./capacity";

// capacity tariff mode, lowest target peak and margin
export default defineComponent({
	name: "CapacitySettingsModal",
	components: { GenericModal, SelectGroup, FormRow },
	mixins: [formatter],
	props: {
		capacityMode: String as PropType<CapacityMode>,
		capacityFloor: Number,
		capacityMargin: Number,
	},
	data() {
		return {
			saving: false,
			values: { mode: "off", floor: FLOOR_MIN / 1000, margin: 0 } as CapacitySettings,
			FLOOR_MIN,
			FLOOR_MAX,
			MARGIN_MIN,
			MARGIN_MAX,
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
		serverValues(): CapacitySettings {
			return {
				mode: this.capacityMode ?? "off",
				floor: (this.capacityFloor ?? FLOOR_MIN) / 1000,
				margin: this.capacityMargin ?? 0,
			};
		},
		requests(): string[] {
			return capacitySettingsRequests(this.serverValues, this.values);
		},
		nothingChanged(): boolean {
			return this.requests.length === 0;
		},
	},
	methods: {
		open() {
			(this.$refs["modal"] as InstanceType<typeof GenericModal>).open();
		},
		reset() {
			this.saving = false;
			this.values = { ...this.serverValues };
		},
		async save() {
			this.saving = true;
			try {
				// errors are shown by the api client
				for (const url of this.requests) {
					await api.post(url);
				}
				(this.$refs["modal"] as InstanceType<typeof GenericModal>).close();
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
