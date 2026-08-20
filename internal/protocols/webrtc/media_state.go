package webrtc

import (
	"sync"
	"sync/atomic"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"

	"github.com/bluenviron/mediamtx/internal/unit"
)

// MediaState controls audio and video delivery for one WHEP session.
type MediaState struct {
	videoPaused          atomic.Bool
	audioPaused          atomic.Bool
	videoGeneration      atomic.Uint64
	videoMutex           sync.Mutex
	videoMediaGeneration map[*description.Media]uint64
}

func (s *MediaState) SetVideoPaused(paused bool) {
	wasPaused := s.videoPaused.Swap(paused)
	if wasPaused && !paused {
		s.videoGeneration.Add(1)
	}
}

func (s *MediaState) SetAudioPaused(paused bool) {
	s.audioPaused.Store(paused)
}

func (s *MediaState) VideoPaused() bool { return s.videoPaused.Load() }
func (s *MediaState) AudioPaused() bool { return s.audioPaused.Load() }
func (s *MediaState) VideoNeedsKeyframe() bool {
	s.videoMutex.Lock()
	defer s.videoMutex.Unlock()
	generation := s.videoGeneration.Load()
	if generation == 0 || len(s.videoMediaGeneration) == 0 {
		return generation != 0
	}
	for _, mediaGeneration := range s.videoMediaGeneration {
		if mediaGeneration < generation {
			return true
		}
	}
	return false
}

// Allow is a non-blocking stream.Reader media filter.
func (s *MediaState) Allow(media *description.Media, u *unit.Unit) bool {
	switch media.Type {
	case description.MediaTypeAudio:
		return !s.audioPaused.Load()

	case description.MediaTypeVideo:
		if s.videoPaused.Load() {
			s.videoMutex.Lock()
			if s.videoMediaGeneration == nil {
				s.videoMediaGeneration = make(map[*description.Media]uint64)
			}
			if _, ok := s.videoMediaGeneration[media]; !ok {
				s.videoMediaGeneration[media] = s.videoGeneration.Load()
			}
			s.videoMutex.Unlock()
			return false
		}
		s.videoMutex.Lock()
		defer s.videoMutex.Unlock()
		if s.videoMediaGeneration == nil {
			s.videoMediaGeneration = make(map[*description.Media]uint64)
		}
		generation := s.videoGeneration.Load()
		mediaGeneration, ok := s.videoMediaGeneration[media]
		if !ok {
			mediaGeneration = 0
			s.videoMediaGeneration[media] = mediaGeneration
		}
		if generation == 0 || mediaGeneration >= generation {
			return true
		}
		au, ok := u.Payload.(unit.PayloadH264)
		if !ok {
			s.videoMediaGeneration[media] = generation
			return true
		}
		for _, nalu := range au {
			if len(nalu) != 0 && h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeIDR {
				s.videoMediaGeneration[media] = generation
				return true
			}
		}
		return false
	}
	return true
}
