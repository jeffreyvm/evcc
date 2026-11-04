#!/usr/bin/env python3
"""Kijkt in het F0-log of de ingebouwde peak shaving van de Indevolts doet wat we verwachten.

Beslist op 4 november: de Indevolts houden zelf een reserve vast (peak shaving in de app, grens 2,5 kW,
reserve 20 % per unit) en evcc stuurt de batterijen niet. Dit script beantwoordt per dag:

  P1  zakt een unit onder haar reserve terwijl het net onder de grens blijft? (dan houdt ze niet vast)
  P2  springen de units bij als het net boven de grens gaat, en hoeveel kwartieren eindigen toch erboven?
  P3  laden de units zelf van het net bij (laden terwijl er afgenomen wordt), en hoeveel kWh?
  P4  volgen ze nog P1 boven de reserve (zelfverbruik), of staan ze stil?

Tekens zoals in het F0-log: batterijvermogen negatief is laden, positief is ontladen.

Gebruik:
  python3 f0_peakshaving_check.py                       # gisteren
  python3 f0_peakshaving_check.py --dag 2026-11-04      # één dag
  python3 f0_peakshaving_check.py --van 2026-11-04 --tot 2026-11-07
"""
from __future__ import annotations

import argparse
import csv
import gzip
from collections import defaultdict
from datetime import date, datetime, timedelta
from pathlib import Path

UNITS = ("garage", "zolder")


def rows(log_dir: Path, day: date):
    """Rijen van één dag uit f0-<dag>.csv of .csv.gz, met getallen of None."""
    for name in (f"f0-{day}.csv", f"f0-{day}.csv.gz"):
        p = log_dir / name
        if not p.exists():
            continue
        fh = gzip.open(p, "rt", newline="") if name.endswith(".gz") else open(p, newline="")
        with fh:
            for r in csv.DictReader(fh):
                out = {"ts": r.get("ts_local", ""), "epoch": _f(r.get("epoch"))}
                out["net"] = _f(r.get("p1_power_w"))
                out["avg15"] = _f(r.get("p1_avg15_w"))
                for u in UNITS:
                    out[f"{u}_p"] = _f(r.get(f"{u}_p_w"))
                    out[f"{u}_soc"] = _f(r.get(f"{u}_soc"))
                yield out
        return
    print(f"{day}: geen F0-log in {log_dir}")


def check_day(log_dir: Path, day: date, limit: float, reserve: float) -> None:
    quarters: dict[str, list[float]] = defaultdict(list)
    q_bat: dict[str, list[float]] = defaultdict(list)
    q_soc: dict[str, list[float]] = defaultdict(list)
    below = {u: 0 for u in UNITS}            # s onder de reserve terwijl ontladen en net onder de grens
    min_soc = {u: None for u in UNITS}
    grid_charge_wh = {u: 0.0 for u in UNITS}
    grid_charge_s = {u: 0 for u in UNITS}
    idle_import_s = {u: 0 for u in UNITS}    # s boven reserve + 5 %, afname boven 300 W, unit levert niets
    over_s = covered_s = 0
    n = 0
    prev_epoch = None

    for r in rows(log_dir, day):
        net = r["net"]
        if net is None:
            continue
        n += 1
        dt = 1.0
        if r["epoch"] is not None and prev_epoch is not None:
            dt = max(0.0, min(5.0, r["epoch"] - prev_epoch))
        prev_epoch = r["epoch"]

        q = r["ts"][11:13] + ":" + f"{int(r['ts'][14:16]) // 15 * 15:02d}" if len(r["ts"]) >= 16 else "?"
        quarters[q].append(net)
        bat = sum(r[f"{u}_p"] or 0.0 for u in UNITS)
        q_bat[q].append(bat)

        if net > limit:
            over_s += 1
        for u in UNITS:
            p, soc = r[f"{u}_p"], r[f"{u}_soc"]
            if soc is None or p is None:
                continue
            q_soc[q].append(soc)
            min_soc[u] = soc if min_soc[u] is None else min(min_soc[u], soc)
            if soc < reserve - 0.5 and p > 50 and net < limit:
                below[u] += 1
            if p < -100 and net > 100:
                grid_charge_s[u] += 1
                grid_charge_wh[u] += -p * dt / 3600
            if soc > reserve + 5 and net > 300 and abs(p) < 20:
                idle_import_s[u] += 1
            if net > limit and p > 100:
                covered_s += 1

    if not n:
        return

    avgs = {q: sum(v) / len(v) for q, v in quarters.items() if len(v) >= 600}
    above = sorted(((a, q) for q, a in avgs.items() if a > limit), reverse=True)
    top = max(avgs.items(), key=lambda kv: kv[1]) if avgs else None

    print(f"\n=== {day} ({n} s gemeten, grens {limit:.0f} W, reserve {reserve:.0f} %) ===")
    if top:
        print(f"Hoogste kwartier: {top[0]} met {top[1]:.0f} W gemiddeld (afname min injectie)")
    print(f"P2 kwartieren boven {limit:.0f} W: {len(above)}"
          + "".join(f"\n     {q}  net {a:.0f} W, batterijen gemiddeld {_avg(q_bat[q]):.0f} W, laagste lading {_min(q_soc[q])} %"
                    for a, q in above[:8]))
    if over_s:
        print(f"   seconden met net boven {limit:.0f} W: {over_s}, waarvan met een ontladende unit: {covered_s}")
    for u in UNITS:
        print(f"{u}:")
        print(f"   laagste lading: {_fmt(min_soc[u])} %")
        print(f"   P1 onder de reserve ontladen terwijl het net onder de grens bleef: {below[u]} s"
              + ("  ← houdt de reserve NIET vast" if below[u] > 60 else ""))
        print(f"   P3 zelf van het net geladen: {grid_charge_s[u]} s, ±{grid_charge_wh[u] / 1000:.2f} kWh")
        print(f"   P4 stil boven de reserve terwijl het huis afnam: {idle_import_s[u]} s"
              + ("  ← volgt P1 niet" if idle_import_s[u] > 600 else ""))


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--log-dir", default="/var/log/f0")
    ap.add_argument("--dag", help="YYYY-MM-DD, standaard gisteren")
    ap.add_argument("--van")
    ap.add_argument("--tot")
    ap.add_argument("--grens", type=float, default=2500, help="peak-shaving-grens in de app, W")
    ap.add_argument("--reserve", type=float, default=20, help="peak-shaving-reserve in de app, %%")
    args = ap.parse_args()

    if args.van:
        d0 = date.fromisoformat(args.van)
        d1 = date.fromisoformat(args.tot) if args.tot else d0
    else:
        d0 = d1 = date.fromisoformat(args.dag) if args.dag else (datetime.now() - timedelta(days=1)).date()
    d = d0
    while d <= d1:
        check_day(Path(args.log_dir), d, args.grens, args.reserve)
        d += timedelta(days=1)


def _f(v):
    try:
        return None if v in (None, "") else float(v)
    except (TypeError, ValueError):
        return None


def _avg(xs):
    return sum(xs) / len(xs) if xs else 0.0


def _min(xs):
    return f"{min(xs):.0f}" if xs else "?"


def _fmt(v):
    return "?" if v is None else f"{v:.0f}"


if __name__ == "__main__":
    main()
