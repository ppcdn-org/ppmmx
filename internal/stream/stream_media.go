package stream

import (
	"strconv"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediamtx/internal/errordumper"
	"github.com/bluenviron/mediamtx/internal/formatlabel"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/pion/rtp"
)

// mediaLabel renders a short stable identifier for one format inside one
// media, e.g. "video[1]/H264". On a Simulcast path every layer is a
// separate video media, so the index is what distinguishes them in the
// per-format diagnostics (see streamFormat.mediaLabel).
func mediaLabel(medi *description.Media, index int, forma format.Format) string {
	return string(medi.Type) + "[" + strconv.Itoa(index) + "]/" + string(formatlabel.FormatToLabel(forma))
}

type streamMedia struct {
	origMedia            *description.Media
	alwaysAvailable      bool
	rtpMaxPayloadSize    int
	replaceNTP           bool
	inboundBytes         *atomic.Uint64
	outboundBytes        *atomic.Uint64
	updateLastTime       func(time.Duration)
	writeRTSP            func(*description.Media, []*rtp.Packet, time.Time)
	updateOutDesc        func(func())
	inboundFramesInError *errordumper.Dumper
	// mediaIndex is this media's position in the stream's OrigDesc, used
	// to build each format's mediaLabel (see streamFormat.mediaLabel).
	mediaIndex int
	parent     logger.Writer

	outMedia *description.Media
	formats  map[format.Format]*streamFormat
}

func (sm *streamMedia) initialize() error {
	sm.formats = make(map[format.Format]*streamFormat)

	sm.outMedia = &description.Media{
		Type:    sm.origMedia.Type,
		ID:      sm.origMedia.ID,
		Formats: make([]format.Format, len(sm.origMedia.Formats)),
	}

	for i, origFormat := range sm.origMedia.Formats {
		sf := &streamFormat{
			origFormat:           origFormat,
			alwaysAvailable:      sm.alwaysAvailable,
			rtpMaxPayloadSize:    sm.rtpMaxPayloadSize,
			replaceNTP:           sm.replaceNTP,
			inboundFramesInError: sm.inboundFramesInError,
			inboundBytes:         sm.inboundBytes,
			outboundBytes:        sm.outboundBytes,
			updateLastTime:       sm.updateLastTime,
			writeRTSP:            sm.writeRTSPWrapper,
			updateOutDesc:        sm.updateOutDesc,
			mediaLabel:           mediaLabel(sm.origMedia, sm.mediaIndex, origFormat),
			parent:               sm.parent,
		}
		err := sf.initialize()
		if err != nil {
			return err
		}

		sm.formats[origFormat] = sf
		sm.outMedia.Formats[i] = sf.outFormat
	}

	return nil
}

func (sm *streamMedia) writeRTSPWrapper(pkts []*rtp.Packet, ntp time.Time) {
	sm.writeRTSP(sm.outMedia, pkts, ntp)
}
