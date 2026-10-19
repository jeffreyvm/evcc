package capacity

import (
	"compress/gzip"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sample is one row of the f0 logger (tools/f0_logger.py)
type sample struct {
	t         time.Time
	grid      float64 // W
	ev        float64 // W
	connected bool
	monthPeak float64 // W, meter register 1.6.0
	battery   float64 // W the batteries could deliver
}

// replayConfig holds the assumptions of a replay
type replayConfig struct {
	Mode          Mode
	MinPower      float64 // W
	MaxPower      float64 // W
	Step          time.Duration
	UnitPower     float64 // W per battery unit above its minimum soc
	BatteryMinSoc float64 // %
}

func defaultReplayConfig() replayConfig {
	return replayConfig{Mode: ModeFast, MinPower: 1380, MaxPower: 7360, Step: 10 * time.Second, UnitPower: 1000, BatteryMinSoc: 15}
}

// slotResult compares a slot as it happened with the simulated control
type slotResult struct {
	start           time.Time
	actual, sim     float64 // W slot average
	evActual, evSim float64 // W slot average
	exceeded        bool
}

// replayResult summarises a replay
type replayResult struct {
	slots             []slotResult
	demand, delivered float64 // kWh of the vehicle
	pauses, restarts  int
}

// replay runs the limiter against recorded data. Home power is taken as grid minus vehicle;
// the vehicle's recorded energy is its demand, charged as fast as the ceiling allows.
func replay(s Settings, cfg replayConfig, samples []sample, loc *time.Location) replayResult {
	var res replayResult
	if len(samples) == 0 {
		return res
	}

	l := NewLimiter(s)

	var (
		backlog    float64 // kWh still wanted by the vehicle
		evSim      float64 // W simulated vehicle power
		slot       time.Time
		slotEnergy float64 // kWh simulated import in the slot
		monthPeak  = samples[0].monthPeak
		month      = MonthStart(samples[0].t, loc)
		cur        *slotResult
		actualE    float64
		evActualE  float64
		evSimE     float64
		prevPaused bool
	)

	closeSlot := func() {
		if cur == nil {
			return
		}
		h := slotHours
		cur.actual, cur.sim = actualE*1e3/h, slotEnergy*1e3/h
		cur.evActual, cur.evSim = evActualE*1e3/h, evSimE*1e3/h
		cur.exceeded = cur.sim > Target(s, monthPeak)+s.Margin+1
		res.slots = append(res.slots, *cur)
		monthPeak = math.Max(monthPeak, cur.sim)
	}

	next := samples[0].t
	for i, smp := range samples {
		if smp.t.Before(next) && i != len(samples)-1 {
			continue
		}

		dt := cfg.Step
		if i > 0 {
			dt = min(smp.t.Sub(next.Add(-cfg.Step)), 5*time.Minute)
		}
		next = smp.t.Add(cfg.Step)

		if m := MonthStart(smp.t, loc); !m.Equal(month) {
			month, monthPeak = m, 0
		}

		if ss := SlotStart(smp.t); !ss.Equal(slot) {
			closeSlot()
			slot, slotEnergy, actualE, evActualE, evSimE = ss, 0, 0, 0, 0
			cur = &slotResult{start: ss}
		}

		home := math.Max(0, smp.grid-smp.ev)
		if smp.grid < 0 {
			home = smp.grid - smp.ev // export: surplus net of the vehicle
		}

		backlog += smp.ev * dt.Hours() / 1e3
		res.demand += smp.ev * dt.Hours() / 1e3

		grid := home + evSim
		b := Compute(s, Measurement{Time: smp.t, GridPower: grid, SlotEnergy: slotEnergy, CountedFrom: slot, MonthPeak: monthPeak})

		connected := smp.connected || backlog > 0.01
		d := l.Limit(b, grid, LoadpointState{
			Now: smp.t.In(loc), Mode: cfg.Mode, Connected: connected,
			ChargePower: evSim, MinPower: cfg.MinPower, MaxPower: cfg.MaxPower,
			BatteryAvailable: smp.battery,
		})

		if l.Paused() && !prevPaused {
			res.pauses++
		}
		if !l.Paused() && prevPaused {
			res.restarts++
		}
		prevPaused = l.Paused()

		want := math.Min(cfg.MaxPower, backlog*1e3/dt.Hours())
		if backlog <= 0 {
			want = 0
		}
		evSim = math.Min(want, d.MaxPower)
		if evSim < cfg.MinPower {
			evSim = 0
			if want > 0 && d.MaxPower >= cfg.MinPower {
				evSim = math.Min(cfg.MinPower, want)
			}
		}

		e := evSim * dt.Hours() / 1e3
		backlog -= e
		res.delivered += e
		evSimE += e
		evActualE += smp.ev * dt.Hours() / 1e3
		actualE += math.Max(0, smp.grid) * dt.Hours() / 1e3
		slotEnergy += math.Max(0, home+evSim) * dt.Hours() / 1e3
	}
	closeSlot()

	return res
}

// readF0 reads a (gzipped) f0 logger CSV
func readF0(t *testing.T, path string, cfg replayConfig) []sample {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		require.NoError(t, err)
		r = gz
	}

	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	require.NoError(t, err)

	col := make(map[string]int, len(head))
	for i, h := range head {
		col[h] = i
	}

	num := func(row []string, name string) (float64, bool) {
		i, ok := col[name]
		if !ok || i >= len(row) || row[i] == "" {
			return 0, false
		}
		v, err := strconv.ParseFloat(row[i], 64)
		return v, err == nil && !math.IsNaN(v)
	}

	var res []sample
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		epoch, ok := num(row, "epoch")
		grid, okGrid := num(row, "p1_power_w")
		if !ok || !okGrid {
			continue
		}

		smp := sample{t: time.Unix(int64(epoch), 0), grid: grid}
		smp.ev, _ = num(row, "alfen_p_w")
		smp.monthPeak, _ = num(row, "p1_monthpeak_w")
		if i, ok := col["alfen_status"]; ok && i < len(row) {
			smp.connected = strings.HasPrefix(row[i], "B") || strings.HasPrefix(row[i], "C")
		}
		for _, unit := range []string{"garage", "zolder"} {
			if soc, ok := num(row, unit+"_soc"); ok && soc > cfg.BatteryMinSoc {
				smp.battery += cfg.UnitPower
			}
		}

		res = append(res, smp)
	}

	sort.Slice(res, func(i, j int) bool { return res[i].t.Before(res[j].t) })
	return res
}

