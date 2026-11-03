#!/usr/bin/env python3
"""Test van register 1142 (BackUpSoc) op één Indevolt, ±25 minuten.

Vraag: kan evcc de reserve vasthouden door het minimum van een unit op te trekken, terwijl de unit
op zelfverbruik (47005=1, P1 volgen) blijft? Het script verandert de modus nooit, alleen 1142, en
zet 1142 op het einde, bij Ctrl-C en bij elke fout terug op de oorspronkelijke waarde.

Voorwaarden: auto laadt NIET, capacityBatteryMode staat niet op on (evcc mag blijven draaien in
off of shadow), het huis neemt iets af (een paar honderd watt volstaat). Zon is mooi meegenomen
voor vraag B4, maar niet nodig.

Fases:
  0  referentie, 3 min: oorspronkelijke 1142 (teruggelezen via 6105), lading, vermogen
  1  minimum = huidige lading, 6 min: stopt het ontladen meteen?
  2  minimum = lading + 10 %, 10 min: laadt de unit zelf van het net om het minimum te halen?
     Stopt meteen als ze 60 s meer dan 300 W van het net laadt (dat is dan het antwoord).
  3  minimum terug naar de oorspronkelijke waarde, 5 min: begint ze weer te ontladen?

Wat het beantwoordt (in RAPPORT-backupsoc-<datum>.md):
  B1  wordt 1142 aanvaard en klopt de teruggelezen waarde (6105)?
  B2  stopt het ontladen bij het minimum, in zelfverbruik?
  B3  laadt de unit zelf van het net als haar lading onder het minimum zit? (mag NIET)
  B4  laadt ze nog uit zonne-overschot met een opgetrokken minimum? (alleen als er overschot was)
  B5  hoe snel ontlaadt ze weer na het terugzetten?

Tekens: in de F0-logs van 2 november was het vermogen van 6000 negatief bij laden (net op 0 W terwijl
de zon de batterij vulde). Het script gaat daarvan uit; draai met --charge-positive als het andersom is.
Afname of overschot leidt het af uit net + unit: volgt de unit P1, dan staat het net zelf op ±0 W.

Gebruik:
  python3 f0_backupsoc_test.py -c /etc/f0/logger.json --unit zolder
  python3 f0_backupsoc_test.py -c ... --unit garage --fast   # fases van 1 min, alleen om de flow te testen
"""
from __future__ import annotations

import argparse
import csv
import json
import signal
import sys
import time
import urllib.request
from datetime import datetime
from pathlib import Path
from statistics import mean
from zoneinfo import ZoneInfo

sys.path.insert(0, str(Path(__file__).resolve().parent))
from f0_common import IDV_POWER, IDV_SOC, Indevolt, read_p1  # noqa: E402

TZ = ZoneInfo("Europe/Brussels")
IDV_BACKUP_SOC = 1142  # schrijven: minimum (back-up-SoC) in %
IDV_BACKUP_SOC_READ = 6105  # teruglezen van 1142

IDLE_W = 30  # onder dit vermogen doet de unit niets
GRID_CHARGE_W = 300  # zoveel laden zonder overschot = zelf van het net
GRID_CHARGE_S = 60


