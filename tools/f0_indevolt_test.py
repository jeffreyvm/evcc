#!/usr/bin/env python3
"""F0-testscript voor de Indevolts: hold, charge, ontladen op vast vermogen, "stil" in real-time control,
moduslatentie en een staptest met beide units in zelfverbruik.

Draaien op een zonnige middag, auto NIET aan het laden, evcc GESTOPT (systemctl stop evcc) en de
batterij-automations van capaciteitstarief v8 in HA UIT. Het script zet beide units op het einde,
bij Ctrl-C en bij elke fout terug op normal (zelfverbruik).

Wat het beantwoordt (per unit, in RAPPORT.md):
  V1  laadt hold (47015=[0,50,minsoc]) nog uit zonne-overschot, of staat de unit echt stil?
  V2  bestaat ontladen op vast vermogen via 47015 (status 2 of 3)? zo ja: injecteert het?
  V3  klopt het teken van 6000 op de PV-variant (+ laden, − ontladen, zoals het template zegt)?
  V4  halen de units een leesinterval van 1 s, en wat is de HTTP-latentie van lezen en schrijven?
  V5  hoe lang duurt een moduswissel tot het vermogen volgt (dode tijd)?
  V6  wat doet een unit die in real-time control staat en daarna geen commando's meer krijgt?
  V7  zijn 47005/47015/1143 terug te lezen (dan kan de waakhond op de modus kijken i.p.v. op gedrag)?
  V8  regelen beide units in normal tegen elkaar (doorschieten, pendelen) bij een last van ±2 kW?
  V9  laadrendement AC→SoC tijdens charge (ruw: ΔSoC × capaciteit / geladen energie).

Gebruik:
  python3 f0_indevolt_test.py -c /etc/f0/logger.json                 # alles, beide units
  python3 f0_indevolt_test.py -c ... --unit zolder --skip step idle  # alleen de zolder, zonder staptest en stil-test
  python3 f0_indevolt_test.py -c ... --fast                           # korte fases (2 min) om de flow te testen
"""
from __future__ import annotations

import argparse
import csv
import json
import signal
import subprocess
import sys
import time
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from statistics import mean, median
from zoneinfo import ZoneInfo

sys.path.insert(0, str(Path(__file__).resolve().parent))
from f0_common import IDV_GRIDCHARGE, IDV_MODE, IDV_POWER, IDV_RTC, IDV_SOC, Indevolt, read_p1  # noqa: E402

TZ = ZoneInfo("Europe/Brussels")
UNIT_SPECS = {"garage": {"power_w": 800, "kwh": 4.0}, "zolder": {"power_w": 1200, "kwh": 6.0}}


@dataclass
class Ctx:
    units: dict[str, Indevolt]
    p1_host: str
    minsoc: int
    maxsoc: int
    out: Path
    rows: csv.writer
    rows_fh: object
    events: list[dict] = field(default_factory=list)
    readable: dict[str, bool] = field(default_factory=dict)
    phase: str = ""
    abort: bool = False


# ---------------------------------------------------------------------------
# meten en loggen
# ---------------------------------------------------------------------------

def sample(ctx: Ctx, unit_names: list[str]) -> dict:
    """Eén meting van P1 en de gevraagde units. Logt een CSV-regel. Geeft dict met waarden en latenties."""
    now = time.time()
    p1 = read_p1(ctx.p1_host)
    s = {"t": now, "p1": p1.power_w, "p1_err": p1.error}
    for n in unit_names:
        u = ctx.units[n]
        try:
            pts = [IDV_POWER, IDV_SOC] + ([IDV_MODE] if ctx.readable.get(n) else [])
            d, ms = u.get(*pts)
            s[n] = {"p": _f(d.get(str(IDV_POWER))), "soc": _f(d.get(str(IDV_SOC))), "mode": d.get(str(IDV_MODE)), "ms": ms, "err": None}
        except Exception as e:  # noqa: BLE001
            s[n] = {"p": None, "soc": None, "mode": None, "ms": None, "err": type(e).__name__}
    row = [datetime.fromtimestamp(now, TZ).isoformat(timespec="seconds"), round(now, 1), ctx.phase, _fmt(s["p1"])]
    for n in ("garage", "zolder"):
        v = s.get(n, {})
        row += [_fmt(v.get("p")), _fmt(v.get("soc")), _fmt(v.get("mode")), _fmt(v.get("ms") and round(v["ms"]))]
    row.append(";".join(e for e in [s["p1_err"] and f"p1:{s['p1_err']}"] + [f"{n}:{s[n]['err']}" for n in unit_names if s[n]["err"]] if e))
    ctx.rows.writerow(row)
    ctx.rows_fh.flush()
    return s


