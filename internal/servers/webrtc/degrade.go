package webrtc

import "github.com/bluenviron/mediamtx/internal/degrade"

func (s *Server) recordDegradeSample(pathName string, cumUnrecov, cumTotal uint64) degrade.Action {
	return s.DegradeManager.RecordSample(pathName, cumUnrecov, cumTotal, degrade.Thresholds{
		RaisePct:       s.DegradeRaisePct,
		LowerPct:       s.DegradeLowerPct,
		RestartPct:     s.DegradeRestartPct,
		ObservationSec: s.DegradeObservationSec,
	})
}

func (s *Server) observeDegradeSessionLayers(pathName string, realLayers int) {
	s.DegradeManager.ObserveSessionLayers(pathName, realLayers)
}

func (s *Server) degradeSampleEnabled() bool {
	return s.DegradeEnable
}

func (s *Server) degradeSampleSec() int {
	return s.DegradeSampleSec
}
