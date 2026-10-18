#!/usr/bin/env python3
"""F0-businesscase: wat is de haalbare maandpiek als de EV begrensd wordt en de batterij 2 kW bijspringt?

Invoer
  1. Fluvius-kwartierwaarden (CSV uit Mijn Fluvius → Verbruik → kwartierwaarden; ; of , gescheiden,
     kolommen Van Datum / Van Tijdstip / Register / Volume; registers "Afname Dag" en "Afname Nacht" tellen).
  2. Het laadpaalvermogen of de laadpaalteller uit Home Assistant als CSV met een tijdkolom en een waardekolom
     (bv. export van sensor.alfen_power in W, of sensor.alfen_energy in kWh). Het script herkent de kolommen
     zelf; anders geef je ze met --ev-time-col / --ev-value-col en --ev-kind.

Rekenwijze per kwartier
  afname_kW      = Fluvius-afname × 4 (bruto, zoals het capaciteitstarief)
  ev_kW          = laadpaalenergie in dat kwartier × 4
  huis_kW        = max(0, afname − ev)                        → scenario B: EV begrensd, huis alleen
  met batterij   = huis − min(P_bat, huis − vloer) zolang de dagreserve het toelaat   → scenario C (2 kW) en D (1,2 kW)
Maandpiek = hoogste kwartier van de maand, minstens de vloer van 2,5 kW. Jaarkost = gemiddelde van de 12 maandpieken × tarief.

Gebruik
  python3 f0_businesscase.py --fluvius kwartieren.csv --ev alfen_power.csv --ev-kind power_w --out ./businesscase
"""
from __future__ import annotations

import argparse
import csv
import io
import re
import sys
from bisect import bisect_right
from collections import defaultdict
from datetime import datetime, timedelta
from pathlib import Path
from zoneinfo import ZoneInfo

TZ = ZoneInfo("Europe/Brussels")
Q = timedelta(minutes=15)

DATE_FORMATS = ["%d-%m-%Y %H:%M:%S", "%d-%m-%Y %H:%M", "%d/%m/%Y %H:%M:%S", "%d/%m/%Y %H:%M", "%Y-%m-%d %H:%M:%S",
                "%Y-%m-%d %H:%M", "%Y-%m-%dT%H:%M:%S", "%Y-%m-%dT%H:%M:%S.%f", "%Y-%m-%d %H:%M:%S.%f"]


# ---------------------------------------------------------------------------
# inlezen
# ---------------------------------------------------------------------------

def _open_csv(path: str):
    text = Path(path).read_text(encoding="utf-8-sig")
    sample = text[:4096]
    delim = ";" if sample.count(";") > sample.count(",") else ","
    return csv.DictReader(io.StringIO(text), delimiter=delim)


def _parse_dt(s: str) -> datetime | None:
    s = s.strip()
    if not s:
        return None
    if re.fullmatch(r"\d{10}(\.\d+)?", s):  # epoch
        return datetime.fromtimestamp(float(s), TZ)
    s2 = re.sub(r"(\+\d\d:\d\d|Z)$", "", s)
    for f in DATE_FORMATS:
        try:
            dt = datetime.strptime(s2, f)
            if s.endswith("Z") or s.endswith("+00:00"):
                return dt.replace(tzinfo=ZoneInfo("UTC")).astimezone(TZ)
            m = re.search(r"([+-]\d\d):(\d\d)$", s)
            if m:
                from datetime import timezone
                off = timedelta(hours=int(m.group(1)), minutes=int(m.group(2)) * (1 if m.group(1)[0] == "+" else -1))
                return dt.replace(tzinfo=timezone(off)).astimezone(TZ)
            return dt.replace(tzinfo=TZ)
        except ValueError:
            continue
    return None


def _num(s: str) -> float | None:
    s = (s or "").strip().replace("\u202f", "").replace(" ", "")
    if not s or s.lower() in ("unknown", "unavailable", "nan", "none"):
        return None
    if "," in s and "." in s:
        s = s.replace(".", "").replace(",", ".")
    elif "," in s:
        s = s.replace(",", ".")
    try:
        return float(s)
    except ValueError:
        return None


