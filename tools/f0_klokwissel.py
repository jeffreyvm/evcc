#!/usr/bin/env python3
"""Controle van het F0-logbestand rond de overgang naar wintertijd (25 oktober 2026).

Gebruik in de evcc-LXC (alleen Python-standaardbibliotheek):
    python3 f0_klokwissel.py                      # zoekt het bestand van 2026-10-25 in /var/log/f0
    python3 f0_klokwissel.py /var/log/f0/<bestand>.csv

Het script schrijft alleen naar het scherm. Kopieer de uitvoer tussen de twee
lijnen "KOPIEER" en plak ze in de chat.
"""
import csv
import glob
import os
import sys
from collections import Counter
from datetime import datetime, timezone

try:
	from zoneinfo import ZoneInfo
	BRU = ZoneInfo("Europe/Brussels")
except Exception:  # tzdata ontbreekt
	BRU = None

LOGDIR = "/var/log/f0"
EMPTY = {"", "none", "null", "nan", "NaN", "None"}


def vind_bestand():
	if len(sys.argv) > 1:
		return sys.argv[1]
	kand = sorted(glob.glob(os.path.join(LOGDIR, "*2026-10-25*")) +
				  glob.glob(os.path.join(LOGDIR, "*20261025*")))
	if kand:
		return kand[0]
	alle = sorted(glob.glob(os.path.join(LOGDIR, "*.csv")), key=os.path.getmtime)
	if not alle:
		sys.exit("Geen CSV gevonden in " + LOGDIR + ". Geef het pad als argument mee.")
	print("(Geen bestand van 25 oktober gevonden; ik neem het nieuwste: %s)" % alle[-1])
	return alle[-1]


def parse_ts(s):
	s = s.strip()
	try:
		x = float(s)
		if x > 1e11:  # milliseconden
			x /= 1000.0
		if x > 1e9:
			return datetime.fromtimestamp(x, tz=timezone.utc), "epoch"
	except ValueError:
		pass
	s2 = s.replace("Z", "+00:00")
	dt = datetime.fromisoformat(s2)
	return dt, ("iso+offset" if dt.tzinfo else "iso-lokaal-zonder-offset")


def kies_tijdkolom(header):
	for naam in header:
		n = naam.lower()
		if n in ("ts", "time", "timestamp", "tijd", "datetime", "t"):
			return naam
	return header[0]


def lokaal(dt):
	if dt.tzinfo is None:
		return dt
	return dt.astimezone(BRU) if BRU else dt.astimezone(timezone.utc)


def hms(dt):
	return lokaal(dt).strftime("%H:%M:%S")


