#!/usr/bin/env python3
"""Haalt de uurstatistieken van een energiesensor uit Home Assistant en schrijft ze als CSV voor
f0_businesscase.py (--ev ... --ev-kind energy_kwh).

HA bewaart de gewone geschiedenis maar 10 dagen, maar de uurstatistieken van energiesensoren blijven bewaard.
Twee manieren, alleen Python-standaardbibliotheek:

1. Via de API van HA (geen bestandstoegang nodig). Maak in HA een token: je profiel → Beveiliging →
   Langlevende toegangstokens → Token aanmaken.
     python3 f0_ha_export.py --url http://IP-VAN-HA:8123 --token TOKEN --list
     python3 f0_ha_export.py --url http://IP-VAN-HA:8123 --token TOKEN --id sensor.alfen_energy --out /root/alfen.csv
   Het token mag ook in de omgevingsvariabele HA_TOKEN staan (dan komt het niet in je shellgeschiedenis).

2. Uit een KOPIE van home-assistant_v2.db:
     python3 f0_ha_export.py --db /tmp/ha.db --list
     python3 f0_ha_export.py --db /tmp/ha.db --id sensor.alfen_energy --out /root/alfen.csv
"""
from __future__ import annotations

import argparse
import base64
import csv
import json
import os
import socket
import sqlite3
import ssl
import struct
import sys
import urllib.parse
from datetime import datetime, timedelta, timezone

UTC = timezone.utc


# ---------------------------------------------------------------------------
# minimale WebSocket-client (HA's statistieken zitten alleen in de WebSocket-API)
# ---------------------------------------------------------------------------

class WS:
    def __init__(self, url: str, timeout: float = 60):
        u = urllib.parse.urlparse(url)
        secure = u.scheme in ("https", "wss")
        host, port = u.hostname, u.port or (443 if secure else 80)
        raw = socket.create_connection((host, port), timeout=timeout)
        self.s = ssl.create_default_context().wrap_socket(raw, server_hostname=host) if secure else raw
        key = base64.b64encode(os.urandom(16)).decode()
        req = (f"GET /api/websocket HTTP/1.1\r\nHost: {host}:{port}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
               f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n")
        self.s.sendall(req.encode())
        head = b""
        while b"\r\n\r\n" not in head:
            chunk = self.s.recv(4096)
            if not chunk:
                raise OSError("verbinding gesloten tijdens de handshake")
            head += chunk
        status = head.split(b"\r\n", 1)[0].decode(errors="replace")
        if " 101 " not in status:
            raise OSError(f"geen WebSocket-verbinding: {status}")
        self.buf = head.split(b"\r\n\r\n", 1)[1]

    def _recv(self, n: int) -> bytes:
        while len(self.buf) < n:
            chunk = self.s.recv(65536)
            if not chunk:
                raise OSError("verbinding gesloten")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def send(self, obj: dict) -> None:
        data = json.dumps(obj).encode()
        hdr = bytearray([0x81])
        n = len(data)
        if n < 126:
            hdr.append(0x80 | n)
        elif n < 65536:
            hdr.append(0x80 | 126)
            hdr += struct.pack(">H", n)
        else:
            hdr.append(0x80 | 127)
            hdr += struct.pack(">Q", n)
        mask = os.urandom(4)
        self.s.sendall(bytes(hdr) + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))

    def recv(self) -> dict:
        msg = b""
        while True:
            b0, b1 = self._recv(2)
            n = b1 & 0x7F
            if n == 126:
                n = struct.unpack(">H", self._recv(2))[0]
            elif n == 127:
                n = struct.unpack(">Q", self._recv(8))[0]
            mask = self._recv(4) if b1 & 0x80 else None
            payload = self._recv(n)
            if mask:
                payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
            op = b0 & 0x0F
            if op == 0x8:
                raise OSError("server sloot de verbinding")
            if op == 0x9:  # ping → pong
                self.s.sendall(bytes([0x8A, 0x80 | len(payload)]) + b"\0\0\0\0" + payload)
                continue
            if op in (0x1, 0x0):
                msg += payload
                if b0 & 0x80:
                    return json.loads(msg)


class HA:
    def __init__(self, url: str, token: str):
        self.ws = WS(url)
        self.id = 0
        first = self.ws.recv()
        if first.get("type") != "auth_required":
            raise OSError(f"onverwacht antwoord van HA: {first}")
        self.ws.send({"type": "auth", "access_token": token})
        res = self.ws.recv()
        if res.get("type") != "auth_ok":
            raise SystemExit(f"HA weigert het token: {res.get('message', res)}")

    def call(self, **msg):
        self.id += 1
        msg["id"] = self.id
        self.ws.send(msg)
        while True:
            res = self.ws.recv()
            if res.get("id") == self.id:
                if not res.get("success"):
                    raise SystemExit(f"HA-fout op {msg['type']}: {res.get('error')}")
                return res["result"]


def _ts(v) -> datetime:
    """HA geeft tijden als milliseconden (nieuwe versies) of ISO-tekst (oude versies)."""
    if isinstance(v, (int, float)):
        return datetime.fromtimestamp(v / 1000, UTC)
    return datetime.fromisoformat(str(v).replace("Z", "+00:00")).astimezone(UTC)