def _find_col(fields: list[str], *needles: str) -> str | None:
    for f in fields:
        fl = f.lower()
        if all(n in fl for n in needles):
            return f
    return None


def read_fluvius(path: str) -> dict[datetime, float]:
    """Geeft per kwartierbegin de bruto afname in kWh (dag + nacht)."""
    rd = _open_csv(path)
    f = rd.fieldnames or []
    c_date = _find_col(f, "van", "datum") or _find_col(f, "datum") or _find_col(f, "date")
    c_time = _find_col(f, "van", "tijd") or _find_col(f, "tijd") or _find_col(f, "time")
    c_reg = _find_col(f, "register") or _find_col(f, "richting")
    c_vol = _find_col(f, "volume") or _find_col(f, "waarde") or _find_col(f, "value")
    if not (c_date and c_vol):
        sys.exit(f"Fluvius-CSV: kolommen niet herkend. Gevonden: {f}")
    out: dict[datetime, float] = defaultdict(float)
    n = 0
    for row in rd:
        reg = (row.get(c_reg) or "").lower() if c_reg else "afname"
        if "afname" not in reg and "import" not in reg and "verbruik" not in reg:
            continue
        v = _num(row.get(c_vol, ""))
        dt = _parse_dt(f"{row[c_date]} {row[c_time]}" if c_time and c_time != c_date else row[c_date])
        if v is None or dt is None:
            continue
        out[dt] += v
        n += 1
    if not out:
        sys.exit("Fluvius-CSV: geen afnamerijen gevonden (verwacht Register 'Afname Dag'/'Afname Nacht').")
    print(f"Fluvius: {n} rijen, {len(out)} kwartieren van {min(out)} tot {max(out)}")
    return dict(out)


def read_ev(path: str, kind: str, time_col: str | None, value_col: str | None) -> list[tuple[datetime, float]]:
    rd = _open_csv(path)
    f = rd.fieldnames or []
    c_t = time_col or _find_col(f, "last_changed") or _find_col(f, "last_updated") or _find_col(f, "start") \
        or _find_col(f, "time") or _find_col(f, "datum") or _find_col(f, "date") or f[0]
    c_v = value_col or _find_col(f, "state") or _find_col(f, "mean") or _find_col(f, "sum") or _find_col(f, "value") \
        or _find_col(f, "waarde") or next((x for x in f if x != c_t), None)
    rows = []
    for row in rd:
        dt, v = _parse_dt(row.get(c_t, "")), _num(row.get(c_v, ""))
        if dt is not None and v is not None:
            rows.append((dt, v))
    rows.sort()
    if not rows:
        sys.exit(f"EV-CSV: niets herkend (tijdkolom {c_t!r}, waardekolom {c_v!r}). Geef --ev-time-col/--ev-value-col.")
    print(f"EV ({kind}): {len(rows)} punten uit kolommen {c_t!r}/{c_v!r}, van {rows[0][0]} tot {rows[-1][0]}")
    if kind == "auto":
        vals = [v for _, v in rows]
        kind = "energy_kwh" if all(b >= a for a, b in zip(vals, vals[1:])) and max(vals) > 50 else "power_w"
        print(f"  → geraden als {kind}")
    return [(t, v * (1000 if kind == "power_kw" else 1)) for t, v in rows] if kind != "energy_kwh" else [("E", *r) for r in rows]  # type: ignore


def read_evcc_sessions(db_path: str) -> list:
    """Laadsessies uit de evcc-database (tabel sessions): energie gelijk gespreid over de sessieduur."""
    import sqlite3
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    cols = [r[1] for r in con.execute("PRAGMA table_info(sessions)")]
    c_start = next(c for c in cols if c in ("created", "start"))
    c_end = next(c for c in cols if c in ("finished", "end"))
    c_kwh = next(c for c in cols if "charged" in c)
    out = []
    for a, b, kwh in con.execute(f"SELECT {c_start}, {c_end}, {c_kwh} FROM sessions WHERE {c_end} IS NOT NULL AND {c_kwh} > 0"):
        ta, tb = _parse_dt(str(a)), _parse_dt(str(b))
        if ta and tb and tb > ta:
            out.append(("S", ta, tb, float(kwh)))
    out.sort(key=lambda r: r[1])
    print(f"evcc-sessies: {len(out)} sessies van {out[0][1]} tot {out[-1][2]}" if out else "evcc-sessies: geen gevonden")
    return out


