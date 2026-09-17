# SRT Adaptive Receiver Latency Design

## Summary

ppmmx previously applied a single, static `srtLatency` to every SRT
connection it accepted. The value was written into the listener config once
at startup (`internal/servers/srt/server.go`'s `Initialize()`) and could not
be changed without restarting the process.

This design adds a per-path, self-tuning receiver latency, ported from
philCDN/mmx's `docs/srt-adaptive-latency-design.md`. Every
`srtLatencyEvalInterval` (default 8 hours) ppmmx evaluates the unrecovered
drop rate observed on each path over the preceding interval and derives a
new latency for that path. The new value is stored but is **not** applied
to the connection currently running. It is applied when the next connection
for that path performs its handshake.

The scheme is deliberately opportunistic: it never forces a disconnect, so
its runtime cost to viewers is zero. The trade-off is that a new value may
remain pending for a long time (see [Latency of Effect](#latency-of-effect)).

`srtLatency` in the configuration file is never modified. It is only the
starting value for a path that has not been tuned yet. The floor and
ceiling of the tuned range are the independent `srtLatencyMin` /
`srtLatencyMax` settings (see [Configuration](#configuration)).

## Motivation

Production data on a similar OBS -> origin SRT ingest link (from philCDN/mmx's
reference dataset, 840 one-minute samples on a single connection) shows the
unrecovered drop rate is not stationary across a day:

| Window | Unrecovered drop rate |
| --- | --- |
| quiet window | 0.086% mean |
| busier window | ~0.58% mean |
| whole period | 0.419% mean, 1.04% p95, 1.73% max |

Bitrate was flat throughout, so the change is a property of the link, not
of the encoder. A latency chosen for the quiet window is too small at
night; a latency chosen for the busy window is needlessly large during the
day.

In the same data `drop` tracks `retrans` closely, meaning retransmissions
were sent but arrived after their play deadline. That is the signature of
a receive window that is too small relative to link RTT and jitter, which
is what this design targets - exactly the same failure mode ppmmx's own
`unrecoverableLossPct` in `internal/servers/srt/conn.go` already surfaces
per minute.

### Why not the alternatives

- **Static increase of `srtLatency`.** Works, costs nothing at runtime, but
  pays the worst-case latency all day, and directly increases glass-to-glass
  delay for every viewer.
- **Forced reconnect on detection.** Typical OBS reconnect gaps run to
  tens of seconds of dead air. Adjusting a 200ms window at that cost is a
  poor trade. Rejected.

## Non-Goals

- Changing the latency of an established connection. SRT negotiates TSBPD
  delay during the handshake and it is immutable for the life of the
  connection. `gosrt`'s `Conn` interface exposes no setter.
- Persisting tuned values across process restarts. State is in-memory; a
  restart reseeds every path from `srtLatency`.
- Tuning the sender side.

## Background: where latency is decided

`gosrt` resolves the TSBPD delay in `connRequest.Accept()`:

```go
// conn_request.go
recvTsbpdDelay := uint16(req.config.ReceiverLatency.Milliseconds())
sendTsbpdDelay := uint16(req.config.PeerLatency.Milliseconds())
```

Two properties make per-path tuning possible:

1. `connRequest.config` is a `Config` **value**, not a pointer. It is
   copied from the listener when a request comes in, so each request owns
   an independent copy and mutating it cannot affect other connections.
2. `ConnRequest.StreamId()` is available *before* `Accept()`, so the path
   can be resolved in time to choose a latency.

The blocker is visibility: `config` is unexported and the `ConnRequest`
interface exposes only `RemoteAddr`, `Version`, `StreamId`, `SocketId`,
`PeerSocketId`, `IsEncrypted`, `SetPassphrase`, `SetRejectionReason`,
`Accept` and `Reject`. `Accept()` takes no arguments.

Therefore a fork of `gosrt` is required.

## Design

### 1. gosrt fork

`third_party/gosrt` vendors `github.com/datarhei/gosrt v0.11.0` (same fork
philCDN/mmx maintains) with one additive method on the `ConnRequest`
interface:

```go
// SetLatency overrides the receiver and peer TSBPD delay for this request
// only. It must be called before Accept.
SetLatency(recv, peer time.Duration)
```

```go
func (req *connRequest) SetLatency(recv, peer time.Duration) {
	req.config.ReceiverLatency = recv
	req.config.PeerLatency = peer
}
```

Both values are set together. The connection negotiates
`max(local RCVLATENCY, peer's PEERLATENCY)`, so setting only one lets a
publisher that requests less silently win.

The fork is wired in through `go.mod` with a `replace` directive:

```
replace github.com/datarhei/gosrt => ./third_party/gosrt
```

See `third_party/gosrt/FORK.md` for upgrade instructions.

### 2. Per-path state and statistics

`internal/servers/srt/latency.go`:

```go
type latencyManager struct {
	cfg   srtLatencyConfig
	log   func(logger.Level, string, ...any)
	mu    sync.Mutex
	paths map[string]*srtLatencyPathState
}

type srtLatencyPathState struct {
	current time.Duration // value handed to the next connection
	samples []float64     // rolling window of per-minute drop rates
}
```

Two entry points:

- `Record(path string, dropRatePct float64)` - appends one sample.
- `LatencyFor(path string) time.Duration` - returns the value for a new
  connection. An unknown path is seeded with `conf.SRTLatency` on first
  sight.

Samples come from the unrecovered-drop-rate computation ppmmx already runs
once per `recvstats.Interval` (60s) in
`internal/servers/srt/conn.go`'s `runReceiveStatsSummary`
(`unrecoverableLossPct`). This is the correct signal here because, unlike
the raw loss rate, it excludes losses that retransmission repaired in time.

State is keyed by path, not by connection, so it survives reconnects.

### 3. Evaluation

Every `srtLatencyEvalInterval` (default 8h), for each path:

```
p95 = percentile(samples, 95)

if   p95 > srtLatencyRaisePct  -> current += srtLatencyStep
elif p95 < srtLatencyLowerPct  -> current -= srtLatencyStep
else                           -> unchanged

clamp(current, srtLatencyMin, srtLatencyMax)
reset window
```

**Why p95 and not the mean.** The mean over a busy/quiet mixed period
dilutes exactly the interval the mechanism exists to detect - a bad night
can sit inside the no-change dead band on the mean while its p95 sits well
above the raise threshold.

**Step and bounds.** Default `srtLatencyStep` is 200ms per evaluation,
clamped to `[srtLatencyMin, srtLatencyMax]` = `[300ms, 3000ms]` by default.
These bounds are independent of `srtLatency`: a path is only *seeded* from
`srtLatency` the first time it is seen, and can move below that seed value
down to `srtLatencyMin` if its drop rate is consistently low. At most one
step per evaluation - this is intentional damping; a single bad night
should not slam the window wide open.

**Hysteresis.** The `srtLatencyLowerPct`-`srtLatencyRaisePct` dead band
(default 0.4%-0.8%) separates the raise and lower thresholds, so a path
hovering near one threshold does not oscillate.

**Insufficient data.** If a path has fewer than `srtLatencyMinSamples`
(default 60, i.e. one hour of traffic) valid samples in the window, the
evaluation is skipped and the window is reset. Zero-traffic intervals are
never recorded at all - they carry no information and would otherwise pull
the p95 toward zero and trigger spurious reductions. This mirrors the
existing zero-traffic guard already present for ppmmx's degrade FSM
(`srtDecideDegrade` in `conn.go`).

### 4. Buffer sizing

`ReceiverBufferSize`/`FlowControlWindow` must scale with latency or the
larger window cannot be filled.

The buffer is sized for `srtLatencyMax`, not for the latency in effect on
a given connection. Sizing it per-connection to the current (possibly
lower) tuned value would mean a later raise could outgrow a buffer that
was already allocated, which is not possible to correct without a
reconnect - the same problem this design exists to avoid for latency
itself. `ReceiverBufferSize`/`FlowControlWindow` are cheap (memory only, no
wire cost), so they are simply sized once for the worst case:

```
bufBytes = max(conf.SRTReceiverBufferSize,
               srtLatencyMax.Seconds() * assumedBitrateBps / 8 * safetyFactor)
fc       = max(conf.SRTFlowControlWindow, ceil(bufBytes / 1316))
```

with `safetyFactor = 2` and `1316 = 7 * 188` bytes per SRT payload. The
configured values remain the floor, so an operator can only ever raise
them. Because both are computed from the fixed `srtLatencyMax` rather than
the fluctuating per-path value, they are set once at listener
`Initialize()` time, not recomputed per connection - see
`srtWorstCaseBuffer` in `internal/servers/srt/server.go`.

### 5. Applying the value

In the `chNewConnRequest` branch of the server loop
(`internal/servers/srt/server.go`'s `run()`), before the request is
accepted:

```
req.StreamId()  ->  parse path  ->  LatencyFor(path)
                ->  req.SetLatency(v, v)
```

`ReceiverBufferSize` and `FlowControlWindow` are already fixed for the
listener as a whole (see [Buffer sizing](#4-buffer-sizing)) and need no
per-request call.

Stream ID parsing already exists at `internal/servers/srt/streamid.go`
(`streamID.unmarshal`) and is reused. A stream ID that fails to parse gets
`conf.SRTLatency` (the connection is rejected further along by the
existing code path regardless).

### Latency of Effect

A newly computed value takes effect only at the next handshake for that
path. A long-lived publish connection may run through several evaluations
without ever seeing the new value applied. This is the direct consequence
of refusing to force a reconnect, and it is the intended trade-off. The
mechanism converges over days, not minutes. Operators who need a value
applied immediately can restart the publisher.

## Configuration

| Key | Default | Meaning |
| --- | --- | --- |
| `srtLatencyAutoTune` | `true` | Master switch. |
| `srtLatency` | `300ms` | Unchanged, existing key. Seeds a path's tuned value the first time it is seen. Never rewritten. |
| `srtLatencyMin` | `300ms` | Lower bound a path can be tuned down to. |
| `srtLatencyMax` | `3000ms` | Upper bound a path can be tuned up to. |
| `srtLatencyEvalInterval` | `8h` | Evaluation period and sample window. |
| `srtLatencyStep` | `200ms` | Adjustment per evaluation. |
| `srtLatencyRaisePct` | `0.8` | p95 above this raises latency. |
| `srtLatencyLowerPct` | `0.4` | p95 below this lowers latency. |
| `srtLatencyMinSamples` | `60` | Minimum samples for a valid evaluation. |

Validation, alongside the existing rules in `internal/conf/conf.go`'s
`Validate()`:

- `srtLatencyMax >= srtLatencyMin`
- `srtLatencyMin <= srtLatency` (the seed value must fall inside the
  tunable range, otherwise the first evaluation after startup immediately
  clamps it)
- `srtLatencyRaisePct > srtLatencyLowerPct` (non-negotiable; equal values
  remove the hysteresis and cause oscillation)
- `srtLatencyStep > 0`, `srtLatencyEvalInterval > 0`,
  `srtLatencyMinSamples > 0`

Since `srtLatencyAutoTune` defaults to `true`, an upgrade changes behavior
out of the box: any path whose measured p95 drop rate is already above
0.8% will start raising its latency within the first 8-hour window,
without any config change. Deployments that must keep today's fixed-latency
behavior need to set `srtLatencyAutoTune: false` explicitly.

## Observability

Per evaluation, one line per path:

```
SRT latency tune - path: live/x, samples: 480, p95: 1.04%, latency: 300ms -> 500ms
```

`ReceiverBufferSize`/`FlowControlWindow` do not change per evaluation (they
are fixed for `srtLatencyMax` at startup, see
[Buffer sizing](#4-buffer-sizing)), so they are not part of this line. The
listener startup log reports the buffer/FC values once.

Skipped evaluations state the reason (`insufficient samples: 12 < 60`).
Unchanged evaluations are logged too, so a quiet path is distinguishable
from a broken evaluator.

## Interaction with existing ppmmx SRT features

ppmmx (unlike upstream philCDN/mmx) already ships:

- **Loss alarm/disconnect** (`srtLossAlarmEnable`, `srtLossDisconnectEnable`):
  based on the raw interval loss rate, unaffected by latency tuning -
  raising latency reduces unrecovered drop, which in turn reduces the
  chance of tripping the disconnect threshold, but the threshold itself is
  untouched by this design.
- **SRT-simulcast degrade** (`srtDegradeEnable`): also driven by the
  unrecovered drop rate (`srtDecideDegrade`), sharing the same
  60s-interval samples this design reads from. The two mechanisms consume
  the same signal independently - a persistently high drop rate will both
  raise the tuned latency (over the next 8h window) and degrade the
  simulcast ladder (immediately, on the next sample), which is
  intentional: latency tuning is a slow, session-spanning fix, while
  degrade is a fast, single-connection mitigation.

## Test Plan

Unit tests (`internal/servers/srt/latency_test.go`), no live link required:

- p95 on a known distribution, confirming a raise decision.
- Clamping at both bounds; repeated raises stop at `srtLatencyMax`,
  repeated lowers stop at `srtLatencyMin`, independent of the seed value
  `srtLatency`.
- Dead-band inputs produce no change.
- Fewer than `srtLatencyMinSamples` produces no change and resets the
  window.
- Two paths tune independently and do not share state.
- `fc >= bufBytes / 1316` holds for the fixed, startup-computed buffer
  values.
- `latencyManager.Run` exits promptly on context cancellation.

Integration (manual/production):

- With `srtLatencyAutoTune: false`, the negotiated latency is identical to
  the pre-existing static behavior.
- A connection established after a raise negotiates the new value; the
  connection that was already running is untouched.

## Risks

| Risk | Mitigation |
| --- | --- |
| Forking `gosrt` adds maintenance cost | Change is ~8 lines, additive, on a stable interface; `replace` precedent already exists for `webtransport-go` |
| Tuned value never applied on long-lived connections | Accepted and documented; zero-disruption was the requirement |
| p95 skewed by a short, very bad connection | `srtLatencyMinSamples` floor; `srtLatencyStep` cap per evaluation |
| Larger buffers increase memory per connection | Fixed once for `srtLatencyMax`, not per-connection tuned value |
| `srtLatencyAutoTune` defaults on, changing behavior at upgrade time | Documented above; operators wanting the old fixed behavior must set it to `false` |
| Tuning masks a genuine network fault | Existing loss-alarm/degrade signals are unchanged and still fire off the same raw counters |
