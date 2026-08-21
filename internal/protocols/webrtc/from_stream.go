package webrtc

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpav1"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtplpcm"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpvp8"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpvp9"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/g711"
	mch264 "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
	"github.com/bluenviron/mediamtx/internal/formatlabel"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	webrtcPayloadMaxSize = 1188 // 1200 - 12 (RTP header)

	// h264OutboundFmtpLine declares the H264 profile/level of outbound
	// (mmx-originated) tracks in the SDP offer, used both for WHEP playback
	// and for WHIP forwarding to Tencent Cloud (internal/forward). It is a
	// fixed value chosen ahead of time, not derived from the actual stream:
	// setupVideoTrack/SetupFromStreamABR/SetupFromStreamMultiH264 build and
	// send the SDP offer before Stream.AddReader() runs, i.e. before any
	// real access unit (and thus real SPS) has been seen on this output
	// path, so there is no live SPS byte data available yet to compute an
	// accurate value from.
	//
	// It must still not misdeclare the profile: encoders configured
	// through OBS/mmx (see conf.Path video encoder settings) always
	// produce High Profile (profile_idc 0x64), never Baseline - using
	// "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"
	// (Baseline, level 3.1) here declared the wrong *profile*, not just
	// the wrong level. level-asymmetry-allowed=1 (RFC 6184 §8.2.2)
	// explicitly permits the actual transmitted *level* to differ from
	// what's declared here, but never the *profile* - the decoder is not
	// required to support a profile it wasn't offered. A downstream
	// consumer that trusts the SDP's declared codec info (rather than
	// re-parsing the real in-band SPS on every access unit, as browsers
	// do) can fail to recognize the stream as H264 at all if the profile
	// is wrong. Root-caused Tencent Cloud's stats dashboard being unable
	// to identify the video codec of streams forwarded there, even though
	// playback itself worked fine (browsers ignore this and use the real
	// SPS).
	//
	// Level 5.1 (0x33) is a fixed upper bound that covers every layer mmx
	// currently produces (verified up to Level 4 for a 1080x1920 layer)
	// with headroom for higher resolutions.
	h264OutboundFmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640033"
)

var multichannelOpusSDP = map[int]string{
	3: "channel_mapping=0,2,1;num_streams=2;coupled_streams=1",
	4: "channel_mapping=0,1,2,3;num_streams=2;coupled_streams=2",
	5: "channel_mapping=0,4,1,2,3;num_streams=3;coupled_streams=2",
	6: "channel_mapping=0,4,1,2,3,5;num_streams=4;coupled_streams=2",
	7: "channel_mapping=0,4,1,2,3,5,6;num_streams=4;coupled_streams=4",
	8: "channel_mapping=0,6,1,4,5,2,3,7;num_streams=5;coupled_streams=4",
}

var errNoSupportedCodecsFrom = errors.New(
	"the stream doesn't contain any supported codec, which are currently " +
		"AV1, VP9, VP8, H265, H264, Opus, G722, G711, LPCM")

type abrOutputClock struct {
	baseTS        uint32
	initialized   bool
	lastTime      time.Time
	lastTS        uint32
	tickRemainder int64
}

func (c *abrOutputClock) next(now time.Time) uint32 {
	if !c.initialized {
		c.initialized = true
		c.lastTime = now
		c.lastTS = c.baseTS
		return c.lastTS
	}

	delta := now.Sub(c.lastTime)
	c.lastTime = now
	if delta <= 0 {
		c.lastTS++
		return c.lastTS
	}

	ticks := uint64(delta/time.Second) * 90000
	subsecond := int64(delta%time.Second)*90000 + c.tickRemainder
	ticks += uint64(subsecond / int64(time.Second))
	c.tickRemainder = subsecond % int64(time.Second)
	if ticks == 0 {
		ticks = 1
		c.tickRemainder = 0
	}
	c.lastTS += uint32(ticks)
	return c.lastTS
}

func randUint32() (uint32, error) {
	var b [4]byte
	_, err := rand.Read(b[:])
	if err != nil {
		return 0, err
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), nil
}

func multiplyAndDivide2(v, m, d time.Duration) time.Duration {
	secs := v / d
	dec := v % d
	return (secs*m + dec*m/d)
}

func timestampToDuration(t int64, clockRate int) time.Duration {
	return multiplyAndDivide2(time.Duration(t), time.Second, time.Duration(clockRate))
}

func audioUnitNTP(sourceNTP time.Time, now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return sourceNTP
}