// TestReplayF0 replays a recorded day: F0_REPLAY=/var/log/f0/f0-2026-10-25.csv.gz go test ./core/capacity -run ReplayF0 -v
// F0_REPLAY_MODE=smart replays smart instead of fast charging.
func TestReplayF0(t *testing.T) {
	path := os.Getenv("F0_REPLAY")
	if path == "" {
		t.Skip("F0_REPLAY not set")
	}

	loc := brussels(t)
	s := DefaultSettings()
	cfg := defaultReplayConfig()
	if os.Getenv("F0_REPLAY_MODE") == "smart" {
		cfg.Mode = ModeSmart
	}

	samples := readF0(t, path, cfg)
	require.NotEmpty(t, samples)

	res := replay(s, cfg, samples, loc)
	t.Log(formatReplay(res, loc))
}

func formatReplay(res replayResult, loc *time.Location) string {
	var sb strings.Builder
	var maxActual, maxSim float64
	var exceeded int

	fmt.Fprintf(&sb, "\n%-17s %8s %8s %8s %8s\n", "kwartier", "net nu", "net F1", "auto nu", "auto F1")
	for _, r := range res.slots {
		maxActual, maxSim = math.Max(maxActual, r.actual), math.Max(maxSim, r.sim)
		mark := ""
		if r.exceeded {
			exceeded++
			mark = "  boven doel"
		}
		if r.evActual > 0 || r.evSim > 0 || r.exceeded || r.actual > 2500 {
			fmt.Fprintf(&sb, "%-17s %8.0f %8.0f %8.0f %8.0f%s\n", r.start.In(loc).Format("02/01 15:04 MST"), r.actual, r.sim, r.evActual, r.evSim, mark)
		}
	}

	fmt.Fprintf(&sb, "\nhoogste kwartier: nu %.0f W, met F1 %.0f W; kwartieren boven het doel: %d\n", maxActual, maxSim, exceeded)
	fmt.Fprintf(&sb, "auto: gevraagd %.2f kWh, geladen %.2f kWh; onderbrekingen %d, herstarts %d\n", res.demand, res.delivered, res.pauses, res.restarts)

	return sb.String()
}

// TestReplaySynthetic checks the replay against a constructed evening: house load with a
// cooking peak and a vehicle that arrives at 17:30 wanting 20 kWh at full power
func TestReplaySynthetic(t *testing.T) {
	loc := brussels(t)
	s := DefaultSettings()
	cfg := defaultReplayConfig()

	start := time.Date(2026, 11, 3, 16, 0, 0, 0, loc)
	var samples []sample
	evLeft := 20.0 // kWh

	for d := time.Duration(0); d < 16*time.Hour; d += time.Second {
		now := start.Add(d)
		h := now.Hour()

		home := 400.0
		if h == 18 && now.Minute() < 40 {
			home = 3200 // oven and hob
		}

		var ev float64
		if now.After(start.Add(90*time.Minute)) && evLeft > 0 {
			ev = 7360
			evLeft -= ev / 3600 / 1e3
		}

		samples = append(samples, sample{t: now, grid: home + ev, ev: ev, connected: now.After(start.Add(90 * time.Minute))})
	}

	res := replay(s, cfg, samples, loc)

	var maxSim float64
	for _, r := range res.slots {
		maxSim = math.Max(maxSim, r.sim)
	}

	// the oven alone needs 3.2 kW for 40 minutes and has no battery: that peak stays
	assert.InDelta(t, 3200, maxSim, 50, "peak set by the house, not by the vehicle")

	// the vehicle charges at most at minimum current in the cooking window and is
	// limited to the target outside it, but delivers its energy by the morning
	assert.InDelta(t, res.demand, res.delivered, 0.05, "energy delivered")

	for _, r := range res.slots {
		local := r.start.In(loc)
		if local.Hour() >= 20 && r.evSim > 0 {
			// after the oven the month peak is 3.2 kW, so the target follows it
			assert.LessOrEqual(t, r.sim, 3200.0+1, local.Format("15:04"))
		}
	}

	t.Log(formatReplay(res, loc))
}
