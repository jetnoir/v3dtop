# v3dtop

![v3dtop — a btop-style monitor for the Raspberry Pi V3D GPU](art/banner.png)

A **btop-style live monitor for the Raspberry Pi V3D GPU** — zero dependencies, one static binary.

Most GPU monitors can't see the Pi's VideoCore. `v3dtop` reads the open-source
`v3d` driver directly and shows **real per-engine utilisation**, clock/thermal/throttle
state, per-process GPU usage, and live job-submit rates from kernel tracepoints.

```
 V3DTOP  Raspberry Pi VideoCore VI · V3D 4.2.14 · 8 QPUs / 2 slices
 ╭─ GPU busy ───────────────────────────────────────────────────────╮
 │ 100.0%  ████████████████████████████████████████████████████████ │
 │ history ⡀⡄⡆⡇⣇⣧⣷⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿ │
 ╰──────────────────────────────────────────────────────────────────╯
 ╭─ Engines ────────────────────────────────────────────────────────╮
 │ RENDER        100.0%  ███████████████████████████████████████████ │
 │ BIN             0.0%  ─────────────────────────────────────────── │
 │ CSD  compute    0.0%  ─────────────────────────────────────────── │
 │ TFU  texture    0.0%  ─────────────────────────────────────────── │
 │ CPU jobs        0.0%  ─────────────────────────────────────────── │
 │ CACHE           0.0%  ─────────────────────────────────────────── │
 ╰──────────────────────────────────────────────────────────────────╯
 ╭─ Clocks · Thermal · Power ───────────────────────────────────────╮
 │ V3D clk 500MHz  core 500MHz  temp 36.0°C  Vcore 0.936V  clean     │
 ╰──────────────────────────────────────────────────────────────────╯
 ╭─ Job submits  (v3d / gpu_scheduler tracepoints) ─────────────────╮
 │ CL 20/s   CSD 0/s   TFU 0/s   CacheClean 0/s   SchedJobs 20/s     │
 ╰──────────────────────────────────────────────────────────────────╯
```

## Install

Native on the Pi (needs Go ≥ 1.21):

```sh
git clone https://github.com/jetnoir/v3dtop && cd v3dtop
go build -o v3dtop .
sudo ./v3dtop
```

Or cross-compile from any machine (no cgo, so this just works):

```sh
GOOS=linux GOARCH=arm64 go build -o v3dtop .
```

Root is needed to read other processes' `fdinfo` engine counters and to use the
tracepoint panel. Run read-only without root and it still shows clock/thermal and
its own GPU clients, with a hint to re-run under `sudo`.

---

# Operations manual

## Command-line options

| Flag              | Meaning                                                      |
|-------------------|-------------------------------------------------------------|
| `-i <seconds>`    | Refresh interval (default `0.5`).                           |
| `--once`          | Render a single frame and exit (good for logs / cron).      |
| `--json`          | Emit one machine-readable JSON sample and exit.             |
| `--no-trace`      | Skip the tracepoint submit panel (no ftrace instance).      |
| `-v`, `--version` | Print version.                                              |
| `-h`, `--help`    | Print usage and keys.                                       |

## Keys (interactive)

| Key     | Action                                   |
|---------|------------------------------------------|
| `q`     | Quit.                                    |
| `p`     | Pause / resume the display.              |
| `t`     | Toggle the Job-submits (tracepoint) panel. |
| `+` `-` | Refresh faster / slower (0.1 s steps).   |

## Reading the panels

**GPU busy** — the single headline number is the *busiest engine* this interval,
with a braille history strip underneath. 100% means that engine was never idle.

**Engines** — the six V3D job queues, each as busy-time ÷ wall-time:

| Engine   | What runs on it                                                    |
|----------|-------------------------------------------------------------------|
| `RENDER` | Fragment/pixel work (the "RCL" render control list). Usually dominant for 3D/GL. |
| `BIN`    | Geometry binning (the "BCL") — vertices sorted into tiles before render. |
| `CSD`    | Compute shader dispatch (GLES 3.1 / compute workloads).           |
| `TFU`    | Texture Formatting Unit — texture uploads/format conversions.     |
| `CPU`    | CPU-side jobs the driver schedules through the GPU queue.          |
| `CACHE`  | Cache-clean operations.                                           |

