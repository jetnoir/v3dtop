// v3dtop — a btop-style live monitor for the Raspberry Pi V3D GPU.
//
// Zero third-party dependencies (Go standard library only). Reads real
// per-engine GPU utilisation from DRM fdinfo (drm-engine-* nanosecond
// counters), clock/thermal/throttle from vcgencmd + debugfs, the static
// capability sheet from v3d_ident, and — optionally — live job/submit rates
// from the v3d and gpu_scheduler ftrace tracepoints via a private trace
// instance that never touches the global buffer.
//
// Copyright (C) 2026 Stuart Thomas. Dual-licensed; see LICENSE.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

const version = "0.1.0"

const (
	driPath   = "/sys/kernel/debug/dri/0"
	traceRoot = "/sys/kernel/tracing"
	instName  = "v3dtop"
)

// The six V3D job queues the kernel exposes as drm-engine-* counters.
var engines = []string{"render", "bin", "csd", "tfu", "cpu", "cache_clean"}
var engLabel = map[string]string{
	"render": "RENDER", "bin": "BIN", "csd": "CSD  compute",
	"tfu": "TFU  texture", "cpu": "CPU jobs", "cache_clean": "CACHE",
}

// Tracepoints we count for the submit-rate panel: path -> display name.
var tracepoints = map[string]string{
	"v3d/v3d_submit_cl":              "CL",
	"v3d/v3d_submit_csd":             "CSD",
	"v3d/v3d_submit_tfu":             "TFU",
	"v3d/v3d_cache_clean_begin":      "CacheClean",
	"gpu_scheduler/drm_sched_job_run": "SchedJobs",
}
var traceOrder = []string{"CL", "CSD", "TFU", "CacheClean", "SchedJobs"}

// ---------------- ANSI ----------------

const (
	rst  = "\x1b[0m"
	bold = "\x1b[1m"
	hide = "\x1b[?25l"
	show = "\x1b[?25h"
	clr  = "\x1b[2J\x1b[H"
	home = "\x1b[H"
	clrEnd = "\x1b[0J"
)

func fg(r, g, b int) string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b) }

func dur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// control holds the live, keyboard-mutable UI state, guarded by its mutex.
type control struct {
	mu        sync.Mutex
	interval  float64
	paused    bool
	showTrace bool
}

var (
	cFg    = fg(205, 214, 244)
	cMute  = fg(108, 112, 134)
	cAcc   = fg(137, 180, 250)
	cPanel = fg(69, 71, 90)
	cOK    = fg(166, 227, 161)
	cWarn  = fg(249, 226, 175)
	cHot   = fg(243, 139, 168)
)

// heat maps 0..100 onto a green->yellow->red ramp.
func heat(p float64) string {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	var r, g, b float64
	if p < 50 {
		t := p / 50
		r, g, b = 166+(249-166)*t, 227+(226-227)*t, 161+(175-161)*t
	} else {
		t := (p - 50) / 50
		r, g, b = 249+(243-249)*t, 226+(139-226)*t, 175+(168-175)*t
	}
	return fg(int(r), int(g), int(b))
}

var braille = []rune{' ', '⡀', '⡄', '⡆', '⡇', '⣇', '⣧', '⣷', '⣿'}

func spark(h []float64, w int) string {
	if len(h) == 0 {
		return strings.Repeat(" ", w)
	}
	if len(h) > w {
		h = h[len(h)-w:]
	}
	var b strings.Builder
	for _, v := range h {
		idx := int(v/100*8 + 0.5)
		if idx > 8 {
			idx = 8
		}
		b.WriteString(heat(v))
		b.WriteRune(braille[idx])
	}
	b.WriteString(rst)
	return b.String()
}