func setupVideoTrack(
	desc *description.Session,
	r *stream.Reader,
) (*OutboundTrack, error) {
	var av1Format *format.AV1
	media := desc.FindFormat(&av1Format)

	if av1Format != nil { //nolint:dupl
		track := &OutboundTrack{
			Caps: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeAV1,
				ClockRate: 90000,
			},
		}

		encoder := &rtpav1.Encoder{
			PayloadType:    105,
			PayloadMaxSize: webrtcPayloadMaxSize,
		}
		err := encoder.Init()
		if err != nil {
			return nil, err
		}

		r.OnData(
			media,
			av1Format,
			func(u *unit.Unit) error {
				if u.NilPayload() {
					return nil
				}

				packets, err2 := encoder.Encode(u.Payload.(unit.PayloadAV1))
				if err2 != nil {
					return nil //nolint:nilerr
				}

				for _, pkt := range packets {
					ntp := u.NTP.Add(timestampToDuration(int64(pkt.Timestamp), 90000))
					pkt.Timestamp += u.RTPPackets[0].Timestamp
					track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
				}

				return nil
			})

		return track, nil
	}

	var vp9Format *format.VP9
	media = desc.FindFormat(&vp9Format)

	if vp9Format != nil {
		track := &OutboundTrack{
			Caps: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeVP9,
				ClockRate:   90000,
				SDPFmtpLine: "profile-id=0",
			},
		}

		encoder := &rtpvp9.Encoder{
			PayloadType:      96,
			PayloadMaxSize:   webrtcPayloadMaxSize,
			InitialPictureID: new(uint16(8445)),
		}
		err := encoder.Init()
		if err != nil {
			return nil, err
		}

		r.OnData(
			media,
			vp9Format,
			func(u *unit.Unit) error {
				if u.NilPayload() {
					return nil
				}

				packets, err2 := encoder.Encode(u.Payload.(unit.PayloadVP9))
				if err2 != nil {
					return nil //nolint:nilerr
				}

				for _, pkt := range packets {
					ntp := u.NTP.Add(timestampToDuration(int64(pkt.Timestamp), 90000))
					pkt.Timestamp += u.RTPPackets[0].Timestamp
					track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
				}

				return nil
			})

		return track, nil
	}

	var vp8Format *format.VP8
	media = desc.FindFormat(&vp8Format)

	if vp8Format != nil { //nolint:dupl
		track := &OutboundTrack{
			Caps: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeVP8,
				ClockRate: 90000,
			},
		}

		encoder := &rtpvp8.Encoder{
			PayloadType:    96,
			PayloadMaxSize: webrtcPayloadMaxSize,
		}
		err := encoder.Init()
		if err != nil {
			return nil, err
		}

		r.OnData(
			media,
			vp8Format,
			func(u *unit.Unit) error {
				if u.NilPayload() {
					return nil
				}

				packets, err2 := encoder.Encode(u.Payload.(unit.PayloadVP8))
				if err2 != nil {
					return nil //nolint:nilerr
				}

				for _, pkt := range packets {
					ntp := u.NTP.Add(timestampToDuration(int64(pkt.Timestamp), 90000))
					pkt.Timestamp += u.RTPPackets[0].Timestamp
					track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
				}

				return nil
			})

		return track, nil
	}

	var h265Format *format.H265
	media = desc.FindFormat(&h265Format)

	if h265Format != nil { //nolint:dupl
		track := &OutboundTrack{
			Caps: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH265,
				ClockRate:   90000,
				SDPFmtpLine: "level-id=93;profile-id=1;tier-flag=0;tx-mode=SRST",
			},
		}

		encoder := &rtph265.Encoder{
			PayloadType:    96,
			PayloadMaxSize: webrtcPayloadMaxSize,
		}
		err := encoder.Init()
		if err != nil {
			return nil, err
		}

		firstReceived := false
		var lastPTS int64

		r.OnData(
			media,
			h265Format,
			func(u *unit.Unit) error {
				if u.NilPayload() {
					return nil
				}

				if !firstReceived {
					firstReceived = true
				} else if u.PTS < lastPTS {
					return fmt.Errorf("WebRTC doesn't support H265 streams with B-frames")
				}
				lastPTS = u.PTS

				packets, err2 := encoder.Encode(u.Payload.(unit.PayloadH265))
				if err2 != nil {
					return nil //nolint:nilerr
				}

				for _, pkt := range packets {
					ntp := u.NTP.Add(timestampToDuration(int64(pkt.Timestamp), 90000))
					pkt.Timestamp += u.RTPPackets[0].Timestamp
					track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
				}

				return nil
			})

		return track, nil
	}

	var h264Format *format.H264
	media = desc.FindFormat(&h264Format)

	if h264Format != nil { //nolint:dupl
		track := &OutboundTrack{
			Caps: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: h264OutboundFmtpLine,
			},
		}

		encoder := &rtph264.Encoder{
			PayloadType:       96,
			PayloadMaxSize:    webrtcPayloadMaxSize,
			PacketizationMode: 1,
		}
		err := encoder.Init()
		if err != nil {
			return nil, err
		}

		firstReceived := false
		var lastPTS int64

		r.OnData(
			media,
			h264Format,
			func(u *unit.Unit) error {
				if u.NilPayload() {
					return nil
				}

				if !firstReceived {
					firstReceived = true
				} else if u.PTS < lastPTS {
					return fmt.Errorf("WebRTC doesn't support H264 streams with B-frames")
				}
				lastPTS = u.PTS

				packets, err2 := encoder.Encode(u.Payload.(unit.PayloadH264))
				if err2 != nil {
					return nil //nolint:nilerr
				}

				for _, pkt := range packets {
					ntp := u.NTP.Add(timestampToDuration(int64(pkt.Timestamp), 90000))
					pkt.Timestamp += u.RTPPackets[0].Timestamp
					track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
				}

				return nil
			})

		return track, nil
	}

	return nil, nil
}