def api_list(ha: HA) -> None:
    rows = ha.call(type="recorder/list_statistic_ids", statistic_type="sum")
    rows = [r for r in rows if (r.get("statistics_unit_of_measurement") or r.get("unit_of_measurement")) in ("kWh", "Wh", "MWh")]
    for r in sorted(rows, key=lambda r: r["statistic_id"]):
        unit = r.get("statistics_unit_of_measurement") or r.get("unit_of_measurement")
        print(f"{r['statistic_id']:60} {unit:4} {r.get('name') or ''}")
    print("\nKies de energieteller van de laadpaal en draai opnieuw met --id <statistic_id>.")


def api_export(ha: HA, sid: str, start: datetime, out: str) -> int:
    points: dict[datetime, float] = {}
    t = start
    now = datetime.now(UTC)
    while t < now:  # per 3 maanden, zodat de antwoorden klein blijven
        t_end = min(t + timedelta(days=92), now)
        res = ha.call(type="recorder/statistics_during_period", start_time=t.isoformat(), end_time=t_end.isoformat(),
                      statistic_ids=[sid], period="hour", types=["sum"], units={"energy": "kWh"})
        for r in res.get(sid, []):
            if r.get("sum") is not None:
                points[_ts(r["end"]) if r.get("end") is not None else _ts(r["start"]) + timedelta(hours=1)] = float(r["sum"])
        t = t_end
    return _write(points, out)


# ---------------------------------------------------------------------------
# uit een kopie van de SQLite-database
# ---------------------------------------------------------------------------

def db_list(con) -> None:
    rows = con.execute("""SELECT m.statistic_id, m.unit_of_measurement, COUNT(*)
                          FROM statistics s JOIN statistics_meta m ON s.metadata_id = m.id
                          WHERE m.unit_of_measurement IN ('kWh', 'Wh', 'MWh') GROUP BY m.id ORDER BY m.statistic_id""").fetchall()
    for sid, unit, n in rows:
        print(f"{sid:60} {unit:4} {n:6} uren")
    print("\nKies de energieteller van de laadpaal en draai opnieuw met --id <statistic_id>.")


def db_export(con, sid: str, out: str) -> int:
    meta = con.execute("SELECT id, unit_of_measurement FROM statistics_meta WHERE statistic_id = ?", (sid,)).fetchone()
    if not meta:
        sys.exit(f"{sid} niet gevonden; probeer --list")
    mid, unit = meta
    factor = {"Wh": 0.001, "MWh": 1000.0}.get(unit, 1.0)
    cols = [r[1] for r in con.execute("PRAGMA table_info(statistics)")]
    t_col = "start_ts" if "start_ts" in cols else "start"
    points = {}
    for t, s in con.execute(f"SELECT {t_col}, sum FROM statistics WHERE metadata_id = ? AND sum IS NOT NULL ORDER BY {t_col}", (mid,)):
        start = datetime.fromtimestamp(float(t), UTC) if t_col == "start_ts" else \
            datetime.fromisoformat(str(t).replace(" ", "T")).replace(tzinfo=UTC)
        points[start + timedelta(hours=1)] = float(s) * factor   # 'sum' = teller op het EINDE van het uur
    return _write(points, out)


def _write(points: dict[datetime, float], out: str) -> int:
    if not points:
        sys.exit("geen uurstatistieken gevonden voor deze sensor in die periode")
    with open(out, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["time", "value"])
        for t in sorted(points):
            w.writerow([t.strftime("%Y-%m-%dT%H:%M:%SZ"), f"{points[t]:.4f}"])
    first, last = min(points), max(points)
    print(f"{len(points)} uren geschreven naar {out}, van {first.astimezone():%d-%m-%Y} tot {last.astimezone():%d-%m-%Y}")
    return len(points)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", help="HA-adres, bv. http://192.168.1.10:8123")
    ap.add_argument("--token", default=os.environ.get("HA_TOKEN"), help="langlevend toegangstoken (of HA_TOKEN)")
    ap.add_argument("--db", help="kopie van home-assistant_v2.db (in plaats van --url)")
    ap.add_argument("--list", action="store_true", help="toon energiesensoren met uurstatistieken")
    ap.add_argument("--id", help="statistic_id, bv. sensor.alfen_energy")
    ap.add_argument("--from", dest="start", default="2025-09-01", help="begindatum (standaard 2025-09-01)")
    ap.add_argument("--out", default="alfen.csv")
    a = ap.parse_args()

    if a.db:
        con = sqlite3.connect(f"file:{a.db}?mode=ro", uri=True)
        db_list(con) if a.list or not a.id else db_export(con, a.id, a.out)
        return
    if not (a.url and a.token):
        sys.exit("Geef --url en --token (of HA_TOKEN), of --db met een kopie van de database.")
    ha = HA(a.url, a.token)
    if a.list or not a.id:
        api_list(ha)
    else:
        api_export(ha, a.id, datetime.fromisoformat(a.start).replace(tzinfo=UTC), a.out)


if __name__ == "__main__":
    main()
