# SRT Adaptive Receiver Latency Design

## Summary

ppmmx previously applied a single, static `srtLatency` to every SRT
connection it accepted. The value was written into the listener config once
at startup (`internal/servers/srt/server.go`'s `Initialize()`) and could not
be changed without restarting the process.

This design adds a per-path, self-tuning receiver latency, ported from
philCDN/mmx's `docs/srt-adaptive-latency-design.md`. Every per-minute
unrecovered-drop-rate event adjusts that path's latency: an event above the
raise threshold adds one `srtLatencyRaiseStep`, one below the lower threshold
subtracts one `srtLatencyStep`, and anything in between leaves it unchanged.
The raise and lower steps are configured separately so latency can ramp up
against loss faster than it walks back down.

SRT fixes the receive/TSBPD delay at handshake time, so a retuned value only
reaches the wire on a fresh connection. The two directions are therefore
applied differently:

- A **raise** forces the current publisher to reconnect immediately: the
  server drops the connection and OBS reconnects on its own, renegotiating the
  larger window. A raise is fixing active, viewer-visible unrecovered loss, so
  the reconnect earns its cost.
- A **lower** is left pending and applied opportunistically at that path's
  next natural handshake. Lowering only trims a few hundred ms of delay off an
  already-healthy link, which does not justify interrupting a working stream.

This is a deliberate change from the original port, which never forced a
disconnect in either direction (see
[Why not the alternatives](#why-not-the-alternatives) and
[Latency of Effect](#latency-of-effect)).

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
- **Forced reconnect on *every* change.** The original port rejected forced
  reconnect outright, assuming an OBS reconnect costs tens of seconds of dead
  air. In this deployment an OBS SRT publisher reconnects in ~1s (measured in
  production, ppcenter's rc042 rollout notes), so a reconnect is affordable
  when it buys something. Reconnecting on a **lower** is still rejected -
  interrupting a healthy stream to shave a few hundred ms of delay is a poor
  trade. A **raise** is the opposite: it applies a larger window while the
  link is actively losing packets a viewer can see, so that one direction
  does force a reconnect. See [Applying the value](#5-applying-the-value).

## Non-Goals

- Changing the latency of an established connection *in place*. SRT
  negotiates TSBPD delay during the handshake and it is immutable for the
  life of the connection; `gosrt`'s `Conn` interface exposes no setter. A
  raise is applied by dropping the connection so the publisher reconnects and
  renegotiates, never by mutating the live socket.
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
}
```

Two entry points:

- `Record(path string, dropRatePct float64)` - applies one event
  immediately (see [Evaluation](#3-evaluation)).
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

Per event, i.e. on every 60s sample `Record` receives, for that path:

```
if   dropRatePct > srtLatencyRaisePct  -> current += srtLatencyRaiseStep
elif dropRatePct < srtLatencyLowerPct  -> current -= srtLatencyStep
else                                   -> unchanged

clamp(current, srtLatencyMin, srtLatencyMax)
```

The decision is applied to the stored value immediately. A raise then forces
the current publisher to reconnect so the new value reaches the wire at once;
a lower is left to reach the wire at that path's next natural handshake (see
[Applying the value](#5-applying-the-value)).

**Per event, not per window.** A windowed aggregate (an earlier revision
used the p95 over a multi-hour interval) delays the response by up to a
whole window and lets one bad minute hide inside a mostly-good one. Reacting
to each event makes the tuned value track the link directly: a run of bad
minutes raises latency by one step per minute until the drop disappears,
while a run of clean minutes walks it back down.

**Step and bounds.** Default `srtLatencyRaiseStep` is 200ms and default
`srtLatencyStep` (the lower/decrease step) is 100ms, per event, clamped to
`[srtLatencyMin, srtLatencyMax]` = `[300ms, 3000ms]` by default. The raise
step is larger so a link that has started dropping latency climbs out of the
loss faster than it falls once the link is clean. These
bounds are independent of `srtLatency`: a path is only *seeded* from
`srtLatency` (default 500ms) the first time it is seen, and can move below
that seed value down to `srtLatencyMin` if its drop rate is consistently
low. At most one step per event - a path that is already at a bound simply
stays there.

**Hysteresis.** The `srtLatencyLowerPct`-`srtLatencyRaisePct` dead band
(default 0.1%-1.0%) separates the raise and lower thresholds, so a path
hovering near one threshold does not oscillate. An event in the band leaves
the tuned value untouched.

**Zero traffic.** Zero-traffic intervals are never recorded at all - they
carry no information and would otherwise look like a low-drop event and
trigger spurious reductions. This mirrors the existing zero-traffic guard
already present for ppmmx's degrade FSM (`srtDecideDegrade` in `conn.go`).

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

**Applying a raise to the running connection.** `Record` returns whether the
event raised the tuned value. `runReceiveStatsSummary`
(`internal/servers/srt/conn.go`), which produces the per-minute sample and
already owns the publish connection, calls `c.Close()` when that return is
true - after emitting the interval's stats line and loss sample so the moment
is still fully logged. The publisher then reconnects on its own and
`chNewConnRequest` hands it the freshly raised value through the `SetLatency`
path above. This reuses the exact lever `srtLossDisconnect` already uses to
make a wedged publisher reconnect; if both fire on the same interval the loss
disconnect returns first and the raise reconnect is simply not reached. A
lower returns false and changes nothing about the running connection. A path
already pinned at `srtLatencyMax` never reports a raise (the clamped value
does not move), so a saturated link does not reconnect on a loop.

### Latency of Effect

A **raise** takes effect within one reconnect (~1s here) of the event that
triggered it: the running publisher is dropped and comes back negotiating the
higher value. A **lower** takes effect only at the path's next natural
handshake, so a long-lived, healthy publish connection may sit above its
tuned-down value indefinitely - which is harmless, since a pending lower means
the link is already clean at the current (higher) latency. Operators who want
a pending lower applied immediately can restart the publisher.

## Configuration

| Key | Default | Meaning |
| --- | --- | --- |
| `srtLatencyAutoTune` | `true` | Master switch. |
| `srtLatency` | `500ms` | Unchanged, existing key. Seeds a path's tuned value the first time it is seen. Never rewritten. |
| `srtLatencyMin` | `300ms` | Lower bound a path can be tuned down to. |
| `srtLatencyMax` | `3000ms` | Upper bound a path can be tuned up to. |
| `srtLatencyRaiseStep` | `200ms` | Amount one above-threshold event adds. |
| `srtLatencyStep` | `100ms` | Amount one below-threshold event subtracts (the decrease step). |
| `srtLatencyRaisePct` | `1.0` | An event above this raises latency. |
| `srtLatencyLowerPct` | `0.1` | An event below this lowers latency. |

Validation, alongside the existing rules in `internal/conf/conf.go`'s
`Validate()`:

- `srtLatencyMax >= srtLatencyMin`
- `srtLatencyMin <= srtLatency` (the seed value must fall inside the
  tunable range, otherwise the first event after startup immediately
  clamps it)
- `srtLatencyRaisePct > srtLatencyLowerPct` (non-negotiable; equal values
  remove the hysteresis and cause oscillation)
- `srtLatencyStep > 0`
- `srtLatencyRaiseStep > 0`

Since `srtLatencyAutoTune` defaults to `true`, an upgrade changes behavior
out of the box: any path whose measured drop rate is already above 1% will
start raising its latency on its next event, without any config change.
Deployments that must keep today's fixed-latency behavior need to set
`srtLatencyAutoTune: false` explicitly.

## Observability

One line, logged whenever an event moves the tuned value:

```
SRT latency tune - path: live/x, unrecovered drop: 1.04%, latency: 500ms -> 600ms
```

On a raise, a second line records the forced reconnect that applies it:

```
[SRT] [conn <addr>] SRT receive latency raised for path live/x; forcing publisher reconnect to apply it
```

Events inside the dead band leave the value unchanged and are not logged;
the per-minute `unrecoveredLoss=` field of the SRT stats line already
reports every sample, so a quiet path is still distinguishable from a broken
tuner.

`ReceiverBufferSize`/`FlowControlWindow` do not change per event (they are
fixed for `srtLatencyMax` at startup, see
[Buffer sizing](#4-buffer-sizing)), so they are not part of this line. The
listener startup log reports the buffer/FC values once.

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
  raise the tuned latency (one step per bad event) and degrade the
  simulcast ladder (immediately, on the next sample), which is
  intentional: latency tuning is a session-spanning fix, while degrade is a
  fast, single-connection mitigation.

## Test Plan

Unit tests (`internal/servers/srt/latency_test.go`), no live link required:

- An event above the raise threshold adds exactly one step; one below the
  lower threshold subtracts exactly one; a dead-band event changes nothing.
- Clamping at both bounds; repeated raises stop at `srtLatencyMax`,
  repeated lowers stop at `srtLatencyMin`, independent of the seed value
  `srtLatency`.
- `Record` reports `raised == true` only when the event moves the value up
  (loss above the raise threshold, not already at `srtLatencyMax`), and
  `false` for a lower, a dead-band hold, and a raise event that is already
  clamped at `srtLatencyMax`.
- Two paths tune independently and do not share state.
- An unknown path is seeded with `srtLatency`.
- `fc >= bufBytes / 1316` holds for the fixed, startup-computed buffer
  values.

Integration (manual/production):

- With `srtLatencyAutoTune: false`, the negotiated latency is identical to
  the pre-existing static behavior.
- A raise closes the running publish connection; the publisher reconnects and
  negotiates the higher value. A lower leaves the running connection in place,
  and the lower value is negotiated only by that path's next connection.

## Risks

| Risk | Mitigation |
| --- | --- |
| Forking `gosrt` adds maintenance cost | Change is ~8 lines, additive, on a stable interface; `replace` precedent already exists for `webtransport-go` |
| A raise reconnects the publisher; a lower may stay pending on a long-lived healthy connection | A pending lower is harmless (the link is clean at the higher latency); a raise, which fixes active loss, is applied within ~1s via the reconnect |
| Repeated raises churn the connection with reconnects | A raise happens at most once per 60s sample and only while loss stays above `srtLatencyRaisePct`; each raise lifts the window, so a link settles after a bounded number of steps and then stops. A path already at `srtLatencyMax` does not reconnect (the clamped value cannot rise) |
| A single very bad event moves the tuned value | `srtLatencyRaiseStep`/`srtLatencyStep` bound each event to one step; a raise reconnects to apply just that one step, then re-evaluates on the next 60s sample |
| Oscillation around a threshold | `srtLatencyLowerPct`-`srtLatencyRaisePct` dead band (default 0.1%-1.0%); only raises reconnect, so a path hovering in the band neither retunes nor reconnects |
| Larger buffers increase memory per connection | Fixed once for `srtLatencyMax`, not per-connection tuned value |
| `srtLatencyAutoTune` defaults on, changing behavior at upgrade time | Documented above; operators wanting the old fixed behavior must set it to `false` |
| Tuning masks a genuine network fault | Existing loss-alarm/degrade signals are unchanged and still fire off the same raw counters |