A workload heavy in `BIN` but light in `RENDER` is geometry-bound; the reverse is
fill-/shader-bound. Pure compute shows up on `CSD`.

**Clocks · Thermal · Power** — V3D and core clock (`vcgencmd` + cycle-accurate
debugfs `measure_clock`), SoC temperature, core voltage, and the decoded
`get_throttled` flags:

| Flag        | Meaning                                              |
|-------------|------------------------------------------------------|
| `clean`     | No throttling, now or since boot.                    |
| `UV-now`    | Under-voltage **right now** (check the PSU/cable).   |
| `freq-cap`  | ARM frequency currently capped.                      |
| `THROTTLED` | Currently throttled.                                 |
| `soft-temp` | Soft temperature limit active (~80–85 °C).           |
| `UV*` `thr*`| The same conditions *occurred since boot* (sticky).  |

**Job submits** — live rates from the `v3d` and `gpu_scheduler` tracepoints: how
many command lists (`CL`), compute dispatches (`CSD`), texture jobs (`TFU`), and
cache-cleans are being submitted per second, plus total scheduler jobs run. This
is collected through a **private ftrace instance** (`/sys/kernel/tracing/instances/v3dtop`)
so the global trace buffer is never touched; it is removed on exit.

**GPU clients** — one row per `drm-client-id`: command, PID, GPU memory
(`drm-total-memory`), and that client's share of GPU time this interval.

## Scripting with `--json`

```sh
# one sample
sudo v3dtop --json

# log GPU state every 5 s to a file
while sleep 5; do sudo v3dtop --json --no-trace -i 0.3; done >> gpu.log
```

Fields: `engines_pct`, `v3d_mhz`, `core_mhz`, `temp_c`, `vcore_v`, `throttled`,
`submit_rates`, `clients[]`, plus `v3d_rev` / `qpus` / `slices`.

## Load generator

`tools/gpuload.c` is a headless GLES2 load generator — it renders a heavy fragment
shader to an off-screen FBO via the render node, so it needs **no display** and is
handy for exercising `v3dtop` on a headless Pi.

```sh
cc tools/gpuload.c -o tools/gpuload -lEGL -lGLESv2 -lgbm
./tools/gpuload 30 1024    # 30 s of load, 1024² target (heavier = lower fps)
```

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| Engines and clients always 0% | Not running as root — `fdinfo` engine counters need `sudo`. |
| Job-submits panel missing | `--no-trace` set, or `/sys/kernel/tracing` unavailable / not root. |
| `gpuload`: *Failed to find a suitable connector* | That's `glmark2`, not this tool — `gpuload` is surfaceless and needs no display. |
| `gpuload`: `GL_RENDERER: (null)` | EGL fell back to software; ensure it binds the render node via GBM (it does by default). |
| Colours look wrong | Needs a truecolor terminal; use `ssh -t` for live mode. |

## How it works

Pure Go standard library. Data sources:

- **`/proc/*/fdinfo/*`** — `drm-engine-*` nanosecond counters (de-duplicated by
  `drm-client-id`); utilisation is the per-interval delta over wall-time.
- **`vcgencmd`** — `measure_clock v3d|core`, `measure_temp`, `measure_volts core`,
  `get_throttled`.
- **`/sys/kernel/debug/dri/0`** — `v3d_ident` (capabilities) and `measure_clock`.
- **`/sys/kernel/tracing/instances/v3dtop`** — a private ftrace instance enabling
  `v3d/*` and `gpu_scheduler/drm_sched_job_run`, counted from its `trace_pipe`.

## Requirements

- Raspberry Pi 4 / 5 (or any board on the open `v3d` KMS driver)
- `vcgencmd` (ships with Raspberry Pi OS)
- A kernel with `ftrace`/`tracefs` — only for the submit panel

Tested on Pi 4, Debian 13 (trixie), kernel 6.18, Mesa 26.2, V3D 4.2.14.

## Licence

Dual-licensed: free for non-commercial use (MIT-style), commercial licence available.
See [LICENSE](LICENSE). © 2026 Stuart Thomas.