func bar(p float64, w int) string {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	fill := int(p/100*float64(w) + 0.5)
	var b strings.Builder
	for i := 0; i < w; i++ {
		if i < fill {
			b.WriteString(heat(float64(i) / float64(max(1, w-1)) * 100))
			b.WriteRune('█')
		} else {
			b.WriteString(cPanel)
			b.WriteRune('─')
		}
	}
	b.WriteString(rst)
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------- data sources ----------------

type client struct {
	comm string
	pid  string
	mem  int64 // KiB
	eng  map[string]int64
}

// readSample walks every process's DRM fdinfo and returns one entry per
// drm-client-id (de-duplicating the identical counters a process exposes
// across several fds).
func readSample() map[string]*client {
	out := map[string]*client{}
	fdis, _ := filepath.Glob("/proc/[0-9]*/fdinfo/*")
	for _, fdi := range fdis {
		data, err := os.ReadFile(fdi)
		if err != nil {
			continue
		}
		txt := string(data)
		if !strings.Contains(txt, "v3d") {
			continue
		}
		c := &client{eng: map[string]int64{}}
		var cid string
		driver := ""
		for _, ln := range strings.Split(txt, "\n") {
			k, v, ok := strings.Cut(ln, ":")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			switch {
			case k == "drm-driver":
				driver = v
			case k == "drm-client-id":
				cid = v
			case k == "drm-total-memory":
				c.mem = parseFirstInt(v)
			case strings.HasPrefix(k, "drm-engine-"):
				c.eng[strings.TrimPrefix(k, "drm-engine-")] = parseFirstInt(v)
			}
		}
		if driver != "v3d" || cid == "" {
			continue
		}
		parts := strings.Split(fdi, "/")
		c.pid = parts[2]
		if comm, err := os.ReadFile("/proc/" + c.pid + "/comm"); err == nil {
			c.comm = strings.TrimSpace(string(comm))
		} else {
			c.comm = "?"
		}
		out[cid] = c
	}
	return out
}

func parseFirstInt(s string) int64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(f[0], 10, 64)
	return n
}

// engineUtil returns per-engine utilisation % and a per-process total.
func engineUtil(prev, cur map[string]*client, dt float64) (map[string]float64, []procRow) {
	eng := map[string]float64{}
	var rows []procRow
	for cid, c := range cur {
		p := prev[cid]
		var ptot float64
		for _, e := range engines {
			if p == nil {
				continue
			}
			du := float64(c.eng[e]-p.eng[e]) / 1e9 / dt * 100
			if du > 0 {
				eng[e] += du
				ptot += du
			}
		}
		rows = append(rows, procRow{c.comm, c.pid, c.mem, ptot})
	}
	return eng, rows
}

type procRow struct {
	comm string
	pid  string
	mem  int64
	pct  float64
}

func vcgen(arg ...string) string {
	out, err := exec.Command("vcgencmd", arg...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type sysInfo struct {
	v3dMHz, coreMHz, temp, volt float64
	thr                         int64
}

func readSys() sysInfo {
	var s sysInfo
	if v := vcgen("measure_clock", "v3d"); strings.Contains(v, "=") {
		n, _ := strconv.ParseFloat(strings.SplitN(v, "=", 2)[1], 64)
		s.v3dMHz = n / 1e6
	}
	if v := vcgen("measure_clock", "core"); strings.Contains(v, "=") {
		n, _ := strconv.ParseFloat(strings.SplitN(v, "=", 2)[1], 64)
		s.coreMHz = n / 1e6
	}
	if v := vcgen("measure_temp"); strings.Contains(v, "=") {
		t := strings.TrimRight(strings.SplitN(v, "=", 2)[1], "'C")
		s.temp, _ = strconv.ParseFloat(strings.Split(t, "'")[0], 64)
	}
	if v := vcgen("measure_volts", "core"); strings.Contains(v, "=") {
		s.volt, _ = strconv.ParseFloat(strings.TrimRight(strings.SplitN(v, "=", 2)[1], "V"), 64)
	}
	if v := vcgen("get_throttled"); strings.Contains(v, "=") {
		s.thr, _ = strconv.ParseInt(strings.TrimPrefix(strings.SplitN(v, "=", 2)[1], "0x"), 16, 64)
	}
	return s
}

func throttleFlags(t int64) string {
	type f struct {
		name string
		col  string
	}
	var fs []f
	if t&0x1 != 0 {
		fs = append(fs, f{"UV-now", cHot})
	}
	if t&0x2 != 0 {
		fs = append(fs, f{"freq-cap", cWarn})
	}
	if t&0x4 != 0 {
		fs = append(fs, f{"THROTTLED", cHot})
	}
	if t&0x8 != 0 {
		fs = append(fs, f{"soft-temp", cWarn})
	}
	if t&0x10000 != 0 {
		fs = append(fs, f{"UV*", cMute})
	}
	if t&0x40000 != 0 {
		fs = append(fs, f{"thr*", cMute})
	}
	if len(fs) == 0 {
		return cOK + "clean" + rst
	}
	var b strings.Builder
	for i, x := range fs {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(x.col + x.name + rst)
	}
	return b.String()
}

type ident struct{ rev, qpu, slices string }

func readIdent() ident {
	var id ident
	data, err := os.ReadFile(driPath + "/v3d_ident")
	if err != nil {
		return id
	}
	for _, ln := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(ln, "Revision:"):
			id.rev = strings.TrimSpace(strings.SplitN(ln, ":", 2)[1])
		case strings.Contains(ln, "QPUs:") && id.qpu == "":
			id.qpu = strings.TrimSpace(strings.SplitN(ln, ":", 2)[1])
		case strings.Contains(ln, "Slices:") && id.slices == "":
			id.slices = strings.TrimSpace(strings.SplitN(ln, ":", 2)[1])
		}
	}
	return id
}