func setupAudioTrack(
	desc *description.Session,
	r *stream.Reader,
	ntpNow func() time.Time,
) (*OutboundTrack, error) {
	var opusFormat *format.Opus
	media := desc.FindFormat(&opusFormat)

	if opusFormat != nil {
		var caps webrtc.RTPCodecCapability

		switch opusFormat.ChannelCount {
		case 1, 2:
			caps = webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeOpus,
				ClockRate: 48000,
				Channels:  2,
				SDPFmtpLine: func() string {
					s := "minptime=10;useinbandfec=1"
					if opusFormat.ChannelCount == 2 {
						s += ";stereo=1;sprop-stereo=1"
					}
					return s
				}(),
			}

		case 3, 4, 5, 6, 7, 8:
			caps = webrtc.RTPCodecCapability{
				MimeType:    mimeTypeMultiopus,
				ClockRate:   48000,
				Channels:    uint16(opusFormat.ChannelCount),
				SDPFmtpLine: multichannelOpusSDP[opusFormat.ChannelCount],
			}

		default:
			return nil, fmt.Errorf("unsupported channel count: %d", opusFormat.ChannelCount)
		}

		track := &OutboundTrack{
			Caps: caps,
		}

		curTimestamp, err := randUint32()
		if err != nil {
			return nil, err
		}

		r.OnData(
			media,
			opusFormat,
			func(u *unit.Unit) error {
				baseTimestamp := curTimestamp
				baseNTP := audioUnitNTP(u.NTP, ntpNow)

				for _, orig := range u.RTPPackets {
					// create a copy of the packet that we can edit freely
					pkt := &rtp.Packet{
						Header:  orig.Header,
						Payload: orig.Payload,
					}

					// recompute timestamp from scratch.
					// Chrome requires a precise timestamp that FFmpeg doesn't provide.
					pkt.Timestamp = curTimestamp
					curTimestamp += uint32(opus.PacketDuration2(pkt.Payload))

					ntp := baseNTP.Add(timestampToDuration(int64(pkt.Timestamp-baseTimestamp), 48000))
					track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
				}

				return nil
			})

		return track, nil
	}

	var g722Format *format.G722
	media = desc.FindFormat(&g722Format)

	if g722Format != nil {
		track := &OutboundTrack{
			Caps: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeG722,
				ClockRate: 8000,
			},
		}

		r.OnData(
			media,
			g722Format,
			func(u *unit.Unit) error {
				baseNTP := audioUnitNTP(u.NTP, ntpNow)
				for _, pkt := range u.RTPPackets {
					ntp := baseNTP.Add(timestampToDuration(
						int64(pkt.Timestamp-u.RTPPackets[0].Timestamp), 8000))
					track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
				}

				return nil
			})

		return track, nil
	}

	var g711Format *format.G711
	media = desc.FindFormat(&g711Format)

	if g711Format != nil {
		// These are the sample rates and channels supported by Chrome.
		// Different sample rates and channels can be streamed too but we don't want compatibility issues.
		// https://webrtc.googlesource.com/src/+/refs/heads/main/modules/audio_coding/codecs/pcm16b/audio_decoder_pcm16b.cc#23
		if g711Format.ClockRate() != 8000 && g711Format.ClockRate() != 16000 &&
			g711Format.ClockRate() != 32000 && g711Format.ClockRate() != 48000 {
			return nil, fmt.Errorf("unsupported clock rate: %d", g711Format.ClockRate())
		}
		if g711Format.ChannelCount != 1 && g711Format.ChannelCount != 2 {
			return nil, fmt.Errorf("unsupported channel count: %d", g711Format.ChannelCount)
		}

		var caps webrtc.RTPCodecCapability

		if g711Format.ClockRate() == 8000 {
			if g711Format.MULaw {
				if g711Format.ChannelCount != 1 {
					caps = webrtc.RTPCodecCapability{
						MimeType:  webrtc.MimeTypePCMU,
						ClockRate: uint32(g711Format.ClockRate()),
						Channels:  uint16(g711Format.ChannelCount),
					}
				} else {
					caps = webrtc.RTPCodecCapability{
						MimeType:  webrtc.MimeTypePCMU,
						ClockRate: 8000,
					}
				}
			} else {
				if g711Format.ChannelCount != 1 {
					caps = webrtc.RTPCodecCapability{
						MimeType:  webrtc.MimeTypePCMA,
						ClockRate: uint32(g711Format.ClockRate()),
						Channels:  uint16(g711Format.ChannelCount),
					}
				} else {
					caps = webrtc.RTPCodecCapability{
						MimeType:  webrtc.MimeTypePCMA,
						ClockRate: 8000,
					}
				}
			}
		} else {
			caps = webrtc.RTPCodecCapability{
				MimeType:  mimeTypeL16,
				ClockRate: uint32(g711Format.ClockRate()),
				Channels:  uint16(g711Format.ChannelCount),
			}
		}

		track := &OutboundTrack{
			Caps: caps,
		}

		if g711Format.ClockRate() == 8000 {
			curTimestamp, err := randUint32()
			if err != nil {
				return nil, err
			}

			r.OnData(
				media,
				g711Format,
				func(u *unit.Unit) error {
					baseTimestamp := curTimestamp
					baseNTP := audioUnitNTP(u.NTP, ntpNow)

					for _, orig := range u.RTPPackets {
						// create a copy of the packet that we can edit freely
						pkt := &rtp.Packet{
							Header:  orig.Header,
							Payload: orig.Payload,
						}

						// recompute timestamp from scratch.
						// Chrome requires a precise timestamp that FFmpeg doesn't provide.
						pkt.Timestamp = curTimestamp
						curTimestamp += uint32(len(pkt.Payload)) / uint32(g711Format.ChannelCount)

						ntp := baseNTP.Add(timestampToDuration(int64(pkt.Timestamp-baseTimestamp), 8000))
						track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
					}

					return nil
				})
		} else {
			encoder := &rtplpcm.Encoder{
				PayloadType:    96,
				PayloadMaxSize: webrtcPayloadMaxSize,
				BitDepth:       16,
				ChannelCount:   g711Format.ChannelCount,
			}
			err := encoder.Init()
			if err != nil {
				return nil, err
			}

			curTimestamp, err := randUint32()
			if err != nil {
				return nil, err
			}

			r.OnData(
				media,
				g711Format,
				func(u *unit.Unit) error {
					if u.NilPayload() {
						return nil
					}

					var lpcm []byte
					if g711Format.MULaw {
						var mu g711.Mulaw
						mu.Unmarshal(u.Payload.(unit.PayloadG711))
						lpcm = mu
					} else {
						var al g711.Alaw
						al.Unmarshal(u.Payload.(unit.PayloadG711))
						lpcm = al
					}

					packets, err2 := encoder.Encode(lpcm)
					if err2 != nil {
						return nil //nolint:nilerr
					}

					baseTimestamp := curTimestamp
					baseNTP := audioUnitNTP(u.NTP, ntpNow)

					for _, pkt := range packets {
						// recompute timestamp from scratch.
						// Chrome requires a precise timestamp that FFmpeg doesn't provide.
						pkt.Timestamp = curTimestamp
						curTimestamp += uint32(len(pkt.Payload)) / 2 / uint32(g711Format.ChannelCount)

						ntp := baseNTP.Add(timestampToDuration(
							int64(pkt.Timestamp-baseTimestamp), g711Format.ClockRate()))
						track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
					}

					return nil
				})
		}

		return track, nil
	}

	var lpcmFormat *format.LPCM
	media = desc.FindFormat(&lpcmFormat)

	if lpcmFormat != nil {
		if lpcmFormat.BitDepth != 16 {
			return nil, fmt.Errorf("unsupported LPCM bit depth: %d", lpcmFormat.BitDepth)
		}

		// These are the sample rates and channels supported by Chrome.
		// Different sample rates and channels can be streamed too but we don't want compatibility issues.
		// https://webrtc.googlesource.com/src/+/refs/heads/main/modules/audio_coding/codecs/pcm16b/audio_decoder_pcm16b.cc#23
		if lpcmFormat.ClockRate() != 8000 && lpcmFormat.ClockRate() != 16000 &&
			lpcmFormat.ClockRate() != 32000 && lpcmFormat.ClockRate() != 48000 {
			return nil, fmt.Errorf("unsupported clock rate: %d", lpcmFormat.ClockRate())
		}
		if lpcmFormat.ChannelCount != 1 && lpcmFormat.ChannelCount != 2 {
			return nil, fmt.Errorf("unsupported channel count: %d", lpcmFormat.ChannelCount)
		}

		track := &OutboundTrack{
			Caps: webrtc.RTPCodecCapability{
				MimeType:  mimeTypeL16,
				ClockRate: uint32(lpcmFormat.ClockRate()),
				Channels:  uint16(lpcmFormat.ChannelCount),
			},
		}

		encoder := &rtplpcm.Encoder{
			PayloadType:    96,
			BitDepth:       16,
			ChannelCount:   lpcmFormat.ChannelCount,
			PayloadMaxSize: webrtcPayloadMaxSize,
		}
		err := encoder.Init()
		if err != nil {
			return nil, err
		}

		curTimestamp, err := randUint32()
		if err != nil {
			return nil, err
		}

		r.OnData(
			media,
			lpcmFormat,
			func(u *unit.Unit) error {
				if u.NilPayload() {
					return nil
				}

				packets, err2 := encoder.Encode(u.Payload.(unit.PayloadLPCM))
				if err2 != nil {
					return nil //nolint:nilerr
				}

				baseTimestamp := curTimestamp
				baseNTP := audioUnitNTP(u.NTP, ntpNow)

				for _, pkt := range packets {
					// recompute timestamp from scratch.
					// Chrome requires a precise timestamp that FFmpeg doesn't provide.
					pkt.Timestamp = curTimestamp
					curTimestamp += uint32(len(pkt.Payload)) / 2 / uint32(lpcmFormat.ChannelCount)

					ntp := baseNTP.Add(timestampToDuration(
						int64(pkt.Timestamp-baseTimestamp), lpcmFormat.ClockRate()))
					track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
				}

				return nil
			})

		return track, nil
	}

	return nil, nil
}

