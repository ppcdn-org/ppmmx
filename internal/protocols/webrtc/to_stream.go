package webrtc

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/rtptime"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type ntpState int

const (
	ntpStateInitial ntpState = iota
	ntpStateReplace
	ntpStateAvailable
)

var errNoSupportedCodecsTo = errors.New(
	"the stream doesn't contain any supported codec, which are currently " +
		"AV1, VP9, VP8, H265, H264, Opus, G722, G711, LPCM")

type mediaEntry struct {
	medi  *description.Media
	video bool
	id    string
	rid   string
}

func sortMediaEntries(entries []mediaEntry) {
	videoCount := 0
	allLayerIDs := true
	allRID := true
	for _, entry := range entries {
		if entry.video {
			videoCount++
			if _, ok := videoTrackIndex(entry.id); !ok {
				allLayerIDs = false
			}
			if entry.rid == "" {
				allRID = false
			}
		}
	}

	if videoCount < 2 || (!allLayerIDs && !allRID) {
		return
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].video != entries[j].video {
			return entries[i].video
		}
		if !entries[i].video {
			return false
		}
		if allLayerIDs {
			ni, _ := videoTrackIndex(entries[i].id)
			nj, _ := videoTrackIndex(entries[j].id)
			return ni < nj
		}

		ni, erri := strconv.Atoi(entries[i].rid)
		nj, errj := strconv.Atoi(entries[j].rid)
		if erri == nil && errj == nil {
			return ni < nj
		}
		return entries[i].rid < entries[j].rid
	})
}

// ToStream maps a WebRTC connection to a MediaMTX stream.
func ToStream(
	pc *PeerConnection,
	pathConf *conf.Path,
	subStream **stream.SubStream,
	log logger.Writer,
) ([]*description.Media, error) {
	var medias []*description.Media //nolint:prealloc
	timeDecoder := &rtptime.GlobalDecoder{}
	timeDecoder.Initialize()

	for _, track := range pc.inboundTracks {
		var typ description.MediaType
		var forma format.Format

		switch strings.ToLower(track.track.Codec().MimeType) {
		case strings.ToLower(webrtc.MimeTypeAV1):
			typ = description.MediaTypeVideo
			forma = &format.AV1{
				PayloadTyp: 96,
			}

		case strings.ToLower(webrtc.MimeTypeVP9):
			typ = description.MediaTypeVideo
			forma = &format.VP9{
				PayloadTyp: 96,
			}

		case strings.ToLower(webrtc.MimeTypeVP8):
			typ = description.MediaTypeVideo
			forma = &format.VP8{
				PayloadTyp: 96,
			}

		case strings.ToLower(webrtc.MimeTypeH265):
			typ = description.MediaTypeVideo
			forma = &format.H265{
				PayloadTyp: 96,
			}

		case strings.ToLower(webrtc.MimeTypeH264):
			typ = description.MediaTypeVideo
			forma = &format.H264{
				PayloadTyp:        96,
				PacketizationMode: 1,
			}

		case strings.ToLower(mimeTypeMultiopus):
			typ = description.MediaTypeAudio
			forma = &format.Opus{
				PayloadTyp:   96,
				ChannelCount: int(track.track.Codec().Channels),
			}

		case strings.ToLower(webrtc.MimeTypeOpus):
			typ = description.MediaTypeAudio
			forma = &format.Opus{
				PayloadTyp: 96,
				ChannelCount: func() int {
					if strings.Contains(track.track.Codec().SDPFmtpLine, "stereo=1") {
						return 2
					}
					return 1
				}(),
			}

		case strings.ToLower(webrtc.MimeTypeG722):
			typ = description.MediaTypeAudio
			forma = &format.G722{}

		case strings.ToLower(webrtc.MimeTypePCMU):
			channels := int(track.track.Codec().Channels)
			if channels == 0 {
				channels = 1
			}

			typ = description.MediaTypeAudio
			forma = &format.G711{
				PayloadTyp: func() uint8 {
					if channels > 1 {
						return 96
					}
					return 0
				}(),
				MULaw:        true,
				SampleRate:   8000,
				ChannelCount: channels,
			}

		case strings.ToLower(webrtc.MimeTypePCMA):
			channels := int(track.track.Codec().Channels)
			if channels == 0 {
				channels = 1
			}

			typ = description.MediaTypeAudio
			forma = &format.G711{
				PayloadTyp: func() uint8 {
					if channels > 1 {
						return 96
					}
					return 8
				}(),
				MULaw:        false,
				SampleRate:   8000,
				ChannelCount: channels,
			}

		case strings.ToLower(mimeTypeL16):
			typ = description.MediaTypeAudio
			forma = &format.LPCM{
				PayloadTyp:   96,
				BitDepth:     16,
				SampleRate:   int(track.track.Codec().ClockRate),
				ChannelCount: int(track.track.Codec().Channels),
			}

		default:
			return nil, fmt.Errorf("unsupported codec: %+v", track.track.Codec().RTPCodecCapability)
		}

		medi := &description.Media{
			Type:    typ,
			Formats: []format.Format{forma},
		}

		var ntpStat ntpState

		if !pathConf.UseAbsoluteTimestamp {
			ntpStat = ntpStateReplace
		}

		handleNTP := func(pkt *rtp.Packet) (time.Time, bool) {
			switch ntpStat {
			case ntpStateReplace:
				return time.Time{}, true

			case ntpStateInitial:
				ntp, avail := track.PacketNTP(pkt)
				if !avail {
					log.Log(logger.Warn, "received RTP packet without absolute time, skipping it")
					return time.Time{}, false
				}

				ntpStat = ntpStateAvailable
				return ntp, true

			default: // ntpStateAvailable
				ntp, avail := track.PacketNTP(pkt)
				if !avail {
					panic("should not happen")
				}

				return ntp, true
			}
		}

		track.OnPacketRTP = func(pkt *rtp.Packet) {
			pkt.PayloadType = forma.PayloadType()

			pts, ok := timeDecoder.Decode(track, pkt)
			if !ok {
				return
			}

			ntp, ok := handleNTP(pkt)
			if !ok {
				return
			}

			(*subStream).WriteUnit(medi, forma, &unit.Unit{
				PTS:        pts,
				NTP:        ntp,
				RTPPackets: []*rtp.Packet{pkt},
			})
		}

		medias = append(medias, medi)
	}

	// Simulcast publishers (e.g. OBS WHIP with multiple Simulcast layers)
	// deliver tracks in packet-arrival order, which is random. When there
	// are multiple video tracks and every one carries a RID, order video
	// medias by RID so that the media order is deterministic and follows
	// the publisher's quality ladder; audio medias are placed after video
	// ones. OBS assigns RIDs "0".."N-1" (highest quality first), but RIDs
	// are compared numerically when possible so the order doesn't break if
	// a publisher ever uses two-digit RIDs (numeric "10" sorting before "2").
	entries := make([]mediaEntry, len(medias))

	for i, medi := range medias {
		video := medi.Type == description.MediaTypeVideo
		entries[i] = mediaEntry{medi, video, pc.inboundTracks[i].id, pc.inboundTracks[i].rid}
	}

	sortMediaEntries(entries)
	for i, entry := range entries {
		medias[i] = entry.medi
	}

	if len(medias) == 0 {
		return nil, errNoSupportedCodecsTo
	}

	return medias, nil
}

func videoTrackIndex(id string) (int, bool) {
	if !strings.HasPrefix(id, "video-") {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(id, "video-"))
	return index, err == nil && index >= 0
}