def ev_energy_per_quarter(rows, quarters: list[datetime]) -> dict[datetime, float]:
    """EV-energie (kWh) per kwartier. Vermogensreeks: trapsgewijs integreren (laatste waarde houden).
    Energiereeks: verschil van de teller op de kwartiergrenzen (lineair geïnterpoleerd)."""
    out: dict[datetime, float] = {}
    if rows and rows[0][0] == "S":
        for q in quarters:
            out[q] = 0.0
        qs = quarters
        for _, ta, tb, kwh in rows:
            p_kw = kwh / ((tb - ta).total_seconds() / 3600)
            i = bisect_right(qs, ta) - 1
            while i < len(qs) and (i < 0 or qs[i] < tb):
                if i >= 0:
                    q = qs[i]
                    overlap = (min(tb, q + Q) - max(ta, q)).total_seconds() / 3600
                    if overlap > 0:
                        out[q] += p_kw * overlap
                i += 1
        return out
    if rows and rows[0][0] == "E":
        pts = [(t, v) for _, t, v in rows]
        ts = [t for t, _ in pts]

        def at(t):
            i = bisect_right(ts, t)
            if i == 0:
                return pts[0][1]
            if i >= len(pts):
                return pts[-1][1]
            (t0, v0), (t1, v1) = pts[i - 1], pts[i]
            return v0 + (v1 - v0) * ((t - t0) / (t1 - t0)) if t1 > t0 else v0
        for q in quarters:
            if ts[0] <= q + Q and ts[-1] >= q:
                out[q] = max(0.0, at(q + Q) - at(q))
        return out
    ts = [t for t, _ in rows]
    for q in quarters:
        if q + Q < ts[0] or q > ts[-1]:
            continue
        e_wh, t, i = 0.0, q, bisect_right(ts, q) - 1
        p = rows[i][1] if i >= 0 else 0.0
        i += 1
        while t < q + Q:
            t_next = min(q + Q, rows[i][0]) if i < len(rows) else q + Q
            e_wh += max(0.0, p) * (t_next - t).total_seconds() / 3600
            t = t_next
            if i < len(rows) and rows[i][0] <= t:
                p = rows[i][1]
                i += 1
        out[q] = e_wh / 1000
    return out


# ---------------------------------------------------------------------------
# scenario's
# ---------------------------------------------------------------------------

def season_reserve(month: int, winter: float, shoulder: float, summer: float) -> float:
    return winter if month in (11, 12, 1, 2) else shoulder if month in (3, 4, 9, 10) else summer


def with_battery(quarters: list[datetime], house_kw: dict[datetime, float], p_bat_kw: float, floor_kw: float,
                 reserve: tuple[float, float, float]) -> dict[datetime, float]:
    """Per dag een reserve (kWh) die elk kwartier boven de vloer tot p_bat_kw afvlakt. Reserve vult elke nacht."""
    out, day, left = {}, None, 0.0
    for q in quarters:
        if q.date() != day:
            day, left = q.date(), season_reserve(q.month, *reserve)
        h = house_kw[q]
        need = max(0.0, h - floor_kw)
        shave = min(p_bat_kw, need, left * 4)      # kWh × 4 = kW gedurende een kwartier
        left -= shave / 4
        out[q] = h - shave
    return out


def month_peaks(series: dict[datetime, float], floor_kw: float) -> dict[str, tuple[float, datetime | None]]:
    best: dict[str, tuple[float, datetime | None]] = {}
    for q, v in series.items():
        k = q.strftime("%Y-%m")
        if k not in best or v > best[k][0]:
            best[k] = (v, q)
    return {k: (max(v, floor_kw), t if v > floor_kw else None) for k, (v, t) in best.items()}


# ---------------------------------------------------------------------------