func setupKLVDataChannel(
	desc *description.Session,
	r *stream.Reader,
) (*OutboundDataChannel, error) {
	var klvFormat *format.KLV
	media := desc.FindFormat(&klvFormat)

	if klvFormat != nil {
		dataChan := &OutboundDataChannel{
			Label: "KLV",
		}

		r.OnData(
			media,
			klvFormat,
			func(u *unit.Unit) error {
				if u.NilPayload() {
					return nil
				}

				dataChan.Write(u.Payload.(unit.PayloadKLV))
				return nil
			})

		return dataChan, nil
	}

	return nil, nil
}

// FromStream maps a MediaMTX stream to a WebRTC connection
func FromStream(
	desc *description.Session,
	r *stream.Reader,
	pc *PeerConnection,
) error {
	videoTrack, err := setupVideoTrack(desc, r)
	if err != nil {
		return err
	}

	if videoTrack != nil {
		pc.OutboundTracks = append(pc.OutboundTracks, videoTrack)
	}

	audioTrack, err := setupAudioTrack(desc, r, nil)
	if err != nil {
		return err
	}

	if audioTrack != nil {
		pc.OutboundTracks = append(pc.OutboundTracks, audioTrack)
	}

	klvDataChan, err := setupKLVDataChannel(desc, r)
	if err != nil {
		return err
	}

	if klvDataChan != nil {
		pc.OutboundDataChannels = append(pc.OutboundDataChannels, klvDataChan)
	}

	if len(pc.OutboundTracks) == 0 && len(pc.OutboundDataChannels) == 0 {
		return errNoSupportedCodecsFrom
	}

	setuppedFormats := r.Formats()

	n := 1
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if !slices.Contains(setuppedFormats, forma) {
				r.Parent.Log(logger.Warn, "skipping track %d (%s)", n, formatlabel.FormatToLabel(forma))
			}
			n++
		}
	}

	return nil
}

