package webrtc

import "github.com/bluenviron/mediamtx/internal/degrade"

// The OBS-degrade protocol's state machine and per-path registry live in
// internal/degrade (see that package's doc comment and
// docs/obs-mmx-degrade-protocol.md) - shared with internal/servers/srt so
// both ingest protocols feed the same per-path state and the same WS
// delivery channel. This file only adapts the sessionParent hooks
// (session.go's runDegradeSampling) onto Server.DegradeManager.

// recordDegradeSample implements the sessionParent hook used by
// session.runDegradeSampling.
func (s *Server) recordDegradeSample(pathName string, cumLost, cumReceived uint64) {
	s.DegradeManager.RecordSample(pathName, cumLost, cumReceived, degrade.Thresholds{
		DegradeInstantLossPct: s.DegradeInstantLossPct,
		DegradeAvgLossPct:     s.DegradeAvgLossPct,
		RecoverInstantLossPct: s.RecoverInstantLossPct,
		RecoverAvgLossPct:     s.RecoverAvgLossPct,
		ObservationSec:        s.DegradeObservationSec,
	})
}

// observeDegradeSessionLayers implements the sessionParent hook used by
// session.runDegradeSampling to report a fresh session's real negotiated
// simulcast layer count.
func (s *Server) observeDegradeSessionLayers(pathName string, realLayers int) {
	s.DegradeManager.ObserveSessionLayers(pathName, realLayers)
}

// degradeSampleEnabled implements the sessionParent hook.
func (s *Server) degradeSampleEnabled() bool {
	return s.DegradeEnable
}
