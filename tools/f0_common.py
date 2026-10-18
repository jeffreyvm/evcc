"""Gedeelde hulpfuncties voor de F0-scripts: HomeWizard P1, Alfen (Modbus TCP) en Indevolt (lokale RPC).

Alleen Python 3.11-standaardbibliotheek; geen pip nodig.
"""
from __future__ import annotations

import json
import socket
import struct
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any

# ---------------------------------------------------------------------------
# HomeWizard P1 (lokale API v1, moet aanstaan in de HomeWizard-app)
# ---------------------------------------------------------------------------

@dataclass
class P1Sample:
    power_w: float | None = None          # netvermogen (+ afname, − injectie)
    import_kwh: float | None = None       # totaal afname (dag + nacht)
    export_kwh: float | None = None       # totaal injectie
    avg15_w: float | None = None          # lopend kwartiergemiddelde van de meter (OBIS 1-0:1.4.0)
    monthpeak_w: float | None = None      # maandpiek van de meter (OBIS 1-0:1.6.0)
    monthpeak_ts: str | None = None       # tijdstip van die maandpiek (yymmddhhmmss)
    error: str | None = None


def read_p1(host: str, timeout: float = 0.8) -> P1Sample:
    """Leest /api/v1/data van de HomeWizard P1. Alle velden optioneel (oudere firmware heeft er minder)."""
    s = P1Sample()
    try:
        with urllib.request.urlopen(f"http://{host}/api/v1/data", timeout=timeout) as r:
            d = json.load(r)
    except Exception as e:  # noqa: BLE001
        s.error = _short_err(e)
        return s
    s.power_w = _num(d.get("active_power_w"))
    s.import_kwh = _num(d.get("total_power_import_kwh"))
    if s.import_kwh is None:  # oudere firmware: t1 + t2
        t1, t2 = _num(d.get("total_power_import_t1_kwh")), _num(d.get("total_power_import_t2_kwh"))
        if t1 is not None and t2 is not None:
            s.import_kwh = t1 + t2
    s.export_kwh = _num(d.get("total_power_export_kwh"))
    if s.export_kwh is None:
        t1, t2 = _num(d.get("total_power_export_t1_kwh")), _num(d.get("total_power_export_t2_kwh"))
        if t1 is not None and t2 is not None:
            s.export_kwh = t1 + t2
    s.avg15_w = _num(d.get("active_power_average_w"))
    # In API v1 heet het veld met een tikfout "montly_power_peak_w"; we proberen beide spellingen.
    s.monthpeak_w = _num(d.get("montly_power_peak_w", d.get("monthly_power_peak_w")))
    s.monthpeak_ts = d.get("montly_power_peak_timestamp", d.get("monthly_power_peak_timestamp"))
    if s.monthpeak_ts is not None:
        s.monthpeak_ts = str(s.monthpeak_ts)
    return s


# ---------------------------------------------------------------------------
# Alfen Eve Single Pro via Modbus TCP (zelfde registers als evcc, charger/alfen.go)
# ---------------------------------------------------------------------------

ALFEN_PORT = int(__import__("os").environ.get("F0_ALFEN_PORT", 502))  # omgevingsvariabele alleen voor tests
ALFEN_UNIT_SOCKET = 1   # laadpunt 1; register 200+ (stationgegevens) zit op unit 200

# Registeradressen (Alfen "Modbus Slave TCP/IP"-document)
ALFEN_REG_VOLTAGES = 306   # 3 × float32
ALFEN_REG_CURRENTS = 320   # 3 × float32 (A)
ALFEN_REG_POWER = 344      # float32 (W, som fases)
ALFEN_REG_ENERGY = 374     # float64 (Wh)
ALFEN_REG_STATUS = 1201    # 5 registers ASCII, Mode-3-status (A1, B1, C2, ...)
ALFEN_REG_APPLIED = 1206   # float32: werkelijk toegepaste maximumstroom (A)
ALFEN_REG_VALID_TIME = 1208  # uint32: resterende geldigheidstijd van het setpoint (s)
ALFEN_REG_SETPOINT = 1210  # float32: Modbus-setpoint maximumstroom (A); evcc schrijft hier
ALFEN_REG_SAFE = 1212      # float32: safe current bij uitblijven van commando's (A)
ALFEN_REG_PHASES = 1215    # uint16: 1 of 3 fases


