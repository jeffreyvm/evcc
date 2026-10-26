#!/usr/bin/env python3
"""Meet per register hoe lang de Alfen over Modbus TCP nodig heeft (alleen lezen, FC3).

Gebruik in de evcc-LXC, met het logscript tijdelijk gestopt zodat er maar één extra verbinding is:
    systemctl stop f0-logger
    python3 /opt/f0/f0_alfen_latency.py            # leest alfen_host uit /etc/f0/logger.json of /opt/f0/logger.json
    python3 /opt/f0/f0_alfen_latency.py 192.168.107.131
    systemctl start f0-logger
Het script schrijft niets naar de Alfen en print geen tokens.
"""
import json
import statistics
import struct
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import f0_common as c  # noqa: E402

CYCLES = 20
REGS = [
    ("currents", c.ALFEN_REG_CURRENTS, 6),
    ("power", c.ALFEN_REG_POWER, 2),
    ("status", c.ALFEN_REG_STATUS, 5),
    ("setpoint", c.ALFEN_REG_SETPOINT, 2),
    ("applied", c.ALFEN_REG_APPLIED, 2),
    ("valid_time", c.ALFEN_REG_VALID_TIME, 2),
    ("safe", c.ALFEN_REG_SAFE, 2),
    ("energy", c.ALFEN_REG_ENERGY, 4),
]


def host():
    if len(sys.argv) > 1:
        return sys.argv[1]
    for pad in ("/etc/f0/logger.json", "/opt/f0/logger.json"):
        try:
            with open(pad) as f:
                return json.load(f)["alfen_host"]
        except (OSError, KeyError, ValueError):
            continue
    sys.exit("alfen_host niet gevonden; geef het IP van de Alfen als argument mee.")


def main():
    h = host()
    print("Alfen:", h, "poort", c.ALFEN_PORT, "| time-out 3,0 s | %d rondes" % CYCLES)
    t0 = time.time()
    mb = c.ModbusTcp(h, timeout=3.0)
    try:
        mb._connect()
        print("Verbinden: %.0f ms" % ((time.time() - t0) * 1000))
    except OSError as e:
        sys.exit("Verbinden mislukt: %s" % e)

    tijden = {n: [] for n, _, _ in REGS}
    fouten = {n: 0 for n, _, _ in REGS}
    ronde = []
    for _ in range(CYCLES):
        r0 = time.time()
        for n, addr, cnt in REGS:
            t = time.time()
            try:
                mb.read_holding(c.ALFEN_UNIT_SOCKET, addr, cnt)
                tijden[n].append((time.time() - t) * 1000)
            except OSError as e:
                fouten[n] += 1
                print("  fout bij %s: %s (%.0f ms)" % (n, e, (time.time() - t) * 1000))
        ronde.append((time.time() - r0) * 1000)
        rest = 1.0 - (time.time() - r0)
        if rest > 0:
            time.sleep(rest)
    mb.close()

    print("\nPer register (ms): min / mediaan / max / fouten")
    for n, _, _ in REGS:
        t = tijden[n]
        if t:
            print("  %-10s %6.0f / %6.0f / %6.0f / %d" % (n, min(t), statistics.median(t), max(t), fouten[n]))
        else:
            print("  %-10s geen enkele geslaagde lezing, %d fouten" % (n, fouten[n]))
    print("\nVolledige ronde van 8 lezingen (ms): min %.0f, mediaan %.0f, max %.0f" %
          (min(ronde), statistics.median(ronde), max(ronde)))
    print("Het logscript geeft een ronde 900 ms; daarboven wordt de rij 'alfen:late'.")


if __name__ == "__main__":
    main()