def log_event(ctx: Ctx, unit: str, what: str, result=None, ms: float | None = None) -> None:
    ev = {"ts": datetime.now(TZ).isoformat(timespec="seconds"), "phase": ctx.phase, "unit": unit, "event": what,
          "result": result, "ms": None if ms is None else round(ms)}
    ctx.events.append(ev)
    print(f"  [{ev['ts'][11:19]}] {unit:7} {what}" + (f" → {result} ({ev['ms']} ms)" if result is not None else ""), flush=True)


def send(ctx: Ctx, unit: str, calls: list[tuple[str, object, float]]) -> float:
    """Logt de SetData-calls van één moduswissel; geeft het tijdstip van de laatste call terug."""
    for what, result, ms in calls:
        log_event(ctx, unit, what, result, ms)
    return time.time()


def run_phase(ctx: Ctx, name: str, units: list[str], seconds: int, guard=None, every_s: float = 1.0) -> list[dict]:
    """Meet `seconds` lang elke seconde. `guard(sample, samples)` mag een reden teruggeven om te stoppen."""
    ctx.phase = name
    print(f"\n== {name} ({seconds // 60} min {seconds % 60} s) ==", flush=True)
    samples: list[dict] = []
    t_end = time.time() + seconds
    next_t = time.time()
    last_print = 0.0
    while time.time() < t_end and not ctx.abort:
        s = sample(ctx, units)
        samples.append(s)
        if guard:
            reason = guard(s, samples)
            if reason:
                log_event(ctx, ",".join(units), f"fase gestopt: {reason}")
                break
        if time.time() - last_print >= 30:
            last_print = time.time()
            txt = " ".join(f"{n}={_fmt(s[n]['p'])}W/{_fmt(s[n]['soc'])}%" for n in units)
            print(f"  {int(t_end - time.time()):4d} s te gaan | P1={_fmt(s['p1'])} W | {txt}", flush=True)
        next_t += every_s
        time.sleep(max(0.0, next_t - time.time()))
    return samples


# ---------------------------------------------------------------------------
# analyses
# ---------------------------------------------------------------------------

def latency(samples: list[dict], unit: str, t0: float, before_w: float | None, threshold: float = 100.0) -> float | None:
    """Seconden tussen het commando (t0) en de eerste meting waarin het vermogen minstens `threshold` W verschoven is."""
    if before_w is None:
        return None
    for s in samples:
        p = s[unit]["p"]
        if s["t"] >= t0 and p is not None and abs(p - before_w) >= threshold:
            return round(s["t"] - t0, 1)
    return None


def avg(samples: list[dict], key, skip_s: float = 60.0) -> float | None:
    """Gemiddelde over de fase, zonder de eerste `skip_s` seconden (inschakelgedrag)."""
    if not samples:
        return None
    skip_s = min(skip_s, (samples[-1]["t"] - samples[0]["t"]) / 4)  # korte fases: hoogstens een kwart overslaan
    t0 = samples[0]["t"] + skip_s
    vals = [key(s) for s in samples if s["t"] >= t0 and key(s) is not None]
    return round(mean(vals), 0) if vals else None


def oscillation(samples: list[dict], unit: str, amp_w: float = 200.0) -> int:
    """Telt richtingswisselingen van het batterijvermogen met een slag van minstens amp_w (pendelen)."""
    vals = [s[unit]["p"] for s in samples if s[unit]["p"] is not None]
    n, last_dir, extreme = 0, 0, None
    for v in vals:
        if extreme is None:
            extreme = v
            continue
        d = 1 if v > extreme else -1 if v < extreme else 0
        if d and d != last_dir and abs(v - extreme) >= amp_w:
            if last_dir:
                n += 1
            last_dir = d
            extreme = v
        elif d == last_dir:
            extreme = v
    return n


