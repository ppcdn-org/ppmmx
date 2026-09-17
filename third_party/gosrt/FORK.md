# mmx fork of datarhei/gosrt

Vendored from `github.com/datarhei/gosrt v0.11.0`. See `docs/srt-adaptive-latency-design.md`.

Only change: `ConnRequest` gained `SetLatency(recv, peer time.Duration)`,
implemented in `conn_request.go`, so a per-connection TSBPD delay can be set
before `Accept()` without touching the listener's shared config. Everything
else is unmodified upstream source.

To pick up a newer upstream release, replace this directory with the new
version and reapply the `SetLatency` addition in `conn_request.go` (interface
method + `connRequest` implementation, both near `SetRejectionReason`).