@dataclass
class AlfenSample:
    current_a: float | None = None
    power_w: float | None = None
    status: str | None = None
    setpoint_a: float | None = None
    applied_a: float | None = None
    valid_s: int | None = None
    safe_a: float | None = None
    energy_wh: float | None = None
    error: str | None = None


class ModbusTcp:
    """Minimale Modbus-TCP-client (alleen FC3 lezen). Houdt de verbinding open; herverbindt bij fouten."""

    def __init__(self, host: str, port: int = ALFEN_PORT, timeout: float = 0.8):
        self.host, self.port, self.timeout = host, port, timeout
        self.sock: socket.socket | None = None
        self.tid = 0

    def _connect(self) -> None:
        self.close()
        self.sock = socket.create_connection((self.host, self.port), timeout=self.timeout)
        self.sock.settimeout(self.timeout)

    def close(self) -> None:
        if self.sock:
            try:
                self.sock.close()
            finally:
                self.sock = None

    def read_holding(self, unit: int, addr: int, count: int) -> bytes:
        for attempt in (0, 1):
            try:
                if self.sock is None:
                    self._connect()
                self.tid = (self.tid + 1) & 0xFFFF
                req = struct.pack(">HHHBBHH", self.tid, 0, 6, unit, 3, addr, count)
                self.sock.sendall(req)
                hdr = self._recv_exact(7)
                tid, _proto, length, _unit = struct.unpack(">HHHB", hdr)
                body = self._recv_exact(length - 1)
                if body[0] & 0x80:
                    raise OSError(f"modbus exception {body[1]} bij register {addr}")
                if body[0] != 3 or body[1] != count * 2:
                    raise OSError(f"onverwacht antwoord fc={body[0]} len={body[1]}")
                return body[2:2 + count * 2]
            except (OSError, struct.error) as e:
                self.close()
                if attempt == 1:
                    raise OSError(_short_err(e)) from e
        raise OSError("onbereikbaar")

    def _recv_exact(self, n: int) -> bytes:
        buf = b""
        while len(buf) < n:
            chunk = self.sock.recv(n - len(buf))
            if not chunk:
                raise OSError("verbinding gesloten")
            buf += chunk
        return buf


def read_alfen(mb: ModbusTcp, extended: bool = True) -> AlfenSample:
    s = AlfenSample()
    try:
        raw = mb.read_holding(ALFEN_UNIT_SOCKET, ALFEN_REG_CURRENTS, 6)
        i1, _i2, _i3 = struct.unpack(">fff", raw)
        s.current_a = round(i1, 2)
        s.power_w = round(struct.unpack(">f", mb.read_holding(ALFEN_UNIT_SOCKET, ALFEN_REG_POWER, 2))[0], 1)
        s.status = mb.read_holding(ALFEN_UNIT_SOCKET, ALFEN_REG_STATUS, 5).decode("ascii", "replace").strip("\x00 ")
        s.setpoint_a = round(struct.unpack(">f", mb.read_holding(ALFEN_UNIT_SOCKET, ALFEN_REG_SETPOINT, 2))[0], 2)
        if extended:
            s.applied_a = round(struct.unpack(">f", mb.read_holding(ALFEN_UNIT_SOCKET, ALFEN_REG_APPLIED, 2))[0], 2)
            s.valid_s = struct.unpack(">I", mb.read_holding(ALFEN_UNIT_SOCKET, ALFEN_REG_VALID_TIME, 2))[0]
            s.safe_a = round(struct.unpack(">f", mb.read_holding(ALFEN_UNIT_SOCKET, ALFEN_REG_SAFE, 2))[0], 2)
            s.energy_wh = round(struct.unpack(">d", mb.read_holding(ALFEN_UNIT_SOCKET, ALFEN_REG_ENERGY, 4))[0], 1)
    except OSError as e:
        s.error = str(e)
    return s


# ---------------------------------------------------------------------------
# Indevolt PowerFlex 2000 via de lokale RPC op poort 8080 (zoals het evcc-template)
# ---------------------------------------------------------------------------

