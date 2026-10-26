#!/usr/bin/env python3
"""F0-logscript: elke seconde één regel met P1, Alfen en beide Indevolts, in een CSV per dag.

Draait als systemd-service in de evcc-LXC (zie README.md). Geen externe pakketten nodig.

Kolommen (één regel per seconde, leeg = niet gelezen in die seconde):
  ts_local            lokale tijd met zone-offset (Europe/Brussels), bv. 2026-10-25T02:30:00+01:00
  epoch               Unix-tijd (seconden); ondubbelzinnig, ook in het dubbele uur van 25 oktober
  p1_power_w          netvermogen (+ afname, − injectie)
  p1_import_kwh       totaal afname-register (dag + nacht)
  p1_export_kwh       totaal injectie-register
  p1_avg15_w          lopend kwartiergemiddelde van de meter (OBIS 1.4.0) — F0 moet de semantiek bevestigen
  p1_monthpeak_w      maandpiek van de meter (OBIS 1.6.0) — dit is wat Fluvius aanrekent
  p1_monthpeak_ts     tijdstip van die maandpiek (zoals de meter het geeft)
  p1_ms               leestijd in ms
  alfen_i_a           laadstroom fase 1 (A)
  alfen_p_w           laadvermogen (W)
  alfen_status        bron evcc: A = geen auto, B = auto verbonden, C = laden; bron modbus: Mode-3-status (A1, B1, C2, ...)
  alfen_setpoint_a    bron evcc: stroom die evcc aanbiedt (offeredCurrent); bron modbus: Modbus-setpoint
  alfen_applied_a     werkelijk toegepast maximum (alleen bron modbus, bij evcc leeg)
  alfen_valid_s       resterende geldigheid van het setpoint (s), review B1.4 (alleen bron modbus, bij evcc leeg)
  alfen_safe_a        safe current bij uitblijvende commando's (alleen bron modbus, bij evcc leeg)
  alfen_energy_wh     totaalteller laadpunt
  alfen_ms            leestijd in ms (bij bron evcc alleen op rijen waar evcc echt bevraagd is)

De Alfen-kolommen komen standaard uit evcc (/api/state), want de Alfen beantwoordt een eigen Modbus-verbinding
van het logscript niet binnen 0,9 s (gemeten op 25 oktober). Zo opent het logscript geen extra verbinding naar de
Alfen. evcc ververst zijn waarden per regelcyclus (standaard 10 s), dus de Alfen-kolommen veranderen niet elke seconde.
Terug naar eigen Modbus-lezing: "alfen_source": "modbus" in de config.
  <unit>_p_w          batterijvermogen (template: + laden, − ontladen; F0 controleert het teken)
  <unit>_soc          lading (%)
  <unit>_mode         register 47005 als het leesbaar is (anders leeg)
  <unit>_ms           leestijd in ms
  extra_*             optionele HA-sensoren (bv. Huawei PV-vermogen), zie config
  errors              fouten per bron; "<bron>:late" = antwoord kwam niet binnen 0,9 s (cellen leeg),
                      "<bron>:busy" = vorige lezing van die bron liep nog, deze seconde overgeslagen
"""
from __future__ import annotations

import argparse
import csv
import gzip
import json
import shutil
import signal
import sys
import threading
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from concurrent.futures import TimeoutError as FutureTimeout
from datetime import datetime
from pathlib import Path
from zoneinfo import ZoneInfo

sys.path.insert(0, str(Path(__file__).resolve().parent))
from f0_common import (  # noqa: E402
	IDV_MODE, IDV_POWER, IDV_SOC, AlfenSample, Indevolt, ModbusTcp, P1Sample, fmt, read_alfen, read_p1,
)

TZ = ZoneInfo("Europe/Brussels")

DEFAULT_CONFIG = {
	"log_dir": "/var/log/f0",
	"interval_s": 1.0,
	"p1_host": "192.168.1.50",
	"alfen_host": "192.168.1.51",
	"alfen_extended": True,
	"alfen_source": "evcc",
	"evcc_url": "http://localhost:7070",
	"evcc_loadpoint": 0,
	"evcc_poll_s": 2.0,
	"indevolt": [
		{"name": "garage", "host": "192.168.1.52", "user": "", "password": ""},
		{"name": "zolder", "host": "192.168.1.53", "user": "", "password": ""},
	],
	"ha": {"url": "", "token": "", "entities": {}},
	"gzip_previous_day": True,
}