func measureClock() string {
	data, err := os.ReadFile(driPath + "/measure_clock")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ---------------- tracepoint counter (private ftrace instance) ----------------

type tracer struct {
	mu      sync.Mutex
	counts  map[string]int64
	dir     string
	pipe    *os.File
	enabled bool
}

func startTracer() *tracer {
	t := &tracer{counts: map[string]int64{}}
	dir := traceRoot + "/instances/" + instName
	if err := os.Mkdir(dir, 0755); err != nil && !os.IsExist(err) {
		return t // tracing unavailable or not root: panel stays empty
	}
	t.dir = dir
	for ev := range tracepoints {
		os.WriteFile(dir+"/events/"+ev+"/enable", []byte("1"), 0644)
	}
	os.WriteFile(dir+"/trace", []byte(""), 0644) // clear
	os.WriteFile(dir+"/tracing_on", []byte("1"), 0644)
	pipe, err := os.Open(dir + "/trace_pipe")
	if err != nil {
		t.cleanup()
		return t
	}
	t.pipe = pipe
	t.enabled = true
	go t.reader()
	return t
}

func (t *tracer) reader() {
	sc := bufio.NewScanner(t.pipe)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		ln := sc.Text()
		for ev, name := range tracepoints {
			// ftrace prints the bare event name after the colon, e.g. "v3d_submit_cl:"
			short := ev[strings.IndexByte(ev, '/')+1:]
			if strings.Contains(ln, short+":") {
				t.mu.Lock()
				t.counts[name]++
				t.mu.Unlock()
				break
			}
		}
	}
}

// snapshot returns counts since the last call and resets them.
func (t *tracer) snapshot() map[string]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]int64{}
	for k, v := range t.counts {
		out[k] = v
	}
	t.counts = map[string]int64{}
	return out
}

func (t *tracer) cleanup() {
	if t.pipe != nil {
		t.pipe.Close()
	}
	if t.dir != "" {
		for ev := range tracepoints {
			os.WriteFile(t.dir+"/events/"+ev+"/enable", []byte("0"), 0644)
		}
		os.Remove(t.dir) // rmdir the private instance
	}
}

// ---------------- terminal ----------------

type winsize struct{ Row, Col, X, Y uint16 }

func termWidth() int {
	ws := &winsize{}
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(syscall.Stdout),
		uintptr(0x5413), uintptr(unsafe.Pointer(ws))) // TIOCGWINSZ
	if err != 0 || ws.Col == 0 {
		return 100
	}
	if ws.Col > 110 {
		return 110
	}
	return int(ws.Col)
}

func sttyRaw()  { c := exec.Command("stty", "-echo", "-icanon", "min", "1", "time", "0"); c.Stdin = os.Stdin; c.Run() }
func sttySane() { c := exec.Command("stty", "sane"); c.Stdin = os.Stdin; c.Run() }

// ---------------- render ----------------

// vlen returns the visible width of s (runes), skipping ANSI CSI sequences.
// Every glyph v3dtop draws (box-drawing, block, braille, °, ·) is width 1.
func vlen(s string) int {
	r := []rune(s)
	n, i := 0, 0
	for i < len(r) {
		if r[i] == 0x1b {
			i++
			if i < len(r) && r[i] == '[' {
				i++
				for i < len(r) && !((r[i] >= 'A' && r[i] <= 'Z') || (r[i] >= 'a' && r[i] <= 'z')) {
					i++
				}
				if i < len(r) {
					i++
				}
			}
			continue
		}
		n++
		i++
	}
	return n
}

// panelRow wraps inner content (which begins with a leading space) in the
// left/right box borders, padding to the full panel width.
func panelRow(w int, inner string) string {
	padN := w - 2 - vlen(inner)
	if padN < 0 {
		padN = 0
	}
	return cAcc + "│" + rst + inner + strings.Repeat(" ", padN) + cAcc + "│" + rst
}

func boxTop(title string, w int) string {
	n := w - utf8.RuneCountInString(title) - 4
	if n < 0 {
		n = 0
	}
	return cAcc + "╭─" + rst + bold + cFg + " " + title + " " + rst + cAcc + strings.Repeat("─", n) + "╮" + rst
}
func boxBot(w int) string { return cAcc + "╰" + strings.Repeat("─", w-2) + "╯" + rst }