def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--fluvius", required=True)
    ap.add_argument("--ev", help="CSV met laadpaalvermogen of -teller uit HA")
    ap.add_argument("--ev-sessions", help="evcc.db (sqlite): laadsessies als EV-bron, bv. /var/lib/evcc/evcc.db")
    ap.add_argument("--ev-kind", choices=["auto", "power_w", "power_kw", "energy_kwh"], default="auto")
    ap.add_argument("--ev-time-col"); ap.add_argument("--ev-value-col")
    ap.add_argument("--floor", type=float, default=2.5, help="vloer kW (capaciteitstarief: 2,5)")
    ap.add_argument("--tarief", type=float, default=60.53, help="€/kW/jaar incl. btw (Fluvius West 2026)")
    ap.add_argument("--reserve", type=float, nargs=3, default=[3.5, 2.0, 1.5], metavar=("WINTER", "TUSSEN", "ZOMER"),
                    help="dagreserve kWh per seizoen voor scenario C/D (voorstel review M3)")
    ap.add_argument("--months", type=int, default=12, help="hoeveel laatste volle maanden meetellen")
    ap.add_argument("--out", default="./businesscase")
    args = ap.parse_args()

    out = Path(args.out); out.mkdir(parents=True, exist_ok=True)
    grid_kwh = read_fluvius(args.fluvius)
    quarters = sorted(grid_kwh)
    # alleen volle maanden, de laatste N
    months_all = sorted({q.strftime("%Y-%m") for q in quarters})
    counts = defaultdict(int)
    for q in quarters:
        counts[q.strftime("%Y-%m")] += 1
    full = [m for m in months_all if counts[m] >= 27 * 96]
    months = full[-args.months:]
    quarters = [q for q in quarters if q.strftime("%Y-%m") in months]
    print(f"Maanden in de berekening: {months[0]} … {months[-1]} ({len(months)}); overgeslagen (onvolledig): {sorted(set(months_all) - set(full))}")

    grid_kw = {q: grid_kwh[q] * 4 for q in quarters}
    if args.ev_sessions:
        ev_kwh = ev_energy_per_quarter(read_evcc_sessions(args.ev_sessions), quarters)
    elif args.ev:
        ev_kwh = ev_energy_per_quarter(read_ev(args.ev, args.ev_kind, args.ev_time_col, args.ev_value_col), quarters)
    else:
        ev_kwh = {}
    covered = [q for q in quarters if ev_kwh.get(q, 0) > 0]
    if args.ev or args.ev_sessions:
        missing = [m for m in months if not any(q.strftime("%Y-%m") == m for q in covered)]
        if missing:
            print(f"LET OP: geen EV-data voor {missing}; daar wordt EV = 0 verondersteld (huis alleen = afname).")
    ev_kw = {q: ev_kwh.get(q, 0.0) * 4 for q in quarters}
    house_kw = {q: max(0.0, grid_kw[q] - ev_kw[q]) for q in quarters}

    scen = {
        "A nu (Fluvius)": grid_kw,
        "B EV begrensd (huis alleen)": house_kw,
        "C B + batterij 2 kW": with_battery(quarters, house_kw, 2.0, args.floor, tuple(args.reserve)),
        "D B + alleen zolder 1,2 kW": with_battery(quarters, house_kw, 1.2, args.floor, tuple(args.reserve)),
    }
    peaks = {name: month_peaks(s, args.floor) for name, s in scen.items()}

    # --- rapport -----------------------------------------------------------
    L = [f"# Businesscase capaciteitstarief — {datetime.now(TZ).strftime('%d-%m-%Y')}", "",
         f"Periode {months[0]} t/m {months[-1]}, vloer {args.floor} kW, tarief €{args.tarief}/kW/jaar, "
         f"dagreserve batterij {args.reserve} kWh (winter/tussenseizoen/zomer).", "",
         "## Maandpieken (kW) per scenario", "",
         "| Maand | " + " | ".join(scen) + " | EV-aandeel in piek A |", "|---|" + "---|" * (len(scen) + 1)]
    for m in months:
        cells = []
        for name in scen:
            v, t = peaks[name][m]
            cells.append(f"{v:.2f}" + (f" ({t.strftime('%d/%m %H:%M')})" if t else ""))
        tA = peaks["A nu (Fluvius)"][m][1]
        ev_share = f"{ev_kw[tA]:.2f} kW" if tA else "–"
        L.append(f"| {m} | " + " | ".join(cells) + f" | {ev_share} |")
    L += ["", "## Jaarkost en besparing", "", "| Scenario | Gem. maandpiek | Kost/jaar | Besparing t.o.v. A |", "|---|---|---|---|"]
    avgA = sum(peaks["A nu (Fluvius)"][m][0] for m in months) / len(months)
    for name in scen:
        a = sum(peaks[name][m][0] for m in months) / len(months)
        L.append(f"| {name} | {a:.2f} kW | €{a * args.tarief:.0f} | €{(avgA - a) * args.tarief:.0f} |")
    L += ["", "Let op: scenario B neemt aan dat de EV nooit boven het budget laadt (EMS-doel), en C/D dat de batterij "
          "elke dag met de seizoensreserve begint en de piek tot haar vermogen afvlakt. Het is een bovengrens voor de "
          "besparing; regelvertraging en lege batterijen (review H1, M3) halen er in de praktijk enkele euro's af.", ""]

    # top-kwartieren huis alleen: waar komen de resterende pieken vandaan?
    L += ["## Resterende pieken huis alleen (scenario B): top 15", "", "| Wanneer | Weekdag | Afname | EV | Huis | Huis − 2 kW |", "|---|---|---|---|---|---|"]
    top = sorted(quarters, key=lambda q: house_kw[q], reverse=True)[:15]
    for q in top:
        L.append(f"| {q.strftime('%d-%m-%Y %H:%M')} | {'weekend' if q.weekday() >= 5 else 'weekdag'} | {grid_kw[q]:.2f} | {ev_kw[q]:.2f} | {house_kw[q]:.2f} | {max(args.floor, house_kw[q] - 2):.2f} |")

    # kookvenster: wanneer zit het huis alleen boven de vloer?
    hist_wd, hist_we = defaultdict(int), defaultdict(int)
    for q in quarters:
        if house_kw[q] > args.floor:
            (hist_we if q.weekday() >= 5 else hist_wd)[q.hour] += 1
    L += ["", f"## Kwartieren met huis alleen > {args.floor} kW, per uur (controle van het kookvenster)", "",
          "| Uur | Weekdagen | Weekend |", "|---|---|---|"]
    for h in range(24):
        if hist_wd[h] or hist_we[h]:
            L.append(f"| {h:02d}–{h + 1:02d} | {hist_wd[h]} | {hist_we[h]} |")
    n_over = sum(hist_wd.values()) + sum(hist_we.values())
    L += ["", f"Totaal {n_over} kwartieren boven de vloer zonder EV, op {len(quarters)} kwartieren "
          f"({100 * n_over / len(quarters):.2f} %). Energie boven de vloer per dag, maximum: "
          f"{max(sum(max(0, house_kw[q] - args.floor) / 4 for q in quarters if q.date() == d) for d in {q.date() for q in quarters}):.2f} kWh "
          f"(moet onder de dagreserve blijven, anders loopt de batterij leeg).", ""]

    (out / "businesscase.md").write_text("\n".join(L))
    with open(out / "kwartieren.csv", "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["kwartier", "afname_kw", "ev_kw", "huis_kw", "huis_bat2_kw", "huis_bat1_2_kw"])
        for q in quarters:
            w.writerow([q.isoformat(), f"{grid_kw[q]:.3f}", f"{ev_kw[q]:.3f}", f"{house_kw[q]:.3f}",
                        f"{scen['C B + batterij 2 kW'][q]:.3f}", f"{scen['D B + alleen zolder 1,2 kW'][q]:.3f}"])
    with open(out / "maandpieken.csv", "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["maand"] + list(scen))
        for m in months:
            w.writerow([m] + [f"{peaks[n][m][0]:.3f}" for n in scen])
    print("\n".join(L[:len(months) + 8]))
    print(f"\nRapport: {out / 'businesscase.md'}")


if __name__ == "__main__":
    main()