class Logger:
	def __init__(self, cfg: dict):
		self.cfg = cfg
		self.log_dir = Path(cfg["log_dir"])
		self.log_dir.mkdir(parents=True, exist_ok=True)
		self.mb = ModbusTcp(cfg["alfen_host"]) if cfg.get("alfen_source", "evcc") == "modbus" else None
		self._evcc_cache: tuple[float, AlfenSample] | None = None
		self.units = [Indevolt(u["name"], u["host"], u.get("user") or None, u.get("password") or None, timeout=0.9)
					  for u in cfg["indevolt"]]
		self.unit_mode_readable: dict[str, bool] = {}
		self.ha = cfg.get("ha") or {}
		self.extra_keys = sorted((self.ha.get("entities") or {}).keys())
		self.stop = threading.Event()
		self.current_day: str | None = None
		self.fh = None
		self.writer = None
		self.err_count: dict[str, int] = {}
		self.pool = ThreadPoolExecutor(max_workers=4 + len(self.units))
		self.inflight: dict = {}

	# --- kolommen ---------------------------------------------------------
	def header(self) -> list[str]:
		cols = ["ts_local", "epoch",
				"p1_power_w", "p1_import_kwh", "p1_export_kwh", "p1_avg15_w", "p1_monthpeak_w", "p1_monthpeak_ts", "p1_ms",
				"alfen_i_a", "alfen_p_w", "alfen_status", "alfen_setpoint_a", "alfen_applied_a", "alfen_valid_s",
				"alfen_safe_a", "alfen_energy_wh", "alfen_ms"]
		for u in self.units:
			cols += [f"{u.name}_p_w", f"{u.name}_soc", f"{u.name}_mode", f"{u.name}_ms"]
		cols += [f"extra_{k}" for k in self.extra_keys]
		cols += ["errors"]
		return cols

	# --- bestand per dag --------------------------------------------------
	def _open_for(self, now: datetime) -> None:
		day = now.strftime("%Y-%m-%d")
		if day == self.current_day:
			return
		if self.fh:
			self.fh.close()
			if self.cfg.get("gzip_previous_day") and self.current_day:
				self.pool.submit(self._gzip, self.log_dir / f"f0-{self.current_day}.csv")
		path = self.log_dir / f"f0-{day}.csv"
		new = not path.exists()
		self.fh = open(path, "a", newline="", buffering=1)
		self.writer = csv.writer(self.fh)
		if new:
			self.writer.writerow(self.header())
		self.current_day = day
		print(f"[f0] log naar {path}", flush=True)

	@staticmethod
	def _gzip(path: Path) -> None:
		try:
			with open(path, "rb") as src, gzip.open(str(path) + ".gz", "wb", compresslevel=6) as dst:
				shutil.copyfileobj(src, dst)
			path.unlink()
		except OSError as e:
			print(f"[f0] gzip van {path} mislukt: {e}", file=sys.stderr, flush=True)

	# --- bronnen ----------------------------------------------------------
	def _read_unit(self, u: Indevolt) -> dict:
		out = {"p": None, "soc": None, "mode": None, "ms": None, "err": None}
		try:
			points = [IDV_POWER, IDV_SOC]
			if self.unit_mode_readable.get(u.name, True):
				points.append(IDV_MODE)
			d, ms = u.get(*points)
			out["ms"] = round(ms)
			out["p"] = _f(d.get(str(IDV_POWER)))
			out["soc"] = _f(d.get(str(IDV_SOC)))
			mode = d.get(str(IDV_MODE))
			if u.name not in self.unit_mode_readable:
				self.unit_mode_readable[u.name] = mode is not None
				print(f"[f0] {u.name}: register 47005 {'leesbaar' if mode is not None else 'NIET leesbaar'} via GetData",
					  flush=True)
			out["mode"] = mode
		except Exception as e:  # noqa: BLE001
			out["err"] = f"{u.name}:{type(e).__name__}"
		return out

	def _read_ha(self) -> dict:
		out: dict = {}
		if not self.ha.get("url") or not self.extra_keys:
			return out
		hdr = {"Authorization": f"Bearer {self.ha['token']}", "Content-Type": "application/json"}
		for key in self.extra_keys:
			ent = self.ha["entities"][key]
			try:
				req = urllib.request.Request(f"{self.ha['url'].rstrip('/')}/api/states/{ent}", headers=hdr)
				with urllib.request.urlopen(req, timeout=0.8) as r:
					out[key] = _f(json.load(r).get("state"))
			except Exception:  # noqa: BLE001
				out[key] = None
		return out

	def _read_alfen(self) -> tuple[AlfenSample, int | None]:
		"""(monster, leestijd in ms). Bron: evcc (standaard) of een eigen Modbus-verbinding ("alfen_source": "modbus")."""
		if self.mb is not None:
			return _timed(read_alfen, self.mb, self.cfg.get("alfen_extended", True))
		now = time.time()
		cached = self._evcc_cache
		if cached and now - cached[0] < float(self.cfg.get("evcc_poll_s", 2.0)):
			return cached[1], None
		t0 = time.perf_counter()
		try:
			req = urllib.request.Request(f"{self.cfg['evcc_url'].rstrip('/')}/api/state")
			with urllib.request.urlopen(req, timeout=0.8) as r:
				state = json.load(r)
			sample = self._alfen_from_evcc(state)
		except Exception as e:  # noqa: BLE001
			sample = AlfenSample(error=f"{type(e).__name__}: {e}"[:80])
		ms = round((time.perf_counter() - t0) * 1000)
		self._evcc_cache = (now, sample) if sample.error is None else None
		return sample, ms

	def _alfen_from_evcc(self, state: dict) -> AlfenSample:
		"""Haalt de laadpuntwaarden uit /api/state van evcc (met of zonder "result"-omhulsel)."""
		st = state.get("result", state) if isinstance(state, dict) else {}
		lps = st.get("loadpoints") or []
		idx = int(self.cfg.get("evcc_loadpoint", 0))
		if idx >= len(lps):
			return AlfenSample(error="geen loadpoint in evcc-state")
		lp = lps[idx]
		cur = lp.get("chargeCurrents")
		kwh = _f(lp.get("chargeTotalImport"))
		s = AlfenSample()
		s.power_w = _f(lp.get("chargePower"))
		s.current_a = _f(cur[0]) if isinstance(cur, list) and cur else _f(lp.get("chargeCurrent"))
		if "connected" in lp:
			s.status = "C" if lp.get("charging") else ("B" if lp.get("connected") else "A")
		s.setpoint_a = _f(lp.get("offeredCurrent"))
		s.energy_wh = None if kwh is None else kwh * 1000
		return s

	# --- hoofdlus -------------------------------------------------------------
	def run(self) -> None:
		interval = float(self.cfg["interval_s"])
		next_tick = (time.time() // interval + 1) * interval
		while not self.stop.is_set():
			now = time.time()
			if now < next_tick:
				self.stop.wait(next_tick - now)
				if self.stop.is_set():
					break
			tick = next_tick
			next_tick += interval
			if time.time() - tick > 5 * interval:   # grote achterstand (bv. slaap): opnieuw uitlijnen
				next_tick = (time.time() // interval + 1) * interval

			# Elke bron krijgt tot 0,9 × interval na de tick. Wie te laat is, krijgt een lege cel en "<bron>:late";
			# een bron waarvan de vorige lezing nog loopt, wordt deze seconde overgeslagen ("<bron>:busy").
			# Zo houdt één trage bron (meestal de Alfen) de andere niet meer op.
			deadline = tick + 0.9 * interval
			late: list[str] = []
			jobs = {"p1": self._start("p1", _timed, read_p1, self.cfg["p1_host"]),
					"alfen": self._start("alfen", self._read_alfen),
					"ha": self._start("ha", self._read_ha)}
			for u in self.units:
				jobs[u.name] = self._start(u.name, self._read_unit, u)
			p1, p1_ms = self._wait("p1", jobs, deadline, late, (P1Sample(), None))
			alf, alf_ms = self._wait("alfen", jobs, deadline, late, (AlfenSample(), None))
			units = [self._wait(u.name, jobs, deadline, late,
								{"p": None, "soc": None, "mode": None, "ms": None, "err": None}) for u in self.units]
			extra = self._wait("ha", jobs, deadline, late, {})

			dt = datetime.fromtimestamp(tick, TZ)
			self._open_for(dt)
			errors = [e for e in [p1.error and f"p1:{p1.error}", alf.error and f"alfen:{alf.error}"] + [u["err"] for u in units] if e] + late
			row = [dt.isoformat(timespec="seconds"), int(tick),
				   fmt(p1.power_w), fmt(p1.import_kwh), fmt(p1.export_kwh), fmt(p1.avg15_w), fmt(p1.monthpeak_w),
				   fmt(p1.monthpeak_ts), fmt(p1_ms),
				   fmt(alf.current_a), fmt(alf.power_w), fmt(alf.status), fmt(alf.setpoint_a), fmt(alf.applied_a),
				   fmt(alf.valid_s), fmt(alf.safe_a), fmt(alf.energy_wh), fmt(alf_ms)]
			for u in units:
				row += [fmt(u["p"]), fmt(u["soc"]), fmt(u["mode"]), fmt(u["ms"])]
			row += [fmt(extra.get(k)) for k in self.extra_keys]
			row.append(";".join(errors))
			self.writer.writerow(row)
			self._count_errors(errors)

		if self.fh:
			self.fh.close()
		if self.mb is not None:
			self.mb.close()
		self.pool.shutdown(wait=False)

	def _start(self, src: str, fn, *args):
		"""Start een lezing in de pool, tenzij de vorige lezing van deze bron nog loopt (dan None)."""
		fut = self.inflight.get(src)
		if fut is not None and not fut.done():
			return None
		fut = self.pool.submit(fn, *args)
		self.inflight[src] = fut
		return fut

	@staticmethod
	def _wait(src: str, jobs: dict, deadline: float, late: list[str], empty):
		"""Wacht hoogstens tot deadline op de lezing van src; anders `empty` en een markering in errors."""
		fut = jobs.get(src)
		if fut is None:
			late.append(f"{src}:busy")
			return empty
		try:
			return fut.result(timeout=max(0.0, deadline - time.time()))
		except FutureTimeout:
			late.append(f"{src}:late")
			return empty

	def _count_errors(self, errors: list[str]) -> None:
		"""Meldt op stderr als een bron 60 s na elkaar faalt (één keer per uitval)."""
		for src in ("p1", "alfen", *[u.name for u in self.units]):
			failing = any(e.startswith(src + ":") for e in errors)
			n = self.err_count.get(src, 0)
			self.err_count[src] = n + 1 if failing else 0
			if failing and self.err_count[src] == 60:
				print(f"[f0] {src}: al 60 s geen meting ({[e for e in errors if e.startswith(src)][0]})", file=sys.stderr, flush=True)
			if not failing and n >= 60:
				print(f"[f0] {src}: meting hersteld na {n} s", file=sys.stderr, flush=True)


def _timed(fn, *args):
	t0 = time.perf_counter()
	res = fn(*args)
	return res, round((time.perf_counter() - t0) * 1000)


def _f(v):
	try:
		return None if v is None else float(v)
	except (TypeError, ValueError):
		return None


def load_config(path: str | None) -> dict:
	cfg = json.loads(json.dumps(DEFAULT_CONFIG))
	if path:
		with open(path) as f:
			user = json.load(f)
		cfg.update({k: v for k, v in user.items() if k != "ha"})
		if "ha" in user:
			cfg["ha"].update(user["ha"])
	return cfg


def main() -> None:
	ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	ap.add_argument("-c", "--config", help="JSON-config (zie f0_logger.example.json)")
	ap.add_argument("--print-config", action="store_true", help="toon de standaardconfig en stop")
	ap.add_argument("--once", action="store_true", help="lees alles één keer, toon het en stop (verbindingstest)")
	args = ap.parse_args()
	if args.print_config:
		print(json.dumps(DEFAULT_CONFIG, indent=2))
		return
	cfg = load_config(args.config)
	lg = Logger(cfg)
	if args.once:
		p1 = read_p1(cfg["p1_host"])
		alf, alf_ms = lg._read_alfen()
		print("P1    :", p1)
		print("Alfen :", alf, f"({alf_ms} ms, bron {cfg.get('alfen_source', 'evcc')})")
		for u in lg.units:
			print(f"{u.name:7}:", lg._read_unit(u))
		print("HA    :", lg._read_ha())
		return
	for sig in (signal.SIGINT, signal.SIGTERM):
		signal.signal(sig, lambda *_: lg.stop.set())
	print(f"[f0] start, interval {cfg['interval_s']} s, map {cfg['log_dir']}", flush=True)
	lg.run()
	print("[f0] gestopt", flush=True)


if __name__ == "__main__":
	main()