class Test:
    def __init__(self, unit: Indevolt, p1_host: str, out: Path, charge_negative: bool):
        self.unit, self.p1_host, self.charge_negative = unit, p1_host, charge_negative
        self.fh = open(out, "w", newline="")
        self.w = csv.writer(self.fh)
        self.w.writerow(["ts_local", "epoch", "phase", "p1_w", "unit_p_w", "unit_soc", "backup_soc", "errors"])
        self.phase = ""
        self.abort = False
        self.events: list[str] = []

    def discharge(self, p: float | None) -> float | None:
        """Vermogen met ontladen positief, laden negatief."""
        if p is None:
            return None
        return p if self.charge_negative else -p

    def sample(self) -> dict:
        now = time.time()
        p1 = read_p1(self.p1_host)
        s = {"t": now, "p1": p1.power_w, "p": None, "soc": None, "backup": None, "err": p1.error and f"p1:{p1.error}"}
        try:
            d, _ = self.unit.get(IDV_POWER, IDV_SOC, IDV_BACKUP_SOC_READ)
            s["p"] = self.discharge(_f(d.get(str(IDV_POWER))))
            s["soc"] = _f(d.get(str(IDV_SOC)))
            s["backup"] = _f(d.get(str(IDV_BACKUP_SOC_READ)))
        except Exception as e:  # noqa: BLE001
            s["err"] = ";".join(x for x in [s["err"], f"unit:{type(e).__name__}"] if x)
        self.w.writerow([datetime.fromtimestamp(now, TZ).isoformat(timespec="seconds"), round(now, 1), self.phase,
                         _fmt(s["p1"]), _fmt(s["p"]), _fmt(s["soc"]), _fmt(s["backup"]), s["err"] or ""])
        self.fh.flush()
        return s

    def log(self, what: str) -> None:
        line = f"[{datetime.now(TZ):%H:%M:%S}] {self.phase or '-'}: {what}"
        self.events.append(line)
        print(line, flush=True)

    def set_backup(self, soc: int) -> tuple[bool, object]:
        try:
            res, ms = self.unit.set(IDV_BACKUP_SOC, [soc])
            self.log(f"1142=[{soc}] → {res} ({ms:.0f} ms)")
            return True, res
        except Exception as e:  # noqa: BLE001
            self.log(f"1142=[{soc}] mislukt: {e}")
            return False, str(e)

    def run(self, name: str, seconds: int, grid_limit: float, stop_on_grid_charge: bool = False) -> list[dict]:
        self.phase = name
        print(f"\n== {name} ({seconds // 60} min) ==", flush=True)
        samples: list[dict] = []
        t_end, next_t, last_print = time.time() + seconds, time.time(), 0.0
        high_since = charge_since = None
        while time.time() < t_end and not self.abort:
            s = self.sample()
            samples.append(s)
            p1, p = s["p1"], s["p"]

            high_since = (high_since or s["t"]) if p1 is not None and p1 > grid_limit else None
            if high_since and s["t"] - high_since >= 30:
                self.log(f"fase gestopt: net 30 s boven {grid_limit:.0f} W")
                break

            # zonder overschot: wat het huis vraagt (net + unit) is niet negatief
            grid_charging = p is not None and p < -GRID_CHARGE_W and p1 is not None and p1 + p > -50
            charge_since = (charge_since or s["t"]) if grid_charging else None
            if stop_on_grid_charge and charge_since and s["t"] - charge_since >= GRID_CHARGE_S:
                self.log(f"fase gestopt: de unit laadt {GRID_CHARGE_S} s lang meer dan {GRID_CHARGE_W} W zonder zonne-overschot")
                break

            if time.time() - last_print >= 30:
                last_print = time.time()
                print(f"  {int(t_end - time.time()):4d} s te gaan | net {_fmt(p1)} W | unit {_fmt(p)} W "
                      f"(+ ontladen) | lading {_fmt(s['soc'])} % | minimum {_fmt(s['backup'])} %", flush=True)
            next_t += 1.0
            time.sleep(max(0.0, next_t - time.time()))
        return samples


def demand(s: dict) -> float:
    """Wat het huis vraagt na de zon: net plus wat de unit levert. Volgt de unit P1, dan staat het net
    op ±0 W, en zegt alleen deze som of er afname of overschot is."""
    return s["p1"] + s["p"]


def summarize(samples: list[dict], skip_s: float = 30.0) -> dict:
    """Gemiddelden na de eerste skip_s seconden, apart voor momenten met afname en met overschot."""
    if not samples:
        return {"n": 0}
    t0 = samples[0]["t"]
    rest = [s for s in samples if s["t"] - t0 >= skip_s and s["p"] is not None and s["p1"] is not None]
    imp = [s for s in rest if demand(s) > 100]
    exp = [s for s in rest if demand(s) < -100]
    return {
        "n": len(rest),
        "import_s": len(imp),
        "export_s": len(exp),
        "discharge_import_w": _avg([s["p"] for s in imp]),
        "charge_export_w": _avg([-s["p"] for s in exp]),
        "grid_charge_s": sum(1 for s in rest if s["p"] < -GRID_CHARGE_W and demand(s) > -50),
        "max_charge_w": max((-s["p"] for s in rest), default=None),
        "soc": (rest[0]["soc"], rest[-1]["soc"]) if rest else (None, None),
        "backup": sorted({s["backup"] for s in rest if s["backup"] is not None}),
    }


def first_discharge(samples: list[dict], threshold: float = 100.0) -> float | None:
    """Seconden tot de unit voor het eerst meer dan threshold W ontlaadt terwijl het huis afneemt."""
    if not samples:
        return None
    t0 = samples[0]["t"]
    for s in samples:
        if s["p"] is not None and s["p1"] is not None and s["p"] > threshold:
            return round(s["t"] - t0)
    return None