// SetupFromStreamABR is like FromStream but routes video through a TrackSelector.
// All H264 video medias of the description are subscribed, and the TrackSelector
// decides which track's access units are forwarded to the single output track.
// Audio and data channels are set up normally.
//
// Continuity across layer switches:
//   - a single shared RTP encoder keeps sequence numbers continuous;
//   - the outbound RTP timestamp is derived from the unit PTS, which lies on
//     a timeline shared by all medias (rtptime.GlobalDecoder on the publish
//     side), so timestamps stay continuous too;
//   - switching happens on IDR boundaries; the stream layer prepends each
//     layer's own SPS/PPS to IDR access units (see unitRemuxerH264), so the
//     decoder can reconfigure right after a switch.
func SetupFromStreamABR(
	desc *description.Session,
	r *stream.Reader,
	pc *PeerConnection,
	selector *TrackSelector,
) error {
	var videoMedias []*description.Media
	for _, m := range desc.Medias {
		if m.Type == description.MediaTypeVideo {
			videoMedias = append(videoMedias, m)
		}
	}

	hasH264 := false
	for _, m := range videoMedias {
		for _, forma := range m.Formats {
			if _, ok := forma.(*format.H264); ok {
				hasH264 = true
			}
		}
	}

	// ── Video: one outbound track, all H264 medias → TrackSelector ──
	if hasH264 {
		track := &OutboundTrack{
			Caps: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: h264OutboundFmtpLine,
			},
		}

		// single shared encoder → continuous sequence numbers across switches
		encoder := &rtph264.Encoder{
			PayloadType:       96,
			PayloadMaxSize:    webrtcPayloadMaxSize,
			PacketizationMode: 1,
		}
		err := encoder.Init()
		if err != nil {
			return err
		}

		tsBase, err := randUint32()
		if err != nil {
			return err
		}
		outputClock := abrOutputClock{baseTS: tsBase}

		for trackID, m := range videoMedias {
			// the media's own format instance is required: Reader.OnData
			// registrations are keyed by it, and a format belonging to a
			// different media is silently dropped by Stream.AddReader
			var h264Format *format.H264
			for _, forma := range m.Formats {
				if h, ok := forma.(*format.H264); ok {
					h264Format = h
					break
				}
			}
			if h264Format == nil {
				// ABR only supports a H264 ladder: a mixed-codec publisher
				// (e.g. one RTMP multitrack layer configured as HEVC/AV1)
				// would otherwise silently lose that layer with no track
				// info and no explanation.
				r.Parent.Log(logger.Warn,
					"ABR: video media %d is not H264, skipping (ABR requires all video layers to use the same H264 codec)",
					trackID)
				continue
			}

			firstReceived := false
			var lastPTS int64
			dimsResolved := false
			var ntpLogCount int

			// SPS parsing (see below) must run for every layer regardless of
			// whether it's currently selected, not just the active one -
			// ShouldQueue only lets the active/pending layer's units reach
			// the callback below, so an inactive layer's real
			// width/height/label would otherwise never replace its
			// layerDefaults guess (see TrackSelector.SetTrackDimensions)
			// until a viewer happens to switch to it. Doing it in the
			// filter itself, which mediamtx's stream package already runs
			// synchronously for every unit of every subscribed layer
			// before the ShouldQueue check gates the real reader queue,
			// gets this label fixed up promptly without consuming the
			// shared WHEP output queue for non-selected layers.
			r.OnDataFiltered(m, h264Format, func(u *unit.Unit) bool {
				if !u.NilPayload() && !dimsResolved {
					au, ok := u.Payload.(unit.PayloadH264)
					if ok {
						for _, nalu := range au {
							if len(nalu) != 0 && mch264.NALUType(nalu[0]&0x1F) == mch264.NALUTypeSPS {
								var sps mch264.SPS
								if err := sps.Unmarshal(nalu); err == nil {
									selector.SetTrackDimensions(trackID, sps.Width(), sps.Height())
									dimsResolved = true
								}
								break
							}
						}
					}
				}

				// RTP fragments that don't complete an access unit and inactive
				// Simulcast layers must not consume the shared WHEP reader queue.
				return !u.NilPayload() && selector.ShouldQueue(trackID)
			}, func(u *unit.Unit) error {
				if u.NilPayload() {
					return nil
				}

				if !firstReceived {
					firstReceived = true
				} else if u.PTS < lastPTS {
					return fmt.Errorf("WebRTC doesn't support H264 streams with B-frames")
				}
				lastPTS = u.PTS

				// This is the offset between the source PTS clock and wall time, not
				// playback latency. WHEP output uses the monotonic abrOutputClock below.
				ntpLogCount++
				if ntpLogCount%300 == 0 {
					r.Parent.Log(logger.Info, "ABR track %d: PTS=%d sourceNTP=%s (media PTS-wall offset=%s)",
						trackID, u.PTS,
						u.NTP.Format("15:04:05.000"),
						time.Since(u.NTP).Round(time.Millisecond))
				}

				au := u.Payload.(unit.PayloadH264)

				keyframe := false
				for _, nalu := range au {
					if len(nalu) != 0 && mch264.NALUType(nalu[0]&0x1F) == mch264.NALUTypeIDR {
						keyframe = true
						break
					}
				}

				shouldForward, _ := selector.OnAccessUnit(trackID, keyframe)
				if !shouldForward {
					return nil
				}

				packets, err2 := encoder.Encode(au)
				if err2 != nil {
					return nil //nolint:nilerr
				}

				// OBS Simulcast can emit frames slower than the cadence declared by
				// its source RTP timestamps. Derive the WHEP clock from real arrival
				// time so browsers play immediately instead of growing their jitter
				// buffer. This also keeps switches on one continuous timeline.
				now := time.Now()
				outTS := outputClock.next(now)

				for _, pkt := range packets {
					pkt.Timestamp = outTS
					track.WriteRTPWithNTP(pkt, now) //nolint:errcheck
				}

				return nil
			})
		}

		pc.OutboundTracks = append(pc.OutboundTracks, track)
	}

	// ── Audio: normal setup ─────────────────────────────────────
	audioTrack, err := setupAudioTrack(desc, r, time.Now)
	if err != nil {
		return err
	}

	if audioTrack != nil {
		pc.OutboundTracks = append(pc.OutboundTracks, audioTrack)
	}

	// ── Data channel ────────────────────────────────────────────
	klvDataChan, err := setupKLVDataChannel(desc, r)
	if err != nil {
		return err
	}

	if klvDataChan != nil {
		pc.OutboundDataChannels = append(pc.OutboundDataChannels, klvDataChan)
	}

	if len(pc.OutboundTracks) == 0 && len(pc.OutboundDataChannels) == 0 {
		return errNoSupportedCodecsFrom
	}

	setuppedFormats := r.Formats()

	n := 1
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if !slices.Contains(setuppedFormats, forma) {
				r.Parent.Log(logger.Warn, "skipping track %d (%s)", n, formatlabel.FormatToLabel(forma))
			}
			n++
		}
	}

	return nil
}