def main():
	pad = vind_bestand()
	with open(pad, newline="", encoding="utf-8", errors="replace") as f:
		kop = f.readline()
		f.seek(0)
		delim = ";" if kop.count(";") > kop.count(",") else ","
		rdr = csv.DictReader(f, delimiter=delim)
		header = rdr.fieldnames or []
		rijen = list(rdr)

	print("=" * 20 + " KOPIEER VANAF HIER " + "=" * 20)
	print("Bestand: %s" % pad)
	print("Rijen: %d (verwacht bij 25 uur: 90000; bij 24 uur: 86400)" % len(rijen))
	print("Kolommen (%d): %s" % (len(header), ", ".join(header)))
	if not rijen:
		print("Leeg bestand.")
		return

	tk = kies_tijdkolom(header)
	data = []   # (regel, dt)
	fout_ts = 0
	soort = None
	for i, r in enumerate(rijen, start=2):  # regel 1 = kop
		try:
			dt, soort = parse_ts(r[tk])
			data.append((i, dt, r))
		except Exception:
			fout_ts += 1
	print("Tijdkolom: %s, formaat: %s, onleesbare tijden: %d" % (tk, soort, fout_ts))
	if not data:
		print("Geen leesbare tijden; stuur me de eerste 3 regels van het bestand.")
		return
	aware = data[0][1].tzinfo is not None
	print("Eerste rij: %s | laatste rij: %s" % (data[0][1].isoformat(), data[-1][1].isoformat()))

	# 1. Gaten en sprongen
	print("\n[1] Gaten en sprongen in de tijd")
	achter, gaten = [], []
	for (l0, a, _), (l1, b, _) in zip(data, data[1:]):
		d = (b - a).total_seconds()
		if d < 0:
			achter.append((l1, a, b, d))
		elif d > 3:
			gaten.append((l1, a, b, d))
	if aware:
		print("Tijd met offset/epoch: een terugsprong betekent een echt probleem.")
	else:
		print("Lokale tijd zonder offset: precies één terugsprong van ±3600 s rond 03:00 is normaal.")
	print("Terugsprongen: %d" % len(achter))
	for l1, a, b, d in achter[:5]:
		print("  regel %d: %s -> %s (%.0f s)" % (l1, a.strftime("%H:%M:%S"), b.strftime("%H:%M:%S"), d))
	print("Gaten > 3 s: %d" % len(gaten))
	for l1, a, b, d in gaten[:10]:
		print("  regel %d: %s -> %s (%.0f s)" % (l1, hms(a), hms(b), d))
	if len(gaten) > 10:
		print("  ... en %d meer" % (len(gaten) - 10))

	# 2. Rijen per lokaal uur en aantal kwartieren
	print("\n[2] Rijen per uur (lokale klok) en kwartieren")
	fold = 0
	per_uur = Counter()
	kwartieren = set()
	vorige = None
	for l, dt, _ in data:
		w = lokaal(dt)
		if not aware and vorige is not None and w < vorige:
			fold += 1
		vorige = w
		per_uur[(fold, w.hour)] += 1
		off = w.utcoffset() if w.tzinfo else None
		kwartieren.add((w.date(), w.hour, w.minute // 15, fold, off))
	for (fo, h), n in sorted(per_uur.items(), key=lambda x: (x[0][0], x[0][1])):
		mark = ""
		if h == 2:
			mark = "  <- uur 02 (komt 2x voor als de wissel erin zit)"
		print("  %s%02d u: %d rijen%s" % ("(na terugsprong) " if fo else "", h, n, mark))
	print("Verschillende kwartieren: %d (verwacht 100 voor een volledige dag)" % len(kwartieren))
	print("Rijen met uur 02: %d (verwacht ±7200 als beide uren erin zitten)" %
		  sum(n for (fo, h), n in per_uur.items() if h == 2))

	# 3. Lege waarden
	print("\n[3] Lege of foute waarden")
	leeg_kol = Counter()
	leeg_venster = 0
	venster = []
	for l, dt, r in data:
		w = lokaal(dt)
		leeg = [k for k, v in r.items() if k and (v is None or v.strip() in EMPTY)]
		for k in leeg:
			leeg_kol[k] += 1
		in_venster = (w.hour == 1 and w.minute >= 55) or w.hour == 2 or (w.hour == 3 and w.minute <= 5)
		if leeg and in_venster:
			leeg_venster += 1
			if len(venster) < 5:
				venster.append((l, w.strftime("%H:%M:%S"), leeg[:4]))
	if leeg_kol:
		print("Kolommen met lege waarden over de hele dag (aantal rijen):")
		for k, n in leeg_kol.most_common(8):
			print("  %s: %d" % (k, n))
	else:
		print("Geen lege waarden in het hele bestand.")
	print("Rijen met een lege waarde tussen 01:55 en 03:05: %d" % leeg_venster)
	for l, t, ks in venster:
		print("  regel %d (%s): %s" % (l, t, ", ".join(ks)))

	# 4. Tellers en maandpiek
	print("\n[4] Importteller en maandpiek")
	imp = [k for k in header if ("import" in k.lower() or "1.8.0" in k) and "ts" not in k.lower().split("_")]
	piek = [k for k in header if ("month" in k.lower() or "1.6.0" in k) and not k.lower().endswith("_ts")]
	pts = [k for k in header if k.lower().endswith("_ts") and ("peak" in k.lower() or "month" in k.lower())]

	def getal(v):
		try:
			return float(v)
		except Exception:
			return None

	for k in imp[:3]:
		reeks = [(l, getal(r.get(k))) for l, _, r in data]
		reeks = [(l, v) for l, v in reeks if v is not None]
		neg = [(l, a, b) for (l, a), (_, b) in zip(reeks[1:], reeks) if a < b]
		# (l, nieuw, oud) -> nieuw < oud betekent daling
		neg = [(l, b, a) for (l, a, b) in neg]
		if reeks:
			print("  %s: eerste %.3f, laatste %.3f, dalingen: %d" % (k, reeks[0][1], reeks[-1][1], len(neg)))
			for l, oud, nieuw in neg[:3]:
				print("     regel %d: %.3f -> %.3f" % (l, oud, nieuw))
	for k in piek[:3]:
		gezien = []
		vorig = object()
		for l, dt, r in data:
			v = r.get(k)
			if v != vorig:
				gezien.append((l, hms(dt), v))
				vorig = v
		print("  %s: %d verschillende waarden" % (k, len(gezien)))
		for l, t, v in gezien[:6]:
			print("     vanaf regel %d (%s): %s" % (l, t, v))
	for k in pts[:2]:
		waarden = []
		vorig = object()
		for l, dt, r in data:
			v = r.get(k)
			if v != vorig:
				waarden.append((l, hms(dt), v))
				vorig = v
		print("  %s: %d wijzigingen, laatste: %s" % (k, len(waarden), waarden[-1][2] if waarden else "-"))

	# 5. Rijen rond de wissel
	print("\n[5] Rijen rond de wissel (tijd + eerste kolommen)")
	idx = None
	if achter:
		idx = next(i for i, (l, _, _) in enumerate(data) if l == achter[0][0])
	else:
		for i, (_, dt, _) in enumerate(data):
			if aware and dt.astimezone(timezone.utc).strftime("%H:%M:%S") == "01:00:00" and \
				dt.astimezone(timezone.utc).date() == datetime(2026, 10, 25).date():
				idx = i
				break
	if idx is None:
		print("  Geen herkenbaar wisselpunt gevonden.")
	else:
		extra = [h for h in header if h != tk][:3]
		for l, dt, r in data[max(0, idx - 2): idx + 3]:
			print("  regel %d: %s | %s" % (l, dt.isoformat(), " | ".join("%s=%s" % (k, r.get(k)) for k in extra)))

	print("=" * 20 + " KOPIEER TOT HIER " + "=" * 22)


if __name__ == "__main__":
	main()