def capacity_battery_mode(evcc_url: str) -> str | None:
    try:
        with urllib.request.urlopen(evcc_url.rstrip("/") + "/api/state", timeout=3) as r:
            st = json.load(r)
    except Exception:  # noqa: BLE001
        return None
    st = st.get("result", st)
    return st.get("capacityBatteryMode")


def write_report(path: Path, unit: str, orig: int, set_ok: bool, ph: dict, release_s: float | None, events: list[str]) -> None:
    p0, p1, p2 = ph.get("0", {}), ph.get("1", {}), ph.get("2", {})
    L = [f"# Test register 1142 (BackUpSoc) — {unit}, {datetime.now(TZ):%d-%m-%Y %H:%M}", "",
         f"Oorspronkelijk minimum (6105): {orig} %. Ontladen is positief, laden negatief.", "",
         "| Fase | s | met afname | met overschot | ontladen bij afname | laden bij overschot | zelf van het net | lading | minimum |",
         "|---|---|---|---|---|---|---|---|---|"]
    for k, name in (("0", "0 referentie"), ("1", "1 minimum = lading"), ("2", "2 minimum = lading + 10 %"), ("3", "3 terug")):
        s = ph.get(k)
        if not s or not s.get("n"):
            L.append(f"| {name} | 0 | | | | | | | |")
            continue
        L.append(f"| {name} | {s['n']} | {s['import_s']} | {s['export_s']} | {_fmt(s['discharge_import_w'])} W | "
                 f"{_fmt(s['charge_export_w'])} W | {s['grid_charge_s']} s | {_fmt(s['soc'][0])} → {_fmt(s['soc'][1])} % | "
                 f"{', '.join(_fmt(x) for x in s['backup']) or '?'} % |")
    L += ["", "## Antwoorden", ""]

    b1 = set_ok and p1.get("backup") and all(abs(x - p1["backup"][0]) < 0.5 for x in p1["backup"])
    L.append(f"- **B1 aanvaard en teruggelezen**: {'ja' if b1 else 'nee of onzeker'} (6105 in fase 1: {p1.get('backup')})")

    base, held = p0.get("discharge_import_w"), p1.get("discharge_import_w")
    if base is None or held is None or base < 100:
        L.append("- **B2 ontladen stopt bij het minimum**: niet te beoordelen (de unit ontlaadde in de referentie niet, of er was geen afname). "
                 "Opnieuw met meer lading in de batterij.")
    else:
        L.append(f"- **B2 ontladen stopt bij het minimum**: {'ja' if held < IDLE_W else 'nee'} "
                 f"(referentie {base:.0f} W, met minimum = lading {held:.0f} W)")

    gc = p2.get("grid_charge_s", 0)
    L.append(f"- **B3 laadt zelf van het net onder het minimum**: {'JA, ' + str(gc) + ' s, max ' + _fmt(p2.get('max_charge_w')) + ' W' if gc else 'nee'}"
             + (" — dan is 1142 alleen bruikbaar als evcc dat in het kwartierbudget meerekent" if gc else ""))

    exp_s = (p1.get("export_s") or 0) + (p2.get("export_s") or 0)
    if exp_s < 60:
        L.append("- **B4 laadt nog uit zonne-overschot**: niet te beoordelen (minder dan 1 minuut overschot in fase 1 en 2)")
    else:
        ch = _avg([x for x in (p1.get("charge_export_w"), p2.get("charge_export_w")) if x is not None])
        L.append(f"- **B4 laadt nog uit zonne-overschot**: {'ja' if ch and ch > 100 else 'nee'} ({_fmt(ch)} W gemiddeld bij overschot, {exp_s} s)")

    L.append("- **B5 weer ontladen na terugzetten**: " + (f"na {release_s:.0f} s" if release_s is not None else "niet gezien in fase 3 (geen afname, of de lading zit op het oorspronkelijke minimum)"))
    L += ["", "## Gebeurtenissen", "", *[f"- {e}" for e in events], ""]
    path.write_text("\n".join(L))


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("-c", "--config", required=True, help="dezelfde JSON als het logscript (hosts, wachtwoorden)")
    ap.add_argument("--unit", choices=["garage", "zolder"], required=True)
    ap.add_argument("--restore", type=int, help="minimum om terug te zetten als 6105 niet te lezen is (bv. 10)")
    ap.add_argument("--charge-positive", action="store_true", help="vermogen van 6000 is positief bij laden")
    ap.add_argument("--grid-limit", type=float, default=2400, help="stop een fase als het net 30 s boven deze waarde zit (W)")
    ap.add_argument("--fast", action="store_true", help="fases van 1 min, alleen om de flow te testen")
    ap.add_argument("--out-dir", default="/var/log/f0")
    args = ap.parse_args()

    cfg = json.load(open(args.config))
    mode = capacity_battery_mode(cfg.get("evcc_url", "http://localhost:7070"))
    if mode == "on":
        sys.exit("capacityBatteryMode staat op on: zet de batterijsturing eerst op shadow of off.")
    ans = input(f"Laadt de auto NIET, en neemt het huis iets af? (evcc-batterijsturing: {mode or 'niet bereikbaar'}) [j/N] ")
    if ans.strip().lower() not in ("j", "ja", "y"):
        sys.exit("Afgebroken.")

    u = next(x for x in cfg["indevolt"] if x["name"] == args.unit)
    unit = Indevolt(u["name"], u["host"], u.get("user") or None, u.get("password") or None)

    try:
        d, _ = unit.get(IDV_BACKUP_SOC_READ, IDV_SOC)
        orig, soc = _f(d.get(str(IDV_BACKUP_SOC_READ))), _f(d.get(str(IDV_SOC)))
    except Exception as e:  # noqa: BLE001
        sys.exit(f"{args.unit} niet bereikbaar: {e}")
    if orig is None:
        if args.restore is None:
            sys.exit(f"6105 is niet te lezen ({d}). Geef het minimum uit de Indevolt-app mee met --restore.")
        orig = args.restore
    if soc is None:
        sys.exit(f"lading (6002) niet te lezen: {d}")
    orig, soc = int(round(orig)), int(round(soc))
    print(f"{args.unit}: lading {soc} %, minimum {orig} %", flush=True)

    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(TZ).strftime("%Y-%m-%d_%H%M")
    csv_path = out_dir / f"backupsoc-{args.unit}-{stamp}.csv"
    rep_path = out_dir / f"RAPPORT-backupsoc-{args.unit}-{stamp}.md"
    t = Test(unit, cfg["p1_host"], csv_path, charge_negative=not args.charge_positive)

    def on_signal(*_):
        t.abort = True
        print("\n!! afgebroken, minimum terug naar de oorspronkelijke waarde", flush=True)
    signal.signal(signal.SIGINT, on_signal)
    signal.signal(signal.SIGTERM, on_signal)

    dur = {"0": 180, "1": 360, "2": 600, "3": 300}
    if args.fast:
        dur = {k: 60 for k in dur}

    phases: dict[str, dict] = {}
    set_ok, release_s = False, None
    try:
        phases["0"] = summarize(t.run("0 referentie", dur["0"], args.grid_limit))
        if not t.abort:
            t.phase = "1 minimum = lading"
            set_ok, _ = t.set_backup(min(100, max(soc, orig)))
            phases["1"] = summarize(t.run("1 minimum = lading", dur["1"], args.grid_limit))
        if not t.abort and set_ok:
            t.phase = "2 minimum = lading + 10 %"
            t.set_backup(min(100, max(soc, orig) + 10))
            phases["2"] = summarize(t.run("2 minimum = lading + 10 %", dur["2"], args.grid_limit, stop_on_grid_charge=True))
        if not t.abort and set_ok:
            t.phase = "3 terug"
            t.set_backup(orig)
            s3 = t.run("3 terug", dur["3"], args.grid_limit)
            phases["3"] = summarize(s3)
            release_s = first_discharge(s3)
    finally:
        t.phase = "einde"
        for attempt in range(3):
            ok, _ = t.set_backup(orig)
            if ok:
                break
            time.sleep(3)
        else:
            print(f"!!!! minimum van {args.unit} NIET teruggezet. Zet het in de Indevolt-app op {orig} % !!!!", flush=True)
        t.fh.close()
        write_report(rep_path, args.unit, orig, set_ok, phases, release_s, t.events)
        print(f"\nRapport: {rep_path}\nRuwe data: {csv_path}", flush=True)


def _f(v):
    try:
        return None if v is None else float(v)
    except (TypeError, ValueError):
        return None


def _avg(xs: list) -> float | None:
    xs = [x for x in xs if x is not None]
    return mean(xs) if xs else None


def _fmt(v) -> str:
    if v is None:
        return ""
    return f"{v:.0f}" if isinstance(v, float) else str(v)


if __name__ == "__main__":
    main()