# Registers (datapunten) uit het evcc-template indevolt-battery.yaml
IDV_POWER = 6000        # W; template zegt: + laden, − ontladen
IDV_SOC = 6002          # %
IDV_DISCHARGE_KWH = 6007
IDV_GRID_IN = 2101      # AC-ingangsvermogen (W)
IDV_GRID_OUT = 2108     # AC-uitgangsvermogen (W)
IDV_MODE = 47005        # 1 = zelfverbruik, 4 = real-time control
IDV_GRIDCHARGE = 1143   # 0/1 netladen toegelaten
IDV_RTC = 47015         # [status, vermogen W, SoC-grens]; status 0 = stand-by, 1 = laden


class Indevolt:
    def __init__(self, name: str, host: str, user: str | None = None, password: str | None = None,
                 timeout: float = 2.0):
        self.name, self.host, self.timeout = name, host, timeout
        self.base = f"http://{host}{'' if ':' in host else ':8080'}/rpc/"
        if user:
            mgr = urllib.request.HTTPPasswordMgrWithDefaultRealm()
            mgr.add_password(None, self.base, user, password or "")
            self.opener = urllib.request.build_opener(urllib.request.HTTPDigestAuthHandler(mgr))
        else:
            self.opener = urllib.request.build_opener()

    def _rpc(self, method: str, config: dict[str, Any]) -> tuple[Any, float]:
        url = self.base + method + "?config=" + urllib.parse.quote(json.dumps(config, separators=(",", ":")))
        req = urllib.request.Request(url, method="POST", data=b"")
        t0 = time.perf_counter()
        with self.opener.open(req, timeout=self.timeout) as r:
            body = r.read()
        ms = (time.perf_counter() - t0) * 1000
        try:
            return json.loads(body), ms
        except json.JSONDecodeError:
            return body.decode("utf-8", "replace"), ms

    def get(self, *points: int) -> tuple[dict[str, Any], float]:
        """Leest datapunten. Antwoord: {"6000": -512, "6002": 61}."""
        data, ms = self._rpc("Indevolt.GetData", {"t": list(points)})
        if not isinstance(data, dict):
            raise OSError(f"GetData gaf geen JSON-object: {data!r}")
        return data, ms

    def set(self, point: int, values: list[int]) -> tuple[Any, float]:
        """Schrijft een register (f=16 = Modbus 'write multiple'). Antwoord: {"result": ...}."""
        return self._rpc("Indevolt.SetData", {"f": 16, "t": point, "v": values})

    # Dezelfde drie modi als het evcc-template
    def mode_normal(self) -> list[tuple[str, Any, float]]:
        return [("47005=[1]", *self.set(IDV_MODE, [1])), ("1143=[0]", *self.set(IDV_GRIDCHARGE, [0]))]

    def mode_hold(self, minsoc: int) -> list[tuple[str, Any, float]]:
        return [("47005=[4]", *self.set(IDV_MODE, [4])), (f"47015=[0,50,{minsoc}]", *self.set(IDV_RTC, [0, 50, minsoc]))]

    def mode_charge(self, power_w: int, maxsoc: int) -> list[tuple[str, Any, float]]:
        return [("47005=[4]", *self.set(IDV_MODE, [4])), ("1143=[1]", *self.set(IDV_GRIDCHARGE, [1])),
                (f"47015=[1,{power_w},{maxsoc}]", *self.set(IDV_RTC, [1, power_w, maxsoc]))]


# ---------------------------------------------------------------------------
# klein grut
# ---------------------------------------------------------------------------

def _num(v: Any) -> float | None:
    try:
        return None if v is None else float(v)
    except (TypeError, ValueError):
        return None


def _short_err(e: Exception) -> str:
    if isinstance(e, urllib.error.HTTPError):
        return f"HTTP {e.code}"
    if isinstance(e, (socket.timeout, TimeoutError)):
        return "timeout"
    return f"{type(e).__name__}: {str(e)[:60]}"


def fmt(v: Any) -> str:
    """CSV-waarde: leeg bij None, anders compact."""
    if v is None:
        return ""
    if isinstance(v, float):
        return f"{v:.3f}".rstrip("0").rstrip(".") if v != int(v) else str(int(v))
    return str(v)