// SetupFromStreamMultiH264 maps H264 video medias to independent WebRTC tracks.
func SetupFromStreamMultiH264(
	desc *description.Session,
	r *stream.Reader,
	pc *PeerConnection,
	videoTrackCount int,
) error {
	type h264Media struct {
		media  *description.Media
		format *format.H264
	}

	videoMedias := make([]h264Media, 0, videoTrackCount)
	for _, media := range desc.Medias {
		if media.Type != description.MediaTypeVideo {
			continue
		}
		for _, forma := range media.Formats {
			if h264Format, ok := forma.(*format.H264); ok {
				videoMedias = append(videoMedias, h264Media{media, h264Format})
				break
			}
		}
		if len(videoMedias) == videoTrackCount {
			break
		}
	}

	// videoTrackCount is only an upper bound: a WHEP client building its
	// offer speculatively (e.g. AutoVideoTracks, which always offers
	// webrtc.MaxAutoVideoTracks recvonly video m-lines without knowing the
	// source's real layer count) may ask for more than the stream has.
	// Rather than failing the whole negotiation, serve however many H264
	// layers actually exist - the surplus offered m-lines are answered as
	// inactive by pion (standard JSEP), and the client already reads its
	// real track count back from the answer SDP instead of assuming it
	// got everything it asked for (see whip.Client.initializeInner).
	if len(videoMedias) == 0 {
		return fmt.Errorf("stream doesn't contain any H264 video layer")
	}

	for i, videoMedia := range videoMedias {
		track := &OutboundTrack{
			Caps: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: h264OutboundFmtpLine,
			},
			TrackID: fmt.Sprintf("video-%d", i),
		}

		encoder := &rtph264.Encoder{
			PayloadType:       96,
			PayloadMaxSize:    webrtcPayloadMaxSize,
			PacketizationMode: 1,
		}
		err := encoder.Init()
		if err != nil {
			return err
		}

		firstReceived := false
		var lastPTS int64
		r.OnData(videoMedia.media, videoMedia.format, func(u *unit.Unit) error {
			if u.NilPayload() {
				return nil
			}
			if !firstReceived {
				firstReceived = true
			} else if u.PTS < lastPTS {
				return fmt.Errorf("WebRTC doesn't support H264 streams with B-frames")
			}
			lastPTS = u.PTS

			packets, err2 := encoder.Encode(u.Payload.(unit.PayloadH264))
			if err2 != nil {
				return nil //nolint:nilerr
			}
			for _, pkt := range packets {
				ntp := u.NTP.Add(timestampToDuration(int64(pkt.Timestamp), 90000))
				pkt.Timestamp += u.RTPPackets[0].Timestamp
				track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
			}
			return nil
		})

		pc.OutboundTracks = append(pc.OutboundTracks, track)
	}

	audioTrack, err := setupAudioTrack(desc, r, nil)
	if err != nil {
		return err
	}
	if audioTrack != nil {
		audioTrack.TrackID = "audio"
		pc.OutboundTracks = append(pc.OutboundTracks, audioTrack)
	}

	klvDataChan, err := setupKLVDataChannel(desc, r)
	if err != nil {
		return err
	}
	if klvDataChan != nil {
		pc.OutboundDataChannels = append(pc.OutboundDataChannels, klvDataChan)
	}

	return nil
}