# ---------------------------------------------------------------------------
# testfases
# ---------------------------------------------------------------------------

def guard_grid(limit_w: float, hold_s: int = 30):
    """Stop als het netvermogen `hold_s` seconden boven limit_w blijft (we willen zelf geen piek zetten)."""
    def g(s, samples):
        recent = [x["p1"] for x in samples[-hold_s:] if x["p1"] is not None]
        if len(recent) >= hold_s and min(recent) > limit_w:
            return f"netafname {hold_s} s boven {limit_w:.0f} W"
        return None
    return g


def test_unit(ctx: Ctx, name: str, args, report: dict) -> None:
    u = ctx.units[name]
    spec = UNIT_SPECS[name]
    r = report.setdefault(name, {})
    mins = (lambda m: int(__import__("os").environ.get("F0_FAST_S", 120)) if args.fast else m * 60)

    # V7: terugleesbaarheid van de stuurregisters
    try:
        d, ms = u.get(IDV_MODE, IDV_RTC, IDV_GRIDCHARGE, IDV_POWER, IDV_SOC)
        ctx.readable[name] = d.get(str(IDV_MODE)) is not None
        r["readback"] = {k: d.get(str(k)) for k in (IDV_MODE, IDV_RTC, IDV_GRIDCHARGE)}
        r["read_ms"] = round(ms)
        log_event(ctx, name, "V7 terugleesbaar 47005/47015/1143", r["readback"], ms)
        soc0 = _f(d.get(str(IDV_SOC)))
    except Exception as e:  # noqa: BLE001
        print(f"!! {name} onbereikbaar: {e}")
        r["error"] = str(e)
        return
    r["soc_start"] = soc0

    # V3/V4 + referentie: zelfverbruik
    send(ctx, name, u.mode_normal())
    base = run_phase(ctx, f"{name} baseline normal", [name], mins(3))
    r["baseline"] = {"p1_avg": avg(base, lambda s: s["p1"], 30), "bat_avg": avg(base, lambda s: s[name]["p"], 30)}
    r["read_latency_ms"] = _lat([s[name]["ms"] for s in base])
    r["read_errors"] = sum(1 for s in base if s[name]["err"])
    if r["baseline"]["p1_avg"] is not None and r["baseline"]["bat_avg"] is not None:
        p1, bat = r["baseline"]["p1_avg"], r["baseline"]["bat_avg"]
        if p1 > 200 and bat > 100:
            r["sign"] = "OMGEKEERD: bij netafname meldt 6000 een positief vermogen (laden) — teken in de fork omdraaien"
        elif p1 < -200 and bat < -100:
            r["sign"] = "OMGEKEERD: bij injectie meldt 6000 een negatief vermogen (ontladen)"
        elif abs(bat) < 60:
            r["sign"] = "niet te bepalen: batterij deed niets in de baseline (vol of leeg?)"
        else:
            r["sign"] = "klopt met het template (+ laden, − ontladen)"
    before = base[-1][name]["p"] if base else None

    # V1 + V5: hold
    t0 = send(ctx, name, u.mode_hold(ctx.minsoc))
    hold = run_phase(ctx, f"{name} hold", [name], mins(args.hold_min), guard_grid(3500))
    r["hold"] = {"bat_avg": avg(hold, lambda s: s[name]["p"]), "p1_avg": avg(hold, lambda s: s["p1"]),
                 "latency_s": latency(hold, name, t0, before),
                 "charged_from_pv": any(s[name]["p"] is not None and s["p1"] is not None and s[name]["p"] > 100 and s["p1"] < -100 for s in hold[len(hold) // 4:])}
    before = hold[-1][name]["p"] if hold else None
    soc_before_charge = hold[-1][name]["soc"] if hold else None

    # V9 + V5: charge op vast vermogen
    if soc_before_charge is not None and soc_before_charge >= ctx.maxsoc - 2:
        log_event(ctx, name, "charge overgeslagen: lading al op maxsoc")
        r["charge"] = {"skipped": "soc op maxsoc"}
    else:
        t0 = send(ctx, name, u.mode_charge(spec["power_w"], ctx.maxsoc))
        chg = run_phase(ctx, f"{name} charge {spec['power_w']} W", [name], mins(args.charge_min), guard_grid(3500))
        bat_avg = avg(chg, lambda s: s[name]["p"])
        soc1 = next((s[name]["soc"] for s in reversed(chg) if s[name]["soc"] is not None), None)
        dur_h = (chg[-1]["t"] - chg[0]["t"]) / 3600 if len(chg) > 1 else 0
        e_in_kwh = (bat_avg or 0) * dur_h / 1000
        r["charge"] = {"bat_avg": bat_avg, "p1_delta_vs_hold": None if bat_avg is None or r["hold"]["p1_avg"] is None else round(avg(chg, lambda s: s["p1"]) - r["hold"]["p1_avg"]),
                       "latency_s": latency(chg, name, t0, before),
                       "soc_from_to": [soc_before_charge, soc1], "energy_in_kwh": round(e_in_kwh, 3),
                       "efficiency_est": None if not e_in_kwh or soc1 is None or soc_before_charge is None else round((soc1 - soc_before_charge) / 100 * spec["kwh"] / e_in_kwh, 2)}
        before = chg[-1][name]["p"] if chg else None

    # V2: ontladen op vast vermogen (kandidaatcodes), kort en afgebroken zodra bewezen
    r["discharge"] = {}
    for code in args.discharge_codes:
        if ctx.abort:
            break
        soc_now = sample(ctx, [name])[name]["soc"]
        if soc_now is not None and soc_now <= ctx.minsoc + 3:
            r["discharge"][str(code)] = "overgeslagen: lading te laag"
            break
        u.set(IDV_MODE, [4])
        what = f"47015=[{code},{spec['power_w']},{ctx.minsoc}]"
        try:
            res, ms = u.set(IDV_RTC, [code, spec["power_w"], ctx.minsoc])
        except Exception as e:  # noqa: BLE001
            res, ms = f"fout {type(e).__name__}", 0
        t0 = send(ctx, name, [(what, res, ms)])

        def g_dis(s, samples, _t0=t0):
            p = s[name]["p"]
            if p is not None and p < -200 and s["t"] - _t0 > 90:
                return "ontladen gezien, genoeg bewijs"
            if s["p1"] is not None and s["p1"] < -300 and s["t"] - _t0 > 60:
                return "injectie gezien, genoeg bewijs"
            return None
        dis = run_phase(ctx, f"{name} discharge code {code}", [name], mins(3), g_dis)
        r["discharge"][str(code)] = {"set_result": res, "bat_avg": avg(dis, lambda s: s[name]["p"], 20), "p1_min": _r(min((s["p1"] for s in dis if s["p1"] is not None), default=None)),
                                     "latency_s": latency(dis, name, t0, before)}
        before = dis[-1][name]["p"] if dis else None
        u.set(IDV_RTC, [0, 50, ctx.minsoc])  # terug naar stand-by tussen de kandidaten

    # V6: stil in real-time control
    if "idle" not in args.skip and not ctx.abort:
        send(ctx, name, u.mode_hold(ctx.minsoc))
        idle = run_phase(ctx, f"{name} stil in real-time control (geen commando's)", [name], mins(args.idle_min))
        moved = [s for s in idle[len(idle) // 4:] if s[name]["p"] is not None and abs(s[name]["p"]) > 100]
        r["idle"] = {"minutes": round((idle[-1]["t"] - idle[0]["t"]) / 60, 1) if len(idle) > 1 else 0,
                     "first_activity_after_s": round(moved[0]["t"] - idle[0]["t"]) if moved else None,
                     "mode_readback_end": idle[-1][name]["mode"] if idle else None,
                     "verdict": "blijft stil staan zolang niemand iets stuurt" if not moved else f"werd na {round(moved[0]['t'] - idle[0]['t'])} s weer actief (time-out naar zelfverbruik?)"}
        before = idle[-1][name]["p"] if idle else None

    # V5: terug naar normal, latentie
    t0 = send(ctx, name, u.mode_normal())
    back = run_phase(ctx, f"{name} terug normal", [name], mins(3))
    r["normal_latency_s"] = latency(back, name, t0, before)
    r["normal_bat_avg"] = avg(back, lambda s: s[name]["p"], 30)
    r["soc_end"] = next((s[name]["soc"] for s in reversed(back) if s[name]["soc"] is not None), None)


def test_step(ctx: Ctx, args, report: dict) -> None:
    """V8: beide units in normal, een last van ±2 kW handmatig aan en uit."""
    names = list(ctx.units)
    for n in names:
        send(ctx, n, ctx.units[n].mode_normal())
    print("\nSTAPTEST: beide units staan in zelfverbruik. Straks vraag ik je een last van ±2 kW aan te zetten (waterkoker"
          " of oven) en 90 s later weer uit. Hou de P1 in de gaten.", flush=True)
    fs = int(__import__("os").environ.get("F0_FAST_S", 30))
    settle = run_phase(ctx, "step: rust", names, fs if args.fast else 60)
    print("\a>>> ZET NU DE LAST AAN (±2 kW) <<<", flush=True)
    t_on = time.time()
    on = run_phase(ctx, "step: last aan", names, fs if args.fast else 90)
    print("\a>>> ZET NU DE LAST UIT <<<", flush=True)
    t_off = time.time()
    off = run_phase(ctx, "step: last uit", names, fs if args.fast else 120)

    def settle_time(samples, t0, band=150):
        """Eerste moment waarna P1 10 s lang binnen ±band blijft."""
        vals = [(s["t"], s["p1"]) for s in samples if s["p1"] is not None]
        for i in range(len(vals)):
            window = vals[i:i + 10]
            if len(window) == 10 and all(abs(p) <= band for _, p in window):
                return round(window[0][0] - t0, 1)
        return None

    rest_p1 = avg(settle, lambda s: s["p1"], 10)
    report["step"] = {
        "p1_rest_avg": rest_p1,
        "on": {"p1_peak": max((s["p1"] for s in on if s["p1"] is not None), default=None),
               "settle_s": settle_time(on, t_on), "p1_avg_after_30s": avg(on, lambda s: s["p1"], 30),
               "oscillations": {n: oscillation(on, n) for n in names}},
        "off": {"p1_injection_peak": _r(min((s["p1"] for s in off if s["p1"] is not None), default=None)),
                "settle_s": settle_time(off, t_off), "oscillations": {n: oscillation(off, n) for n in names}},
        "unit_share_on": {n: avg(on, lambda s, n=n: s[n]["p"], 30) for n in names},
    }
    print(json.dumps(report["step"], indent=2, ensure_ascii=False))


# ---------------------------------------------------------------------------
# rapport
# ---------------------------------------------------------------------------

def write_report(ctx: Ctx, report: dict, args) -> Path:
    L = [f"# F0 Indevolt-test — {datetime.now(TZ).strftime('%d-%m-%Y %H:%M')}", "",
         f"minsoc {ctx.minsoc} %, maxsoc {ctx.maxsoc} %, kandidaten ontladen {args.discharge_codes}, fast={args.fast}", ""]
    for n in ctx.units:
        r = report.get(n)
        if not r:
            continue
        L += [f"## {n.capitalize()} ({UNIT_SPECS[n]['power_w']} W, {UNIT_SPECS[n]['kwh']} kWh)", ""]
        if "error" in r:
            L += [f"Onbereikbaar: {r['error']}", ""]
            continue
        hold, chg = r.get("hold", {}), r.get("charge", {})
        L += [f"- **V7 terugleesbaar**: {r['readback']} (47005 {'leesbaar → waakhond kan op de modus zelf kijken' if r['readback'].get(IDV_MODE) is not None else 'NIET leesbaar → waakhond moet op gedrag kijken, zoals ontworpen'})",
              f"- **V4 lezen**: mediaan {r['read_latency_ms']['median']} ms, p95 {r['read_latency_ms']['p95']} ms, max {r['read_latency_ms']['max']} ms, {r['read_errors']} fouten in de baseline → interval van 1 s {'haalbaar' if r['read_latency_ms']['p95'] < 500 else 'KRAP'}",
              f"- **V3 teken**: {r.get('sign')} (baseline P1 {r['baseline']['p1_avg']} W, batterij {r['baseline']['bat_avg']} W)",
              f"- **V1 hold**: batterij gemiddeld {hold.get('bat_avg')} W bij P1 {hold.get('p1_avg')} W; laadt uit zonne-overschot: **{'JA' if hold.get('charged_from_pv') else 'NEE'}**; dode tijd hold {hold.get('latency_s')} s"]
        if chg.get("skipped"):
            L.append(f"- **V9 charge**: overgeslagen ({chg['skipped']})")
        else:
            L.append(f"- **V9 charge {UNIT_SPECS[n]['power_w']} W**: gemeten {chg.get('bat_avg')} W, P1 steeg {chg.get('p1_delta_vs_hold')} W t.o.v. hold; SoC {chg.get('soc_from_to')}; {chg.get('energy_in_kwh')} kWh in → rendement laden ≈ {chg.get('efficiency_est')} (ruw, SoC in hele %); dode tijd {chg.get('latency_s')} s")
        for code, d in r.get("discharge", {}).items():
            if isinstance(d, str):
                L.append(f"- **V2 ontladen code {code}**: {d}")
            else:
                ok = d["bat_avg"] is not None and d["bat_avg"] < -200
                L.append(f"- **V2 ontladen code {code}**: SetData → {d['set_result']}; batterij {d['bat_avg']} W, P1-minimum {d['p1_min']} W → **{'BESTAAT (en injecteert als de last wegvalt)' if ok else 'geen ontlading gezien'}**; dode tijd {d['latency_s']} s")
        if r.get("idle"):
            L.append(f"- **V6 stil in real-time control** ({r['idle']['minutes']} min): {r['idle']['verdict']}; 47005 op het einde: {r['idle']['mode_readback_end']}")
        L += [f"- **V5 terug naar normal**: dode tijd {r.get('normal_latency_s')} s, daarna gemiddeld {r.get('normal_bat_avg')} W",
              f"- SoC {r.get('soc_start')} % → {r.get('soc_end')} %", ""]
    if report.get("step"):
        s = report["step"]
        L += ["## Staptest, beide units in zelfverbruik (V8)", "",
              f"- rust: P1 gemiddeld {s['p1_rest_avg']} W",
              f"- last aan: P1-piek {s['on']['p1_peak']} W, binnen ±150 W na {s['on']['settle_s']} s, daarna gemiddeld {s['on']['p1_avg_after_30s']} W; aandeel per unit {s['unit_share_on']}; richtingswisselingen ≥200 W: {s['on']['oscillations']}",
              f"- last uit: injectiepiek {s['off']['p1_injection_peak']} W, binnen ±150 W na {s['off']['settle_s']} s; richtingswisselingen: {s['off']['oscillations']}",
              f"- acceptatie review H5 (stabiel < 10 s, injectiepiek < 300 W): **{'OK' if (s['on']['settle_s'] or 99) < 10 and (s['off']['settle_s'] or 99) < 10 and (s['off']['p1_injection_peak'] or 0) > -300 else 'NIET gehaald'}**", ""]
    L += ["## Nog met de hand na te kijken", "",
          "- Firmwareversie van beide units (app of webinterface) noteren in het ontwerpdocument.",
          "- Staan 47005/47015 in flash? Niet meetbaar met dit script: vraag het Indevolt-support of kijk in de documentatie.",
          f"- Ruwe data: `{ctx.out}`; events: `{ctx.out.with_suffix('.events.json')}`", ""]
    p = ctx.out.with_name("RAPPORT.md") if ctx.out.name.startswith("test-") else ctx.out.with_suffix(".md")
    p.write_text("\n".join(L))
    ctx.out.with_suffix(".events.json").write_text(json.dumps(ctx.events, indent=1, ensure_ascii=False))
    return p


# ---------------------------------------------------------------------------

def _lat(ms: list) -> dict:
    v = sorted(x for x in ms if x is not None)
    if not v:
        return {"median": None, "p95": None, "max": None}
    return {"median": round(median(v)), "p95": round(v[int(len(v) * 0.95) - 1] if len(v) > 1 else v[0]), "max": round(v[-1])}


def _r(v):
    return None if v is None else round(v)


def _f(v):
    try:
        return None if v is None else float(v)
    except (TypeError, ValueError):
        return None


def _fmt(v):
    if v is None:
        return ""
    return f"{v:.0f}" if isinstance(v, float) else str(v)


def evcc_running() -> bool:
    try:
        return subprocess.run(["systemctl", "is-active", "--quiet", "evcc"], check=False).returncode == 0
    except FileNotFoundError:
        return False


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("-c", "--config", required=True, help="dezelfde JSON als het logscript (hosts, wachtwoorden)")
    ap.add_argument("--unit", choices=["garage", "zolder", "both"], default="both")
    ap.add_argument("--minsoc", type=int, default=10, help="minsoc zoals in evcc ingesteld (standaard 10)")
    ap.add_argument("--maxsoc", type=int, default=100)
    ap.add_argument("--hold-min", type=int, default=15)
    ap.add_argument("--charge-min", type=int, default=15)
    ap.add_argument("--idle-min", type=int, default=20, help="minuten stil in real-time control (review: liefst 60)")
    ap.add_argument("--discharge-codes", type=lambda s: [int(x) for x in s.split(",") if x], default=[2, 3],
                    help="kandidaatwaarden voor 47015[0] als 'ontladen'; leeg = overslaan")
    ap.add_argument("--skip", nargs="*", default=[], choices=["idle", "step"], help="fases overslaan")
    ap.add_argument("--fast", action="store_true", help="alle fases 2 min (alleen om de flow te testen)")
    ap.add_argument("--ignore-evcc", action="store_true", help="niet stoppen als evcc nog draait (NIET aan te raden)")
    ap.add_argument("--out-dir", default="/var/log/f0")
    args = ap.parse_args()

    cfg = json.load(open(args.config))
    if evcc_running() and not args.ignore_evcc:
        sys.exit("evcc draait nog. Stop het eerst (systemctl stop evcc), anders sturen twee regelaars dezelfde batterij.")
    ans = input("Staan de batterij-automations van capaciteitstarief v8 in Home Assistant UIT, en laadt de auto NIET? [j/N] ")
    if ans.strip().lower() not in ("j", "ja", "y"):
        sys.exit("Eerst uitzetten, dan opnieuw starten.")

    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(TZ).strftime("%Y-%m-%d_%H%M")
    out = out_dir / f"test-{stamp}.csv"
    fh = open(out, "w", newline="")
    w = csv.writer(fh)
    w.writerow(["ts_local", "epoch", "phase", "p1_w", "garage_p_w", "garage_soc", "garage_mode", "garage_ms",
                "zolder_p_w", "zolder_soc", "zolder_mode", "zolder_ms", "errors"])
    units = {u["name"]: Indevolt(u["name"], u["host"], u.get("user") or None, u.get("password") or None)
             for u in cfg["indevolt"] if args.unit in ("both", u["name"])}
    ctx = Ctx(units=units, p1_host=cfg["p1_host"], minsoc=args.minsoc, maxsoc=args.maxsoc, out=out, rows=w, rows_fh=fh)

    def on_signal(*_):
        ctx.abort = True
        print("\n!! afgebroken, alles terug naar normal", flush=True)
    signal.signal(signal.SIGINT, on_signal)
    signal.signal(signal.SIGTERM, on_signal)

    report: dict = {}
    try:
        for n in units:
            if not ctx.abort:
                test_unit(ctx, n, args, report)
        if "step" not in args.skip and not ctx.abort and args.unit == "both":
            test_step(ctx, args, report)
    finally:
        # ALTIJD terug naar zelfverbruik, met herhaling
        for n, u in units.items():
            for attempt in range(3):
                try:
                    u.mode_normal()
                    print(f"[veilig] {n} terug op normal", flush=True)
                    break
                except Exception as e:  # noqa: BLE001
                    print(f"[veilig] {n}: normal sturen mislukt ({e}), poging {attempt + 1}/3", flush=True)
                    time.sleep(3)
            else:
                print(f"!!!! {n} NIET terug op normal gekregen. Doe het met de hand: 47005=[1], 1143=[0] !!!!", flush=True)
        fh.close()
        p = write_report(ctx, report, args)
        print(f"\nRapport: {p}\nRuwe data: {out}\nVergeet niet: systemctl start evcc", flush=True)


if __name__ == "__main__":
    main()
