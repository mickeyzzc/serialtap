# Waveform Observation Guide

[English](waveform-guide.md) | [简体中文](../zh-CN/waveform-guide.md)

The panel's "波形" (waveform) tab — built into `serialtap run`, default
http://127.0.0.1:8801/ — is a **general-purpose oscilloscope**: it turns the
numbers inside serial telemetry lines into a rolling chart. This guide takes
you from zero to reading the chart; the **?** button in the panel header
carries a condensed version of the same content.

## Three steps to start

1. **Click a device card** on the left to select the board
2. Click the **波形** (waveform) tab
3. Watch — no configuration needed

The panel auto-detects telemetry lines and plots the numbers. "高级提取…"
(advanced extraction) is only for when the automatic channels aren't what you
want — and it has a live preview, so you see what you'll get before applying.

## How auto-detection works

Firmware typically emits telemetry in a fixed periodic line format, e.g.:

```
#S1 1117 27762 -49 09fb0e0212ff0a050e080803...
```

The panel groups lines by their **leading token** (the frame key, like `#S1`)
and plots the **most active** frame type within the last 5 seconds — occasional
heartbeat/handshake lines (`#S1-HELLO`, `I (1234)`) yield automatically.
Token classification within a line:

| Token shape | Channel | Example |
|---|---|---|
| `key=number` | **named channel** (named after the key) | `t=23.4` → channel `t` |
| pure number | **positional channel** (`v2`, `v3`… by token order) | `-49` → channel `v3` |
| hex blob / text | **excluded** (not a pure number) | `09fb0e02…` |

> Why hex blobs are never plotted: they contain `a-f` letters, failing the
> pure-number test — no user configuration needed. Binary payloads like CSI
> amplitude arrays aren't meant to be read as waveforms anyway; interpreting
> them is upper-layer business (e.g. the homepulse sense engine).

Channels cap at 6; if the number of numeric tokens drifts, labels follow.

## Custom extraction (advanced)

Three typical reasons to define channels yourself:

1. **Only one lane matters**: auto plots 3 channels but you only care about RSSI
2. **Grouping**: you want your own semantics instead of positional grouping
3. **Unusual line format**: auto-detection doesn't match

### Workflow

Click **高级提取…** in the waveform toolbar:

1. **Pick a template** (click to fill, then edit): `#S1 → RSSI`, first number
   in line, all numbers in line, `t=/h=` dual channel
2. **Edit the regex** and watch the **live test**: the last 20 raw log lines
   of the current device, extracted value(s) on the left, original line on the
   right — you see exactly what each line yields before applying
3. **Apply** (or **恢复自动识别** — back to auto — at any time)

### Regex cheat sheet

Each **capture group** (a pair of parentheses) = one channel; a regex without
capture groups turns every in-line match into a channel (≤6). The `[timestamp]`
prefix the panel adds to lines is stripped automatically — write the regex
against the **board's raw line**.

| What you want | Regex | Notes |
|---|---|---|
| RSSI (3rd number) on #S1 lines | `#S1 \d+ \d+ (-?\d+)` | one capture group |
| Temperature + humidity | `t=([\d.]+)\s+h=([\d.]+)` | two groups = two channels |
| First number in line | `(-?\d+(?:\.\d+)?)` | wrap the value in a group |
| All numbers in line | `-?\d+(?:\.\d+)?` | no group → every match is a channel |
| Floats only (drop integer counters) | `(-?\d+\.\d+)` | counters are integers, filtered naturally |

Syntax essentials: `\d` digit, `[\d.]` digit or dot, `( )` capture group,
`-?` optional minus, `\s+` whitespace. An invalid regex is flagged in red
immediately under the input.

## How to read the chart

### Lanes

**One lane per channel**, never squeezed together: the lane's top-left corner
labels the **channel name, current value, and range** (min~max with 8%
headroom). A curve touching the lane's top/bottom means it hit the window
extreme for that channel.

### Envelope and center line

Each curve is a **per-pixel min/max envelope plus a center line**: for every
horizontal pixel, all samples in that pixel contribute their min and max to a
filled envelope, and their mean to the center line. At high rates (tens of Hz):

- the envelope's "fuzz" is **real variation amplitude**, not noise
- the center line is the **trend**: slow drifts, sudden drops, steps
- a thin line (narrow envelope) = the value barely changed in this window

### Rate and windows

**Hz** in the toolbar is the sample rate (matched lines per second). Time
windows: 15s / 60s / 5min / 30min — longer sees more but blurs detail.

### Hover, freeze, clear, export

- **Hover**: a crosshair spans all lanes, reading every channel's value at that
  moment, with the time offset at the bottom
- **Freeze**: stops sampling and pins the window at the freeze moment for close
  inspection or screenshots; click again to resume
- **Clear**: empties the sample buffer
- **Export CSV**: samples as `t_ms,ch1,ch2…` with a channel-name header —
  ready for Excel/Python

### Reading examples

- **RSSI lane**: slow drift = channel/environment change; sudden drops =
  interference or occlusion; periodic undulation = something moving
  periodically (the basic signal of CSI sensing)
- **Counter lanes (v1/v2)**: a monotonic ramp is **normal**. Constant slope =
  steady flow; slope zeroed = dropped lines or firmware stopped; slope break =
  a reboot (cross-check with reset ticks on the event timeline)
- **Temperature/humidity**: steps in the center line are events (AC kicked in,
  someone came close); envelope width is sensor jitter
- **All-flat lines + red silent badge**: the board stopped outputting — check
  the card's silent/reopen indicators, then try "软重连" (soft reconnect)

## Troubleshooting

| Symptom | Fix |
|---|---|
| Chart empty | The device emits no number-bearing lines. Wait a few seconds; confirm telemetry lines appear in the full-log tab |
| Only diagonal ramps | Normal for counters — use advanced extraction to pick the lane you care about |
| Fewer channels than expected | Bare numeric tokens cap at 6; hex blobs never count (by design) |
| Auto picked the wrong frame | Occasional heartbeat lines win briefly and self-correct as the 5s window slides; or lock it down with custom extraction |
| Custom mode plots nothing | Regex matches 0 lines — the live test shows "0/20"; adjust against the original text shown |
| Chart frozen | You clicked freeze — click it again |