func frame(prev, cur map[string]*client, dt float64, hist *[]float64, id ident, tr *tracer, showTrace bool, iv float64) string {
	w := termWidth()
	sys := readSys()
	eng, rows := engineUtil(prev, cur, dt)

	var busy float64
	for _, v := range eng {
		if v > busy {
			busy = v
		}
	}
	*hist = append(*hist, busy)
	if len(*hist) > w-6 {
		*hist = (*hist)[len(*hist)-(w-6):]
	}

	var L []string
	L = append(L, fmt.Sprintf("%s%s V3DTOP %s%sRaspberry Pi VideoCore VI  ·  V3D %s  ·  %s QPUs / %s slices%s",
		bold, cAcc, rst, cMute, id.rev, id.qpu, id.slices, rst))
	L = append(L, "")

	// headline gauge
	L = append(L, boxTop("GPU busy", w))
	L = append(L, panelRow(w, fmt.Sprintf(" %s%s%5.1f%%%s %s", heat(busy), bold, busy, rst, bar(busy, w-14))))
	L = append(L, panelRow(w, fmt.Sprintf(" %shistory%s %s", cMute, rst, spark(*hist, w-12))))
	L = append(L, boxBot(w))

	// engines
	L = append(L, boxTop("Engines", w))
	for _, e := range engines {
		v := eng[e]
		L = append(L, panelRow(w, fmt.Sprintf(" %s%-13s%s %s%5.1f%%%s %s",
			cFg, engLabel[e], rst, heat(v), v, rst, bar(v, w-30))))
	}
	L = append(L, boxBot(w))

	// clocks / thermal / power
	L = append(L, boxTop("Clocks · Thermal · Power", w))
	L = append(L, panelRow(w, fmt.Sprintf(" %sV3D clk %s%s%.0fMHz%s   %score %s%s%.0fMHz%s   %stemp %s%s%.1f°C%s   %sVcore %s%s%.3fV%s   %sthrottle %s%s",
		cMute, heat(sys.v3dMHz/550*100), bold, sys.v3dMHz, rst,
		cMute, rst, cFg, sys.coreMHz, rst,
		cMute, heat(sys.temp/85*100), bold, sys.temp, rst,
		cMute, rst, cFg, sys.volt, rst,
		cMute, rst, throttleFlags(sys.thr))))
	if mc := measureClock(); mc != "" {
		L = append(L, panelRow(w, fmt.Sprintf(" %sdebugfs measure_clock: %s%s", cMute, mc, rst)))
	}
	L = append(L, boxBot(w))

	// submit-rate panel (tracepoints). Always snapshot (to keep rates sane
	// across a toggle); render only when the panel is enabled.
	if tr.enabled {
		snap := tr.snapshot()
		if showTrace {
			L = append(L, boxTop("Job submits  (v3d / gpu_scheduler tracepoints)", w))
			inner := " "
			for _, name := range traceOrder {
				rate := float64(snap[name]) / dt
				inner += fmt.Sprintf("%s%s %s%s%.0f/s%s    ", cMute, name, cFg, bold, rate, rst)
			}
			L = append(L, panelRow(w, inner))
			L = append(L, boxBot(w))
		}
	}

	// clients
	L = append(L, boxTop("GPU clients", w))
	L = append(L, panelRow(w, fmt.Sprintf(" %s%-18s%8s%12s%8s%s", cMute, "COMMAND", "PID", "GPU MEM", "GPU%", rst)))
	if len(rows) > 0 {
		sort.Slice(rows, func(i, j int) bool { return rows[i].pct > rows[j].pct })
		for i, r := range rows {
			if i >= 6 {
				break
			}
			comm := r.comm
			if len(comm) > 18 {
				comm = comm[:18]
			}
			L = append(L, panelRow(w, fmt.Sprintf(" %s%-18s%s%8s%s %s%8.1f MB %s%6.1f%%%s",
				cFg, comm, cMute, r.pid, rst, cFg, float64(r.mem)/1024, heat(r.pct), r.pct, rst)))
		}
	} else {
		note := "(no active V3D clients)"
		if os.Geteuid() != 0 {
			note = "(run with sudo to read per-process engine counters)"
		}
		L = append(L, panelRow(w, fmt.Sprintf(" %s%s%s", cMute, note, rst)))
	}
	L = append(L, boxBot(w))

	L = append(L, fmt.Sprintf("%s q quit · p pause · t trace · +/- speed · %.0fms · %s%s", cMute, iv*1000, time.Now().Format("15:04:05"), rst))
	return strings.Join(L, "\n")
}