// mmxForwardSimulcastTrackID is the shared TrackID/m-line every video
// layer's OutboundTrack is grouped under by SetupFromStreamSimulcast - see
// OutboundTrack's type doc.
const mmxForwardSimulcastTrackID = "video"

// SetupFromStreamSimulcast maps every H264 video media of desc to one RID
// encoding ("0", "1", ... highest quality first, matching the RID order
// OBS's own WHIP Simulcast offer uses and to_stream.go's inbound sort) of a
// single shared video track/m-line, instead of FromStream's "first H264
// media only" or SetupFromStreamMultiH264's "one m-line per layer". This is
// what lets mmx forward its own multi-layer stream to another mmx node's
// WHIP publish endpoint, which - like mmx's own WHIP publish endpoint -
// only ever accepts a single video m-line (see TracksAreValid and
// offerVideoSimulcastLayerCount in internal/servers/webrtc/session.go).
// Audio and data channels are set up normally.
func SetupFromStreamSimulcast(
	desc *description.Session,
	r *stream.Reader,
	pc *PeerConnection,
) error {
	type h264Media struct {
		media  *description.Media
		format *format.H264
	}

	var videoMedias []h264Media
	for _, media := range desc.Medias {
		if media.Type != description.MediaTypeVideo {
			continue
		}
		for _, forma := range media.Formats {
			if h264Format, ok := forma.(*format.H264); ok {
				videoMedias = append(videoMedias, h264Media{media, h264Format})
				break
			}
		}
	}

	if len(videoMedias) == 0 {
		return errNoSupportedCodecsFrom
	}

	for i, videoMedia := range videoMedias {
		track := &OutboundTrack{
			Caps: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: h264OutboundFmtpLine,
			},
			TrackID: mmxForwardSimulcastTrackID,
			RID:     strconv.Itoa(i),
		}

		encoder := &rtph264.Encoder{
			PayloadType:       96,
			PayloadMaxSize:    webrtcPayloadMaxSize,
			PacketizationMode: 1,
		}
		err := encoder.Init()
		if err != nil {
			return err
		}

		firstReceived := false
		var lastPTS int64
		r.OnData(videoMedia.media, videoMedia.format, func(u *unit.Unit) error {
			if u.NilPayload() {
				return nil
			}
			if !firstReceived {
				firstReceived = true
			} else if u.PTS < lastPTS {
				return fmt.Errorf("WebRTC doesn't support H264 streams with B-frames")
			}
			lastPTS = u.PTS

			packets, err2 := encoder.Encode(u.Payload.(unit.PayloadH264))
			if err2 != nil {
				return nil //nolint:nilerr
			}
			for _, pkt := range packets {
				ntp := u.NTP.Add(timestampToDuration(int64(pkt.Timestamp), 90000))
				pkt.Timestamp += u.RTPPackets[0].Timestamp
				track.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
			}
			return nil
		})

		pc.OutboundTracks = append(pc.OutboundTracks, track)
	}

	audioTrack, err := setupAudioTrack(desc, r, nil)
	if err != nil {
		return err
	}
	if audioTrack != nil {
		audioTrack.TrackID = "audio"
		pc.OutboundTracks = append(pc.OutboundTracks, audioTrack)
	}

	klvDataChan, err := setupKLVDataChannel(desc, r)
	if err != nil {
		return err
	}
	if klvDataChan != nil {
		pc.OutboundDataChannels = append(pc.OutboundDataChannels, klvDataChan)
	}

	return nil
}