// jsonOnce prints a single machine-readable sample (for logging / scripting).
func jsonOnce(iv float64, tr *tracer) {
	prev := readSample()
	t0 := time.Now()
	time.Sleep(dur(iv))
	cur := readSample()
	dt := time.Since(t0).Seconds()
	eng, rows := engineUtil(prev, cur, dt)
	sys := readSys()
	id := readIdent()
	out := map[string]any{
		"ts": time.Now().Format(time.RFC3339), "v3d_rev": id.rev,
		"qpus": id.qpu, "slices": id.slices, "engines_pct": eng,
		"v3d_mhz": sys.v3dMHz, "core_mhz": sys.coreMHz,
		"temp_c": sys.temp, "vcore_v": sys.volt, "throttled": sys.thr,
	}
	if tr.enabled {
		snap := tr.snapshot()
		rates := map[string]float64{}
		for k, v := range snap {
			rates[k] = float64(v) / dt
		}
		out["submit_rates"] = rates
	}
	var cl []map[string]any
	for _, r := range rows {
		cl = append(cl, map[string]any{"comm": r.comm, "pid": r.pid, "mem_kib": r.mem, "gpu_pct": r.pct})
	}
	out["clients"] = cl
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

// ---------------- main ----------------

func main() {
	iv := 0.5
	once := false
	jsonMode := false
	useTrace := true
	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--once":
			once = true
		case "--json":
			jsonMode = true
		case "--no-trace":
			useTrace = false
		case "-i":
			if i+1 < len(os.Args) {
				if n, err := strconv.ParseFloat(os.Args[i+1], 64); err == nil {
					iv = n
				}
				i++
			}
		case "-v", "--version":
			fmt.Println("v3dtop", version)
			return
		case "-h", "--help":
			fmt.Println("usage: sudo v3dtop [-i seconds] [--once] [--json] [--no-trace]")
			fmt.Println("keys:  q quit · p pause · t toggle trace panel · +/- faster/slower")
			return
		}
	}

	id := readIdent()
	tr := &tracer{counts: map[string]int64{}}
	if useTrace {
		tr = startTracer()
	}

	if jsonMode {
		jsonOnce(iv, tr)
		tr.cleanup()
		return
	}

	prev := readSample()
	t0 := time.Now()
	hist := []float64{}

	if once {
		time.Sleep(dur(iv))
		cur := readSample()
		dt := time.Since(t0).Seconds()
		fmt.Print(clr + frame(prev, cur, dt, &hist, id, tr, useTrace, iv) + "\n")
		tr.cleanup()
		return
	}

	ctl := &control{interval: iv, showTrace: useTrace}

	// graceful exit
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	quit := make(chan struct{})
	reconf := make(chan struct{}, 1)
	sttyRaw()
	go func() { // keyboard
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			if n == 0 {
				continue
			}
			ctl.mu.Lock()
			switch buf[0] {
			case 'q', 'Q':
				ctl.mu.Unlock()
				close(quit)
				return
			case 'p', 'P':
				ctl.paused = !ctl.paused
			case 't', 'T':
				ctl.showTrace = !ctl.showTrace
			case '+', '=':
				if ctl.interval > 0.15 {
					ctl.interval -= 0.1
				}
				select {
				case reconf <- struct{}{}:
				default:
				}
			case '-', '_':
				if ctl.interval < 5 {
					ctl.interval += 0.1
				}
				select {
				case reconf <- struct{}{}:
				default:
				}
			}
			ctl.mu.Unlock()
		}
	}()
	cleanup := func() {
		fmt.Print(show + rst + "\n")
		sttySane()
		tr.cleanup()
	}
	defer cleanup()

	fmt.Print(hide + clr)
	ticker := time.NewTicker(dur(iv))
	defer ticker.Stop()
	lastFrame := ""
	for {
		select {
		case <-sig:
			return
		case <-quit:
			return
		case <-reconf:
			ctl.mu.Lock()
			d := ctl.interval
			ctl.mu.Unlock()
			ticker.Reset(dur(d))
		case <-ticker.C:
			ctl.mu.Lock()
			paused, showTrace, curIv := ctl.paused, ctl.showTrace, ctl.interval
			ctl.mu.Unlock()
			if paused {
				fmt.Print(home + lastFrame + "\n" + cWarn + " ⏸ PAUSED — p to resume" + rst + clrEnd)
				continue
			}
			cur := readSample()
			dt := time.Since(t0).Seconds()
			t0 = time.Now()
			lastFrame = frame(prev, cur, dt, &hist, id, tr, showTrace, curIv)
			fmt.Print(home + lastFrame + clrEnd)
			prev = cur
		}
	}
}
