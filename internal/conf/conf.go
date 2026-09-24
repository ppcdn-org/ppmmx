// Package conf contains the struct that holds the configuration of the software.
package conf

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bluenviron/gohlslib/v2"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/auth"

	"github.com/bluenviron/mediamtx/internal/conf/decrypt"
	"github.com/bluenviron/mediamtx/internal/conf/env"
	"github.com/bluenviron/mediamtx/internal/conf/yamlwrapper"
	"github.com/bluenviron/mediamtx/internal/logger"
)

// ErrPathNotFound is returned when a path is not found.
var ErrPathNotFound = errors.New("path not found")

// DefaultWHIPWSSecret is a placeholder value used only by tests that need
// some fixed string and don't care about its content. There is no insecure
// built-in fallback for the real WHIP_WS_SECRET: Validate() requires it to
// be set explicitly whenever webrtcDegradeEnable is true (see
// docs/obs-mmx-degrade-protocol.md) - it must match whatever the OBS-side
// executor is configured with.
const DefaultWHIPWSSecret = "test-only-placeholder-not-a-real-secret"

// srtMinPayloadSize is the smallest SRT payload a sender is likely to use
// (7 MPEG-TS packets of 188 bytes). Only used to convert a receive buffer
// size in bytes into a worst-case packet count when validating
// SRTFlowControlWindow - the smallest payload gives the largest, i.e.
// safest, packet count.
const srtMinPayloadSize = 7 * 188

func sortedKeys(paths map[string]*OptionalPath) []string {
	ret := make([]string, len(paths))
	i := 0
	for name := range paths {
		ret[i] = name
		i++
	}
	sort.Strings(ret)
	return ret
}

func firstThatExists(paths []string) string {
	for _, pa := range paths {
		_, err := os.Stat(pa)
		if err == nil {
			return pa
		}
	}
	return ""
}

func setAllNilSlicesToEmptyRecursive(rv reflect.Value) {
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}

	if rv.Kind() == reflect.Struct {
		for _, field := range rv.Fields() {
			switch field.Kind() {
			case reflect.Slice:
				if field.IsNil() {
					field.Set(reflect.MakeSlice(field.Type(), 0, 0))
				} else {
					for j := range field.Len() {
						elem := field.Index(j)
						if elem.Kind() == reflect.Pointer || elem.Kind() == reflect.Struct {
							setAllNilSlicesToEmptyRecursive(elem)
						}
					}
				}

			case reflect.Pointer:
				if !field.IsNil() {
					setAllNilSlicesToEmptyRecursive(field)
				}

			case reflect.Struct:
				setAllNilSlicesToEmptyRecursive(field.Addr())

			case reflect.Map:
				if !field.IsNil() {
					for _, key := range field.MapKeys() {
						mapValue := field.MapIndex(key)
						if mapValue.Kind() == reflect.Pointer {
							setAllNilSlicesToEmptyRecursive(mapValue)
						}
					}
				}
			}
		}
	}
}

func copyStructFields(dest any, source any) {
	rvsource := reflect.ValueOf(source).Elem()
	rvdest := reflect.ValueOf(dest)
	nf := rvsource.NumField()
	var zero reflect.Value

	for i := range nf {
		fnew := rvsource.Field(i)
		f := rvdest.Elem().FieldByName(rvsource.Type().Field(i).Name)
		if f == zero {
			continue
		}

		if fnew.Kind() == reflect.Pointer {
			if !fnew.IsNil() {
				if f.Kind() == reflect.Pointer {
					f.Set(fnew)
				} else {
					f.Set(fnew.Elem())
				}
			}
		} else {
			f.Set(fnew)
		}
	}
}

func mustParseCIDR(v string) IPNetwork {
	_, ne, err := net.ParseCIDR(v)
	if err != nil {
		panic(err)
	}
	if ipv4 := ne.IP.To4(); ipv4 != nil {
		return IPNetwork{IP: ipv4, Mask: ne.Mask[len(ne.Mask)-4 : len(ne.Mask)]}
	}
	return IPNetwork(*ne)
}

func anyPathHasDeprecatedCredentials(pathDefaults Path, paths map[string]*OptionalPath) bool {
	if pathDefaults.PublishUser != nil ||
		pathDefaults.PublishPass != nil ||
		pathDefaults.PublishIPs != nil ||
		pathDefaults.ReadUser != nil ||
		pathDefaults.ReadPass != nil ||
		pathDefaults.ReadIPs != nil {
		return true
	}

	for _, pa := range paths {
		if pa != nil {
			rva := reflect.ValueOf(pa.Values).Elem()
			if rva.FieldByName("PublishUser").Interface().(*Credential) != nil ||
				rva.FieldByName("PublishPass").Interface().(*Credential) != nil ||
				rva.FieldByName("PublishIPs").Interface().(*IPNetworks) != nil ||
				rva.FieldByName("ReadUser").Interface().(*Credential) != nil ||
				rva.FieldByName("ReadPass").Interface().(*Credential) != nil ||
				rva.FieldByName("ReadIPs").Interface().(*IPNetworks) != nil {
				return true
			}
		}
	}
	return false
}

func deepClone(rv reflect.Value) reflect.Value {
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return rv
		}
		newPtr := reflect.New(rv.Elem().Type())
		newPtr.Elem().Set(deepClone(rv.Elem()))
		return newPtr

	case reflect.Struct:
		newStruct := reflect.New(rv.Type()).Elem()
		for i := range rv.NumField() {
			field := rv.Field(i)
			newField := newStruct.Field(i)
			if newField.CanSet() {
				newField.Set(deepClone(field))
			}
		}
		return newStruct

	case reflect.Slice:
		if rv.IsNil() {
			return reflect.Zero(rv.Type())
		}
		newSlice := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Cap())
		for i := range rv.Len() {
			newSlice.Index(i).Set(deepClone(rv.Index(i)))
		}
		return newSlice

	case reflect.Map:
		if rv.IsNil() {
			return reflect.Zero(rv.Type())
		}
		newMap := reflect.MakeMap(rv.Type())
		for _, key := range rv.MapKeys() {
			newMap.SetMapIndex(key, deepClone(rv.MapIndex(key)))
		}
		return newMap

	default:
		return rv
	}
}

type nilLogger struct{}

func (nilLogger) Log(_ logger.Level, _ string, _ ...any) {
}

var defaultAuthInternalUsers = []AuthInternalUser{
	{
		User: "any",
		Pass: "",
		Permissions: []AuthInternalUserPermission{
			{
				Action: AuthActionPublish,
			},
			{
				Action: AuthActionRead,
			},
			{
				Action: AuthActionPlayback,
			},
		},
	},
	{
		User: "any",
		Pass: "",
		IPs:  IPNetworks{mustParseCIDR("127.0.0.1/32"), mustParseCIDR("::1/128")},
		Permissions: []AuthInternalUserPermission{
			{
				Action: AuthActionAPI,
			},
			{
				Action: AuthActionMetrics,
			},
			{
				Action: AuthActionPprof,
			},
		},
	},
}

// Conf is a configuration.
type Conf struct {
	// General
	LogLevel            LogLevel        `json:"logLevel"`
	LogDestinations     LogDestinations `json:"logDestinations"`
	LogStructured       bool            `json:"logStructured"`
	LogFile             string          `json:"logFile"`
	SysLogPrefix        string          `json:"sysLogPrefix"`
	DumpPackets         bool            `json:"dumpPackets"`
	ReadTimeout         Duration        `json:"readTimeout"`
	WriteTimeout        Duration        `json:"writeTimeout"`
	ReadBufferCount     *int            `json:"readBufferCount,omitempty" deprecated:"true"`
	WriteQueueSize      int             `json:"writeQueueSize"`
	UDPMaxPayloadSize   int             `json:"udpMaxPayloadSize"`
	UDPReadBufferSize   uint            `json:"udpReadBufferSize"`
	RunOnConnect        string          `json:"runOnConnect"`
	RunOnConnectRestart bool            `json:"runOnConnectRestart"`
	RunOnDisconnect     string          `json:"runOnDisconnect"`

	// Authentication
	AuthMethod                AuthMethod                   `json:"authMethod"`
	AuthInternalUsers         []AuthInternalUser           `json:"authInternalUsers"`
	AuthHTTPAddress           string                       `json:"authHTTPAddress"`
	ExternalAuthenticationURL *string                      `json:"externalAuthenticationURL,omitempty" deprecated:"true"`
	AuthHTTPFingerprint       string                       `json:"authHTTPFingerprint"`
	AuthHTTPExclude           []AuthInternalUserPermission `json:"authHTTPExclude"`
	AuthJWTJWKS               string                       `json:"authJWTJWKS"`
	AuthJWTJWKSFingerprint    string                       `json:"authJWTJWKSFingerprint"`
	AuthJWTClaimKey           string                       `json:"authJWTClaimKey"`
	AuthJWTExclude            []AuthInternalUserPermission `json:"authJWTExclude"`
	AuthJWTInHTTPQuery        *bool                        `json:"authJWTInHTTPQuery,omitempty" deprecated:"true"`
	AuthJWTIssuer             string                       `json:"authJWTIssuer"`
	AuthJWTAudience           string                       `json:"authJWTAudience"`

	// Control API
	API               bool       `json:"api"`
	APIAddress        string     `json:"apiAddress"`
	APIEncryption     bool       `json:"apiEncryption"`
	APIServerKey      string     `json:"apiServerKey"`
	APIServerCert     string     `json:"apiServerCert"`
	APIAllowOrigin    *string    `json:"apiAllowOrigin,omitempty" deprecated:"true"`
	APIAllowOrigins   []string   `json:"apiAllowOrigins"`
	APITrustedProxies IPNetworks `json:"apiTrustedProxies"`

	// Metrics
	Metrics               bool       `json:"metrics"`
	MetricsAddress        string     `json:"metricsAddress"`
	MetricsEncryption     bool       `json:"metricsEncryption"`
	MetricsServerKey      string     `json:"metricsServerKey"`
	MetricsServerCert     string     `json:"metricsServerCert"`
	MetricsAllowOrigin    *string    `json:"metricsAllowOrigin,omitempty" deprecated:"true"`
	MetricsAllowOrigins   []string   `json:"metricsAllowOrigins"`
	MetricsTrustedProxies IPNetworks `json:"metricsTrustedProxies"`

	// PPROF
	PPROF               bool       `json:"pprof"`
	PPROFAddress        string     `json:"pprofAddress"`
	PPROFEncryption     bool       `json:"pprofEncryption"`
	PPROFServerKey      string     `json:"pprofServerKey"`
	PPROFServerCert     string     `json:"pprofServerCert"`
	PPROFAllowOrigin    *string    `json:"pprofAllowOrigin,omitempty" deprecated:"true"`
	PPROFAllowOrigins   []string   `json:"pprofAllowOrigins"`
	PPROFTrustedProxies IPNetworks `json:"pprofTrustedProxies"`

	// Playback
	Playback               bool       `json:"playback"`
	PlaybackAddress        string     `json:"playbackAddress"`
	PlaybackEncryption     bool       `json:"playbackEncryption"`
	PlaybackServerKey      string     `json:"playbackServerKey"`
	PlaybackServerCert     string     `json:"playbackServerCert"`
	PlaybackAllowOrigin    *string    `json:"playbackAllowOrigin,omitempty" deprecated:"true"`
	PlaybackAllowOrigins   []string   `json:"playbackAllowOrigins"`
	PlaybackTrustedProxies IPNetworks `json:"playbackTrustedProxies"`

	// RTSP server
	RTSP                  bool             `json:"rtsp"`
	RTSPDisable           *bool            `json:"rtspDisable,omitempty" deprecated:"true"`
	Protocols             *RTSPTransports  `json:"protocols,omitempty" deprecated:"true"`
	RTSPTransports        RTSPTransports   `json:"rtspTransports"`
	Encryption            *Encryption      `json:"encryption,omitempty" deprecated:"true"`
	RTSPEncryption        Encryption       `json:"rtspEncryption"`
	RTSPAddress           string           `json:"rtspAddress"`
	RTSPSAddress          string           `json:"rtspsAddress"`
	RTPAddress            string           `json:"rtpAddress"`
	RTCPAddress           string           `json:"rtcpAddress"`
	MulticastIPRange      string           `json:"multicastIPRange"`
	MulticastRTPPort      int              `json:"multicastRTPPort"`
	MulticastRTCPPort     int              `json:"multicastRTCPPort"`
	SRTPAddress           string           `json:"srtpAddress"`
	SRTCPAddress          string           `json:"srtcpAddress"`
	MulticastSRTPPort     int              `json:"multicastSRTPPort"`
	MulticastSRTCPPort    int              `json:"multicastSRTCPPort"`
	ServerKey             *string          `json:"serverKey,omitempty"`
	ServerCert            *string          `json:"serverCert,omitempty"`
	RTSPServerKey         string           `json:"rtspServerKey"`
	RTSPServerCert        string           `json:"rtspServerCert"`
	AuthMethods           *RTSPAuthMethods `json:"authMethods,omitempty" deprecated:"true"`
	RTSPAuthMethods       RTSPAuthMethods  `json:"rtspAuthMethods"`
	RTSPTrustedProxies    IPNetworks       `json:"rtspTrustedProxies"`
	RTSPUDPReadBufferSize *uint            `json:"rtspUDPReadBufferSize,omitempty" deprecated:"true"`

	// RTMP server
	RTMP               bool       `json:"rtmp"`
	RTMPDisable        *bool      `json:"rtmpDisable,omitempty" deprecated:"true"`
	RTMPEncryption     Encryption `json:"rtmpEncryption"`
	RTMPAddress        string     `json:"rtmpAddress"`
	RTMPSAddress       string     `json:"rtmpsAddress"`
	RTMPServerKey      string     `json:"rtmpServerKey"`
	RTMPServerCert     string     `json:"rtmpServerCert"`
	RTMPTrustedProxies IPNetworks `json:"rtmpTrustedProxies"`

	// HLS server
	HLS                bool       `json:"hls"`
	HLSDisable         *bool      `json:"hlsDisable,omitempty" deprecated:"true"`
	HLSAddress         string     `json:"hlsAddress"`
	HLSEncryption      bool       `json:"hlsEncryption"`
	HLSServerKey       string     `json:"hlsServerKey"`
	HLSServerCert      string     `json:"hlsServerCert"`
	HLSAllowOrigin     *string    `json:"hlsAllowOrigin,omitempty" deprecated:"true"`
	HLSAllowOrigins    []string   `json:"hlsAllowOrigins"`
	HLSTrustedProxies  IPNetworks `json:"hlsTrustedProxies"`
	HLSAlwaysRemux     bool       `json:"hlsAlwaysRemux"`
	HLSVariant         HLSVariant `json:"hlsVariant"`
	HLSSegmentCount    int        `json:"hlsSegmentCount"`
	HLSSegmentDuration Duration   `json:"hlsSegmentDuration"`
	HLSPartDuration    Duration   `json:"hlsPartDuration"`
	HLSSegmentMaxSize  StringSize `json:"hlsSegmentMaxSize"`
	HLSDirectory       string     `json:"hlsDirectory"`
	HLSMuxerCloseAfter Duration   `json:"hlsMuxerCloseAfter"`
	HLSCDNSecret       string     `json:"hlsCDNSecret"`

	// WebRTC server
	WebRTC                      bool              `json:"webrtc"`
	WebRTCDisable               *bool             `json:"webrtcDisable,omitempty" deprecated:"true"`
	WebRTCAddress               string            `json:"webrtcAddress"`
	WebRTCEncryption            bool              `json:"webrtcEncryption"`
	WebRTCServerKey             string            `json:"webrtcServerKey"`
	WebRTCServerCert            string            `json:"webrtcServerCert"`
	WebRTCAllowOrigin           *string           `json:"webrtcAllowOrigin,omitempty" deprecated:"true"`
	WebRTCAllowOrigins          []string          `json:"webrtcAllowOrigins"`
	WebRTCTrustedProxies        IPNetworks        `json:"webrtcTrustedProxies"`
	WebRTCLocalUDPAddress       string            `json:"webrtcLocalUDPAddress"`
	WebRTCLocalTCPAddress       string            `json:"webrtcLocalTCPAddress"`
	WebRTCIPsFromInterfaces     bool              `json:"webrtcIPsFromInterfaces"`
	WebRTCIPsFromInterfacesList []string          `json:"webrtcIPsFromInterfacesList"`
	WebRTCAdditionalHosts       []string          `json:"webrtcAdditionalHosts"`
	WebRTCICEServers2           []WebRTCICEServer `json:"webrtcICEServers2"`
	WebRTCSTUNGatherTimeout     Duration          `json:"webrtcSTUNGatherTimeout"`
	WebRTCHandshakeTimeout      Duration          `json:"webrtcHandshakeTimeout"`
	WebRTCTrackGatherTimeout    Duration          `json:"webrtcTrackGatherTimeout"`
	// WebRTCInboundRTPBufferSize sets rtpreceiver.Receiver.BufferSize (the
	// packet-reordering buffer) for every WHIP-published inbound track -
	// see internal/protocols/webrtc/inbound_track.go.
	//
	// This is WHIP's structural equivalent of SRTLatency: it bounds how
	// far out of order (or how late, via NACK retransmit) a packet may
	// arrive and still be recovered. gortsplib sizes a flat
	// []*rtp.Packet of exactly this length and treats any sequence gap
	// wider than it as unrecoverable loss, so the same failure mode
	// applies - a retransmit that arrives after the window has moved on
	// is *counted as lost even though it arrived*, showing up as a loss
	// spike with flat RTT.
	//
	// Sizing: gortsplib's own default is 64 packets, which at ~5Mbps/30fps
	// is only ~200ms of recovery window - barely more than one NACK
	// detection interval (100ms) plus an RTT, and a single keyframe burst
	// can consume most of it on its own. Defaulted to 512 below for the
	// same reason SRTLatency is 2000ms: the cost is bounded reordering
	// delay on an already-buffered publish path, and the benefit is
	// retransmits actually landing inside the window.
	WebRTCInboundRTPBufferSize int `json:"webrtcInboundRTPBufferSize"`

	// ABR (Adaptive Bitrate)
	WebRTCABREnable         bool   `json:"webrtcABREnable"`
	WebRTCABRWSPath         string `json:"webrtcABRWSPath"`
	WebRTCABRSwitchCooldown int    `json:"webrtcABRSwitchCooldown"`
	SplitRecAuthMode        string `json:"splitRecAuthMode"`
	TXSecretKeyBack         string `json:"-"`

	// WHIP degrade (OBS <-> mmx RTP-loss negotiation, see
	// docs/obs-mmx-degrade-protocol.md): degrades simulcast layers then
	// bitrate on sustained RTP loss, pushed to a per-path WS the OBS-side
	// executor connects to at "/{path}" + WebRTCDegradeWSPathSuffix.
	//
	// Degrade* and Recover* are separate thresholds (hysteresis): loss must
	// rise above Degrade* to trigger degrading, and fall back to at/below
	// Recover* to trigger recovering. Recover* <= Degrade* is enforced by
	// Validate() below; the gap between them is a dead zone that does
	// neither, so a loss rate hovering near a single boundary can't flap
	// the ladder back and forth every ObservationSec. Setting Recover* ==
	// Degrade* (the default) collapses the dead zone to zero width.
	WebRTCDegradeEnable         bool    `json:"webrtcDegradeEnable"`
	WebRTCDegradeWSPathSuffix   string  `json:"webrtcDegradeWSPathSuffix"`
	WebRTCDegradeInstantLossPct float64 `json:"webrtcDegradeInstantLossPct"`
	WebRTCDegradeAvgLossPct     float64 `json:"webrtcDegradeAvgLossPct"`
	WebRTCRecoverInstantLossPct float64 `json:"webrtcRecoverInstantLossPct"`
	WebRTCRecoverAvgLossPct     float64 `json:"webrtcRecoverAvgLossPct"`
	WebRTCDegradeObservationSec int     `json:"webrtcDegradeObservationSec"`
	WebRTCDegradeWSSecret       string  `json:"-"`

	// WHIP publish auth (see docs/obs-whip-publish-auth-protocol.md): AES key
	// checkWHIPDeviceID uses to decrypt the ppcenter-issued bearer token
	// every WHIP publish request must carry. Independent of
	// WebRTCDegradeWSSecret above (that one authenticates the degrade
	// protocol's own WS channel, not WHIP publish).
	WebRTCWHIPAuthKey string `json:"-"`

	// WebRTCForwardSecret is a static pre-shared secret checkWHIPDeviceID
	// also accepts, for mmx-to-mmx forwardMmx pushes between your own
	// trusted nodes (see Path.ForwardMmx*). Independent of both
	// WebRTCWHIPAuthKey (external ppobs publishers) and
	// WebRTCDegradeWSSecret (degrade WS channel).
	WebRTCForwardSecret string `json:"-"`

	// RTPLossAlarmEnable reports a WHIP publish session's RTP packet-loss
	// rate (the same figure logged every recvstats.Interval as
	// "[recv-stats] proto=whip ... loss=") to ppcenter (POST
	// /internal/mmx/v1/alarms/rtp-loss) whenever it crosses
	// RTPLossAlarmThresholdPct, and once more when it drops back under -
	// ppcenter raises/auto-resolves a superadmin node alarm from that (see
	// AlarmManager.CheckRTPLoss). Requires MMXControl for the endpoint/
	// credential, same reuse as SRTLossAlarmEnable/trafficUsage. Named RTP
	// rather than WebRTC since it's specifically the RTP-layer loss figure,
	// not a broader WebRTC/ICE/DTLS health signal.
	RTPLossAlarmEnable bool `json:"rtpLossAlarmEnable"`
	// RTPLossAlarmThresholdPct is a 0-100 percentage, not a 0-1 ratio.
	RTPLossAlarmThresholdPct float64 `json:"rtpLossAlarmThresholdPct"`

	// RTPLossRecycleEnable forces a WHIP publish session closed once its RTP
	// loss rate has stayed above RTPLossRecycleThresholdPct continuously for
	// RTPLossRecycleSec, so the publisher (typically OBS) reconnects on a
	// fresh PeerConnection/ICE path. This is the WHIP counterpart to SRT's
	// LossDisconnect self-heal, but tuned as a distinct "mild but chronic"
	// tier: a low threshold sustained for a long time (default 1% for 1h),
	// aimed at a connection that has settled into steady light loss the
	// degrade FSM and the higher alarm threshold both tolerate, but that a
	// reconnect would likely clear. Independent of RTPLossAlarmEnable and
	// does not require MMXControl - a node self-heals with ppcenter
	// unreachable (only the RTP loss ALARM needs the control plane).
	RTPLossRecycleEnable bool `json:"rtpLossRecycleEnable"`
	// RTPLossRecycleThresholdPct is a 0-100 percentage, not a 0-1 ratio.
	// Deliberately lower than RTPLossAlarmThresholdPct: this tier is about
	// chronic mild loss, not the alarm-level loss operators are paged for.
	RTPLossRecycleThresholdPct float64 `json:"rtpLossRecycleThresholdPct"`
	// RTPLossRecycleSec is how long loss must stay continuously over the
	// threshold before the recycle fires. Long by design (default 3600) so a
	// working-but-lossy stream is only recycled after the loss has clearly
	// failed to clear on its own, keeping the ~1s reconnect blip rare.
	RTPLossRecycleSec int `json:"rtpLossRecycleSec"`

	// CDN ingest (pull external CDN sources, re-publish them locally over
	// RTMP loopback so they're recorded/distributed like any other
	// publisher). Defaults to false (see setDefaults) - opt in per
	// deployment. Mutually exclusive with a path's forwardTencent (see
	// Path.validate in path.go): enabling this forces forwardTencent off,
	// since ingest would otherwise loop a Tencent-sourced stream straight
	// back to Tencent.
	IngestThreadEnable bool     `json:"ingestThreadEnable"`
	IngestSources      []string `json:"ingestSources"`

	// Net storage (recording upload): NetStorageEnv "prod" selects S3,
	// anything else selects MinIO. Credentials come from bin/.env. There is
	// no separate MinIO bucket setting: it's always the lowercased env name.
	NetStorageEnv            string `json:"-"`
	NetStorageS3Bucket       string `json:"-"`
	NetStorageS3Region       string `json:"-"`
	NetStorageS3AccessKey    string `json:"-"`
	NetStorageS3SecretKey    string `json:"-"`
	NetStorageS3Domain       string `json:"-"`
	NetStorageS3Endpoint     string `json:"-"`
	NetStorageS3ACL          string `json:"-"`
	NetStorageMinioEndpoint  string `json:"-"`
	NetStorageMinioAccessKey string `json:"-"`
	NetStorageMinioSecretKey string `json:"-"`
	NetStorageMinioUseSSL    string `json:"-"`
	NetStorageMinioDomain    string `json:"-"`

	// PlayURIRateLimit overrides the default 30 req/min/IP cap on
	// /api/playUri and /api/play(Tx)?Url. From PLAY_URI_RATE_LIMIT in
	// bin/.env; zero/unset keeps the default.
	PlayURIRateLimit int `json:"-"`

	// Forward (Tencent Cloud WHIP relay)
	TencentWHIPEnable    bool   `json:"tencentWHIPEnable"`
	TencentWHIPEndpoint  string `json:"tencentWHIPEndpoint"`
	TencentWHIPDomain    string `json:"tencentWHIPDomain"`
	TencentWHIPApp       string `json:"tencentWHIPApp"`
	TencentWHIPSecretKey string `json:"tencentWHIPSecretKey"`
	TencentWHIPTokenDays int    `json:"tencentWHIPTokenDays"`

	// Forward (mmx-to-mmx WHIP relay): account-level total switch, mirroring
	// TencentWHIPEnable above. The target URL/auth token are per-path (see
	// Path.ForwardMmxURL/ForwardMmxToken) since each path can forward to a
	// different downstream mmx node.
	ForwardMmxEnable bool `json:"forwardMmxEnable"`

	// Admin web UI (integrated)
	AdminAddress string `json:"adminAddress"`
	AdminName    string `json:"adminName"`
	// PlaySignatureTTL controls how long a signed /api/playUri (and
	// /api/play(Tx)?Url) URL stays valid before its txTime/txSecret expire.
	// Zero/unset defaults to 1h (see setDefaults).
	PlaySignatureTTL Duration `json:"playSignatureTTL"`

	// PPCDN control plane
	MMXControl               bool     `json:"mmxControl"`
	MMXControlURL            string   `json:"mmxControlURL"`
	MMXNodeSecret            string   `json:"-"` // from .env MMX_NODE_SECRET; sent in registration and as the Authorization bearer
	MMXNodeRole              string   `json:"mmxNodeRole"`
	MMXNodeRegion            string   `json:"mmxNodeRegion"`
	MMXNodeCapacity          int      `json:"mmxNodeCapacity"`
	MMXHeartbeatInterval     Duration `json:"mmxHeartbeatInterval"`
	MMXWebRTCBaseURL         string   `json:"mmxWebRTCBaseURL"`
	MMXPublishURL            string   `json:"mmxPublishURL"`
	MMXABRNegotiationAddress string   `json:"mmxABRNegotiationAddress"`

	// PPCDN recording sync (optional, only for origin nodes)
	MMXRecordingSyncEnabled      bool     `json:"mmxRecordingSyncEnabled"`
	MMXRecordingSyncURL          string   `json:"mmxRecordingSyncURL"`
	MMXRecordingSyncBearerToken  string   `json:"mmxRecordingSyncBearerToken"`
	MMXRecordingSyncPollInterval Duration `json:"mmxRecordingSyncPollInterval"`

	WebRTCICEUDPMuxAddress  *string   `json:"webrtcICEUDPMuxAddress,omitempty" deprecated:"true"`
	WebRTCICETCPMuxAddress  *string   `json:"webrtcICETCPMuxAddress,omitempty" deprecated:"true"`
	WebRTCICEHostNAT1To1IPs *[]string `json:"webrtcICEHostNAT1To1IPs,omitempty" deprecated:"true"`
	WebRTCICEServers        *[]string `json:"webrtcICEServers,omitempty" deprecated:"true"`

	// SRT server
	SRT        bool   `json:"srt"`
	SRTAddress string `json:"srtAddress"`
	// SRTPublishTokenRequired rejects a SRT publish that carries no
	// ppcenter publish token in its streamID. The token itself is checked
	// whenever one is present (and WHIP_AUTH_KEY is set); this only
	// controls whether omitting it is allowed, so existing publishers and
	// third-party SRT tools keep working until it's turned on.
	SRTPublishTokenRequired bool `json:"srtPublishTokenRequired"`
	// SRT transport tuning. These map to the corresponding SRT socket
	// options (SRTO_RCVLATENCY / SRTO_PEERLATENCY / SRTO_RCVBUF /
	// SRTO_FC) and exist because gosrt's own defaults are far too tight
	// for a WAN ingest: 120ms of latency at a real-world ~30ms RTT is
	// only ~4x RTT, barely one retransmission round-trip. Any jitter
	// that pushes a retransmitted packet past that window makes SRT
	// declare it lost even though it did arrive - which shows up as a
	// sudden packet-loss spike while RTT itself stays flat (the
	// signature of a too-small receive window, not of real congestion).
	//
	// SRTLatency is the receiver-side buffering window; it is applied to
	// both RCVLATENCY and PEERLATENCY (the negotiated latency is the max
	// of the two ends' requests anyway). Rule of thumb: at least 3-4x
	// the worst-case RTT. It is also the price paid in end-to-end
	// glass-to-glass delay, since TSBPD holds every packet for the full
	// window before delivering it upstream - so it should be the
	// smallest value that still leaves room for a retransmission round
	// (which costs ~1 RTT), not the largest value that fits.
	// SRTReceiverBufferSize must be large enough to hold SRTLatency
	// worth of stream at the expected bitrate, otherwise the buffer,
	// not the latency, becomes the limit.
	SRTLatency            Duration   `json:"srtLatency"`
	SRTReceiverBufferSize StringSize `json:"srtReceiverBufferSize"`
	// SRTFlowControlWindow is SRTO_FC in packets. It must be able to
	// cover SRTLatency worth of in-flight packets; SRT itself requires
	// FC >= the receive buffer expressed in packets, and silently
	// clamps the usable buffer to FC otherwise.
	SRTFlowControlWindow int `json:"srtFlowControlWindow"`

	// SRT adaptive receive latency (see docs/srt-adaptive-latency-design.md):
	// per publish path, every per-minute unrecovered drop-rate event adjusts
	// that path's own tuned latency, clamped to
	// [SRTLatencyMin, SRTLatencyMax]. An event above SRTLatencyRaisePct
	// raises the latency by SRTLatencyRaiseStep, one below
	// SRTLatencyLowerPct lowers it by SRTLatencyStep, and a value inside
	// the dead band leaves it unchanged. A path seeds its tuned value from
	// SRTLatency the first time it is seen.
	//
	// SRT negotiates the TSBPD delay once at handshake time and it cannot
	// be changed for the life of a connection, so a retuned value only
	// reaches the wire on a fresh handshake. The two directions are handled
	// differently (see the design doc and conn.go's runReceiveStatsSummary):
	//   - a RAISE (loss above SRTLatencyRaisePct) forces the current
	//     publisher to reconnect immediately, so the larger window is
	//     actually applied - it is fixing active, viewer-visible unrecovered
	//     loss, and the ~1s OBS SRT reconnect pays for itself;
	//   - a LOWER only trims a few hundred ms of delay off an already-healthy
	//     link, so it is left to apply opportunistically at that path's next
	//     natural reconnect rather than interrupting a working stream.
	// SRTLatencyAutoTune gates the whole mechanism, the raise reconnect
	// included.
	//
	// SRTLatencyRaisePct/SRTLatencyLowerPct form a hysteresis band
	// (SRTLatencyLowerPct < SRTLatencyRaisePct, enforced by Validate) so a
	// path hovering near one threshold doesn't oscillate.
	//
	// SRTReceiverBufferSize/SRTFlowControlWindow are not tuned per path:
	// they are sized once at startup for SRTLatencyMax (the worst case a
	// path can ever be tuned to), since sizing them for a path's current
	// (possibly lower) tuned value would mean a later raise could outgrow
	// a buffer already allocated - the same problem this whole mechanism
	// exists to avoid for latency itself.
	SRTLatencyAutoTune bool     `json:"srtLatencyAutoTune"`
	SRTLatencyMin      Duration `json:"srtLatencyMin"`
	SRTLatencyMax      Duration `json:"srtLatencyMax"`
	// SRTLatencyRaiseStep is how much one above-threshold event adds;
	// SRTLatencyStep is how much one below-threshold event subtracts. They
	// are deliberately separate so ramping up against loss can be faster
	// than walking back down once the link is clean.
	SRTLatencyRaiseStep Duration `json:"srtLatencyRaiseStep"`
	SRTLatencyStep      Duration `json:"srtLatencyStep"`
	SRTLatencyRaisePct  float64  `json:"srtLatencyRaisePct"`
	SRTLatencyLowerPct  float64  `json:"srtLatencyLowerPct"`

	// SRTLossAlarmEnable reports a publish connection's SRT UNRECOVERABLE
	// loss rate to ppcenter (POST /internal/mmx/v1/alarms/srt-loss) whenever
	// it crosses SRTLossAlarmThresholdPct, and once more when it drops back
	// under - ppcenter raises/auto-resolves a superadmin node alarm from
	// that (see AlarmManager.CheckSRTLoss). Requires MMXControl for the
	// endpoint/credential, same reuse as trafficUsage/splitRecFileReporter.
	//
	// Unrecoverable, not raw SRT loss: raw loss counts packets ARQ
	// retransmits away, so alarming on it pages operators for conditions
	// SRT is designed to absorb. Only packets lost and never retransmitted
	// are counted (see conn.go's runReceiveStatsSummary).
	SRTLossAlarmEnable bool `json:"srtLossAlarmEnable"`
	// SRTLossAlarmThresholdPct is a 0-100 percentage, not a 0-1 ratio. It
	// applies to the UNRECOVERABLE loss rate (see SRTLossAlarmEnable), so a
	// much lower number is appropriate than for raw loss.
	SRTLossAlarmThresholdPct float64 `json:"srtLossAlarmThresholdPct"`
	// SRTLossDisconnectEnable forces a publish connection closed once its
	// unrecoverable loss rate has stayed above SRTLossAlarmThresholdPct
	// continuously for SRTLossDisconnectSec, so a wedged OBS publisher is
	// made to reconnect (typically renegotiating a bitrate the link can
	// carry) instead of degrading indefinitely. Independent of
	// SRTLossAlarmEnable and does not require MMXControl - a node can
	// self-heal with ppcenter unreachable.
	SRTLossDisconnectEnable bool `json:"srtLossDisconnectEnable"`
	SRTLossDisconnectSec    int  `json:"srtLossDisconnectSec"`
	// SRTLossRecycleEnable is a second, independent disconnect tier below the
	// SRTLossDisconnect one above. Where LossDisconnect reacts fast to
	// alarm-level loss (SRTLossAlarmThresholdPct sustained SRTLossDisconnectSec,
	// default 120s) - a link that genuinely can't carry the bitrate - Recycle
	// targets a connection stuck in chronic MILD unrecoverable loss: a low
	// threshold (default 1%) sustained for a long time (default 1h), which the
	// degrade FSM tolerates but a fresh reconnect would likely clear. Both act
	// on the UNRECOVERABLE loss rate (see runReceiveStatsSummary); each has its
	// own tracker so the two thresholds don't interfere. Independent of
	// SRTLossAlarmEnable and does not require MMXControl.
	SRTLossRecycleEnable bool `json:"srtLossRecycleEnable"`
	// SRTLossRecycleThresholdPct is a 0-100 percentage, not a 0-1 ratio,
	// applied to the unrecoverable loss rate. Lower than SRTLossAlarmThresholdPct.
	SRTLossRecycleThresholdPct float64 `json:"srtLossRecycleThresholdPct"`
	// SRTLossRecycleSec is how long unrecoverable loss must stay continuously
	// over SRTLossRecycleThresholdPct before the recycle fires. Long by design
	// (default 3600) so the ~1s reconnect blip stays rare.
	SRTLossRecycleSec int `json:"srtLossRecycleSec"`

	// SRT-simulcast degrade (see docs/obs-mmx-degrade-protocol.md and
	// internal/degrade): shares the same FSM/WS channel as WebRTCDegrade*
	// below (same WebRTCDegradeWSPathSuffix/WebRTCDegradeWSSecret) - only
	// the trigger thresholds are configured separately per protocol, since
	// SRT and WHIP ingest can have different loss characteristics.
	// Requires WebRTC to also be enabled, since that's what serves the
	// degrade WS channel (see Validate). See the WebRTCDegrade*/
	// WebRTCRecover* comment above for why Degrade*/Recover* are split.
	//
	// SRT feeds the FSM its UNRECOVERABLE loss rate (lost minus what ARQ
	// retransmitted - see internal/servers/srt conn.go's
	// unrecoverableAccumulator), NOT raw loss: on a healthy-but-lossy link
	// raw loss (5-10%) is mostly retransmitted away while unrecoverable
	// loss stays ~0.1%, so triggering on raw loss bottoms the ladder out on
	// a condition SRT is designed to absorb. These are 0-100 percentages
	// applied to that unrecoverable rate.
	SRTDegradeEnable         bool    `json:"srtDegradeEnable"`
	SRTDegradeInstantLossPct float64 `json:"srtDegradeInstantLossPct"`
	SRTDegradeAvgLossPct     float64 `json:"srtDegradeAvgLossPct"`
	SRTRecoverInstantLossPct float64 `json:"srtRecoverInstantLossPct"`
	SRTRecoverAvgLossPct     float64 `json:"srtRecoverAvgLossPct"`
	SRTDegradeObservationSec int     `json:"srtDegradeObservationSec"`

	// MoQ server
	MoQ               bool       `json:"moq"`
	MoQHTTP2Address   string     `json:"moqHTTP2Address"`
	MoQHTTP3Address   string     `json:"moqHTTP3Address"`
	MoQServerKey      string     `json:"moqServerKey"`
	MoQServerCert     string     `json:"moqServerCert"`
	MoQAllowOrigins   []string   `json:"moqAllowOrigins"`
	MoQTrustedProxies IPNetworks `json:"moqTrustedProxies"`
	MoQHTTPS2Address  *string    `json:"moqHTTPS2Address,omitempty" deprecated:"true"`
	MoQHTTPS3Address  *string    `json:"moqHTTPS3Address,omitempty" deprecated:"true"`

	// Record (deprecated)
	Record                *bool         `json:"record,omitempty" deprecated:"true"`
	RecordPath            *string       `json:"recordPath,omitempty" deprecated:"true"`
	RecordFormat          *RecordFormat `json:"recordFormat,omitempty" deprecated:"true"`
	RecordPartDuration    *Duration     `json:"recordPartDuration,omitempty" deprecated:"true"`
	RecordSegmentDuration *Duration     `json:"recordSegmentDuration,omitempty" deprecated:"true"`
	RecordDeleteAfter     *Duration     `json:"recordDeleteAfter,omitempty" deprecated:"true"`

	// Path defaults
	PathDefaults Path `json:"pathDefaults"`

	// Paths
	OptionalPaths map[string]*OptionalPath `json:"paths"`
	Paths         map[string]*Path         `json:"-"` // filled by Validate()
}

func (conf *Conf) setDefaults() {
	// General
	conf.LogLevel = LogLevel(logger.Info)
	conf.LogDestinations = LogDestinations{LogDestination(logger.DestinationStdout)}
	conf.LogStructured = false
	conf.LogFile = "mediamtx.log"
	conf.SysLogPrefix = "mediamtx"
	conf.ReadTimeout = 10 * Duration(time.Second)
	conf.WriteTimeout = 10 * Duration(time.Second)
	conf.WriteQueueSize = 512
	conf.UDPMaxPayloadSize = 1452

	// Authentication
	conf.AuthMethod = AuthMethodInternal
	conf.AuthInternalUsers = defaultAuthInternalUsers
	conf.AuthHTTPExclude = []AuthInternalUserPermission{
		{
			Action: AuthActionAPI,
		},
		{
			Action: AuthActionMetrics,
		},
		{
			Action: AuthActionPprof,
		},
	}
	conf.AuthJWTClaimKey = "mediamtx_permissions"

	// Control API
	conf.APIAddress = ":9997"
	conf.APIServerKey = "server.key"
	conf.APIServerCert = "server.crt"
	conf.APIAllowOrigins = []string{"*"}

	// Metrics
	conf.MetricsAddress = ":9998"
	conf.MetricsServerKey = "server.key"
	conf.MetricsServerCert = "server.crt"
	conf.MetricsAllowOrigins = []string{"*"}

	// PPROF
	conf.PPROFAddress = ":9999"
	conf.PPROFServerKey = "server.key"
	conf.PPROFServerCert = "server.crt"
	conf.PPROFAllowOrigins = []string{"*"}

	// Playback server
	conf.PlaybackAddress = ":9996"
	conf.PlaybackServerKey = "server.key"
	conf.PlaybackServerCert = "server.crt"
	conf.PlaybackAllowOrigins = []string{"*"}

	// RTSP server
	conf.RTSP = true
	conf.RTSPEncryption = EncryptionNo
	conf.RTSPTransports = RTSPTransports{
		gortsplib.ProtocolUDP:          {},
		gortsplib.ProtocolUDPMulticast: {},
		gortsplib.ProtocolTCP:          {},
	}
	conf.RTSPAddress = ":8554"
	conf.RTSPSAddress = ":8322"
	conf.RTPAddress = ":8000"
	conf.RTCPAddress = ":8001"
	conf.MulticastIPRange = "224.1.0.0/16"
	conf.MulticastRTPPort = 8002
	conf.MulticastRTCPPort = 8003
	conf.SRTPAddress = ":8004"
	conf.SRTCPAddress = ":8005"
	conf.MulticastSRTPPort = 8006
	conf.MulticastSRTCPPort = 8007
	conf.RTSPServerKey = "server.key"
	conf.RTSPServerCert = "server.crt"
	conf.RTSPAuthMethods = RTSPAuthMethods{RTSPAuthMethod(auth.VerifyMethodBasic)}

	// RTMP server
	conf.RTMP = true
	conf.RTMPEncryption = EncryptionNo
	conf.RTMPAddress = ":1935"
	conf.RTMPSAddress = ":1936"
	conf.RTMPServerKey = "server.key"
	conf.RTMPServerCert = "server.crt"

	// HLS
	conf.HLS = true
	conf.HLSAddress = ":8888"
	conf.HLSServerKey = "server.key"
	conf.HLSServerCert = "server.crt"
	conf.HLSAllowOrigins = []string{"*"}
	conf.HLSVariant = HLSVariant(gohlslib.MuxerVariantLowLatency)
	conf.HLSSegmentCount = 7
	conf.HLSSegmentDuration = 1 * Duration(time.Second)
	conf.HLSPartDuration = 200 * Duration(time.Millisecond)
	conf.HLSSegmentMaxSize = 50 * 1024 * 1024
	conf.HLSMuxerCloseAfter = 60 * Duration(time.Second)

	// WebRTC server
	conf.WebRTC = true
	conf.WebRTCAddress = ":8889"
	conf.WebRTCServerKey = "server.key"
	conf.WebRTCServerCert = "server.crt"
	conf.WebRTCAllowOrigins = []string{"*"}
	conf.WebRTCLocalUDPAddress = ":8189"
	conf.WebRTCIPsFromInterfaces = true
	conf.WebRTCSTUNGatherTimeout = 5 * Duration(time.Second)
	conf.WebRTCHandshakeTimeout = 10 * Duration(time.Second)
	conf.WebRTCTrackGatherTimeout = 2 * Duration(time.Second)
	// 512 packets (vs. gortsplib's 64 default). 64 is only ~200ms of
	// reorder/retransmit window at 5Mbps/30fps, which a NACK round trip
	// plus one keyframe burst can exhaust - the WHIP-side analogue of the
	// too-small SRT latency fixed alongside this. Previously unset here,
	// so every node that didn't override it in YAML silently ran on 64.
	// See the field doc for the sizing rationale.
	conf.WebRTCInboundRTPBufferSize = 512
	conf.WebRTCABREnable = false
	conf.WebRTCABRWSPath = "/ws/control"
	conf.WebRTCABRSwitchCooldown = 3000
	conf.SplitRecAuthMode = "simple"
	conf.WebRTCDegradeEnable = false
	conf.WebRTCDegradeWSPathSuffix = "/ws/whip"
	conf.WebRTCDegradeInstantLossPct = 5.0
	conf.WebRTCDegradeAvgLossPct = 1.0
	// Recover* defaults equal to Degrade* - zero-width dead zone, i.e. the
	// old single-threshold behavior - until an operator opts into
	// hysteresis by lowering Recover* below Degrade*.
	conf.WebRTCRecoverInstantLossPct = 5.0
	conf.WebRTCRecoverAvgLossPct = 1.0
	conf.WebRTCDegradeObservationSec = 60
	// Opt-in, matching SRTLossAlarmEnable - 2.0% chosen as a first-cut
	// default, distinct from SRT's own 10.0% (RTP and SRT loss have
	// different underlying transports/error-recovery, no reason to assume
	// the same number is right for both).
	conf.RTPLossAlarmEnable = false
	conf.RTPLossAlarmThresholdPct = 2.0
	// Mild-but-chronic loss recycle (opt-in, like the alarm). 1% is below the
	// 2.0% alarm - the point is to catch loss too light to page on but that
	// won't clear on its own; 3600s (1h) keeps the forced reconnect rare.
	conf.RTPLossRecycleEnable = false
	conf.RTPLossRecycleThresholdPct = 1.0
	conf.RTPLossRecycleSec = 3600
	// Default ingest source: pull mmx's own Tencent-forwarded backup domain
	// back down and republish it locally. "tencent:" gets txSecret/txTime
	// signed with TX_SECRET_KEY_BACK (see internal/ingest); other prefixes
	// are pulled as-is. ingestThreadEnable defaults to false; opt in per
	// yml with ingestThreadEnable: true.
	conf.IngestThreadEnable = false
	conf.IngestSources = []string{"tencent:rtmp://play.example.com/live/table1-fwh"}
	conf.TencentWHIPEnable = false
	// kept as a literal here (not internal/forward.DefaultTencentEndpoint) to
	// avoid an import cycle: internal/forward depends on
	// internal/protocols/webrtc, which depends on internal/conf.
	//
	// Tencent WHIP push URL: uses webrtc:// scheme format
	conf.TencentWHIPEndpoint = "webrtc://{domain}/{app}/{streamKey}" +
		"?txSecret={txSecret}&txTime={txTime}"
	conf.TencentWHIPTokenDays = 30
	conf.ForwardMmxEnable = false
	conf.AdminAddress = ":8080"
	conf.PlaySignatureTTL = Duration(time.Hour)

	// SRT server
	conf.SRT = true
	conf.SRTAddress = ":8890"
	// 500ms default (~15x a typical ~32ms RTT), configurable per-node via
	// the srtLatency YAML key. The previous 2000ms was ~60x RTT and
	// pushed end-to-end P2P delay past 2 seconds for HEVC viewers; 500ms
	// still leaves enough margin for NAK-triggered retransmits on a
	// clean public-internet link while keeping ingest latency reasonable.
	// Only the seed value for a path not yet tuned by SRTLatencyAutoTune
	// below - it is never rewritten.
	conf.SRTLatency = 500 * Duration(time.Millisecond)
	conf.SRTReceiverBufferSize = 2 * 1024 * 1024
	// 65536 packets (vs. gosrt's 25600 default): the flow-control window
	// has to scale with SRTLatency above, otherwise the sender is capped
	// on in-flight packets long before the bigger latency window can
	// actually be used. At 5Mbps x 3 simulcast layers with a 2s window,
	// 25600 packets is not enough headroom. Also the floor srtWorstCaseBuffer
	// sizes up from for SRTLatencyMax below.
	conf.SRTFlowControlWindow = 65536
	// SRT adaptive receive latency (see docs/srt-adaptive-latency-design.md).
	// SRTLatencyMin is below SRTLatency so a quiet path can be tuned down
	// past its seed value; the seed must still start inside the range
	// (enforced by Validate).
	conf.SRTLatencyAutoTune = true
	conf.SRTLatencyMin = 300 * Duration(time.Millisecond)
	conf.SRTLatencyMax = 3000 * Duration(time.Millisecond)
	conf.SRTLatencyStep = 100 * Duration(time.Millisecond)
	conf.SRTLatencyRaiseStep = 200 * Duration(time.Millisecond)
	conf.SRTLatencyRaisePct = 1.0
	conf.SRTLatencyLowerPct = 0.1
	// Both alarm/disconnect flags default off (opt-in, matching
	// WebRTCDegradeEnable) so turning either on "just works" with these
	// numbers without also having to set the threshold/duration.
	conf.SRTLossAlarmEnable = false
	// 1% unrecoverable loss: raw-loss defaults (10%) do not carry over, since
	// ARQ hides most raw loss. Measured healthy baseline on the SGP ingest
	// path is ~0.13% unrecovered against ~5-10% raw loss.
	conf.SRTLossAlarmThresholdPct = 1.0
	conf.SRTLossDisconnectEnable = false
	conf.SRTLossDisconnectSec = 120
	// Second, slower disconnect tier for chronic mild loss (see
	// SRTLossRecycleEnable). 1% unrecoverable sustained for 1h - below the
	// alarm/LossDisconnect threshold, far longer than its 120s.
	conf.SRTLossRecycleEnable = false
	conf.SRTLossRecycleThresholdPct = 1.0
	conf.SRTLossRecycleSec = 3600
	// Same starting numbers as WebRTCDegrade*'s own defaults below, absent
	// any SRT-specific tuning data yet - independently adjustable per
	// protocol once real-world loss characteristics diverge. Applied to
	// SRT's UNRECOVERABLE loss rate (see SRTDegradeEnable above).
	conf.SRTDegradeEnable = false
	conf.SRTDegradeInstantLossPct = 5.0
	conf.SRTDegradeAvgLossPct = 1.0
	conf.SRTRecoverInstantLossPct = 5.0
	conf.SRTRecoverAvgLossPct = 1.0
	conf.SRTDegradeObservationSec = 60

	// MoQ server
	conf.MoQ = true
	conf.MoQHTTP2Address = ":8892"
	conf.MoQHTTP3Address = ":8892"
	conf.MoQServerKey = "auto.key"
	conf.MoQServerCert = "auto.crt"
	conf.MoQAllowOrigins = []string{"*"}

	conf.PathDefaults.setDefaults()
}

// Load loads a Conf.
func Load(fpath string, defaultConfPaths []string, l logger.Writer) (*Conf, string, error) {
	conf := &Conf{}

	conf.setDefaults()

	fpath, err := conf.loadFromFile(fpath, defaultConfPaths)
	if err != nil {
		return nil, "", err
	}

	err = env.Load("RTSP", conf) // legacy prefix
	if err != nil {
		return nil, "", err
	}

	err = env.Load("MTX", conf)
	if err != nil {
		return nil, "", err
	}
	conf.TencentWHIPSecretKey = DotenvValue("TX_SECRET_KEY")
	conf.TXSecretKeyBack = DotenvValue("TX_SECRET_KEY_BACK")
	conf.WebRTCDegradeWSSecret = DotenvValue("WHIP_WS_SECRET")
	conf.WebRTCWHIPAuthKey = DotenvValue("WHIP_AUTH_KEY")
	conf.WebRTCForwardSecret = DotenvValue("MMX_FORWARD_SECRET")
	conf.MMXNodeSecret = DotenvValue("MMX_NODE_SECRET")
	// bin/.env keys: "prod" selects S3, anything else selects MinIO.
	conf.NetStorageEnv = DotenvValue("APP_ENV")
	conf.NetStorageS3Bucket = DotenvValue("S3_BUCKET")
	conf.NetStorageS3Region = DotenvValue("S3_REGION")
	conf.NetStorageS3AccessKey = DotenvValue("S3_ACCESS_ID")
	conf.NetStorageS3SecretKey = DotenvValue("S3_ACCESS_PASS")
	conf.NetStorageS3Domain = DotenvValue("S3_HTTPS_DOMAIN")
	// Optional custom S3 API endpoint for S3-compatible providers (e.g. OVH:
	// "https://s3.sgp.io.cloud.ovh.net"). Empty = the AWS SDK's default AWS
	// endpoint for S3_REGION. Sourced from .env so all S3 settings live in one
	// place (rather than needing AWS_ENDPOINT_URL_S3 in the process env).
	conf.NetStorageS3Endpoint = DotenvValue("S3_ENDPOINT_URL")
	// Canned ACL sent with every uploaded object (e.g. "public-read",
	// "private" - see AWS's ObjectCannedACL). Empty omits the ACL header
	// entirely, so the object falls back to the bucket's own default
	// ACL/policy - the original, implicit behavior.
	conf.NetStorageS3ACL = DotenvValue("S3_ACL")
	// Nacos (optional): if BOOTSTRAP_JASYPT_ENCRYPTOR_PASSWORD is set, fetch
	// MinIO settings from Nacos first; the MINIO_* env vars below still
	// override on top when set, same precedence as the previous Go service.
	applyNacosMinioConfig(conf, l)
	if v := DotenvValue("MINIO_EP"); v != "" {
		conf.NetStorageMinioEndpoint = v
	}
	if v := DotenvValue("MINIO_AK"); v != "" {
		conf.NetStorageMinioAccessKey = v
	}
	if v := DotenvValue("MINIO_SK"); v != "" {
		conf.NetStorageMinioSecretKey = v
	}
	if v := DotenvValue("MINIO_USESSL"); v != "" {
		conf.NetStorageMinioUseSSL = v
	}
	if v := DotenvValue("MINIO_URL"); v != "" {
		conf.NetStorageMinioDomain = v
	}
	if v, err := strconv.Atoi(DotenvValue("PLAY_URI_RATE_LIMIT")); err == nil {
		conf.PlayURIRateLimit = v
	}

	// disallow nil slices for ease of use and compatibility
	setAllNilSlicesToEmptyRecursive(reflect.ValueOf(conf))

	err = conf.Validate(l)
	if err != nil {
		return nil, "", err
	}

	return conf, fpath, nil
}

func (conf *Conf) loadFromFile(fpath string, defaultConfPaths []string) (string, error) {
	if fpath == "" {
		fpath = firstThatExists(defaultConfPaths)

		// when the configuration file is not explicitly set,
		// it is optional.
		if fpath == "" {
			return "", nil
		}
	}

	byts, err := os.ReadFile(fpath)
	if err != nil {
		return "", err
	}

	if key, ok := os.LookupEnv("RTSP_CONFKEY"); ok { // legacy format
		byts, err = decrypt.Decrypt(key, byts)
		if err != nil {
			return "", err
		}
	}

	if key, ok := os.LookupEnv("MTX_CONFKEY"); ok {
		byts, err = decrypt.Decrypt(key, byts)
		if err != nil {
			return "", err
		}
	}

	err = yamlwrapper.Unmarshal(byts, conf)
	if err != nil {
		return "", err
	}

	return fpath, nil
}

// Clone clones the configuration.
func (conf Conf) Clone() *Conf {
	cloned := deepClone(reflect.ValueOf(conf)).Interface().(Conf)
	return &cloned
}

// Validate checks the configuration for errors, converts deprecated fields into new ones, fills dependent fields.
func (conf *Conf) Validate(l logger.Writer) error {
	if l == nil {
		l = &nilLogger{}
	}

	// General (deprecated params)

	if conf.ReadBufferCount != nil {
		l.Log(logger.Warn, "parameter 'readBufferCount' is deprecated and has been replaced with 'writeQueueSize'")
		conf.WriteQueueSize = *conf.ReadBufferCount
	}

	// General

	if conf.ReadTimeout <= 0 {
		return fmt.Errorf("'readTimeout' must be greater than zero")
	}

	if conf.WriteTimeout <= 0 {
		return fmt.Errorf("'writeTimeout' must be greater than zero")
	}

	if conf.WriteQueueSize <= 0 {
		return fmt.Errorf("'writeQueueSize' must be greater than zero")
	}

	if (conf.WriteQueueSize & (conf.WriteQueueSize - 1)) != 0 {
		return fmt.Errorf("'writeQueueSize' must be a power of two")
	}

	if conf.UDPMaxPayloadSize > 1472 {
		return fmt.Errorf("'udpMaxPayloadSize' must be less than 1472")
	}

	// Authentication (deprecated params)

	if conf.ExternalAuthenticationURL != nil {
		l.Log(logger.Warn, "parameter 'externalAuthenticationURL' is deprecated "+
			"and has been replaced with 'authMethod' and 'authHTTPAddress'")
		conf.AuthMethod = AuthMethodHTTP
		conf.AuthHTTPAddress = *conf.ExternalAuthenticationURL
	}

	deprecatedCredentialsMode := false
	if anyPathHasDeprecatedCredentials(conf.PathDefaults, conf.OptionalPaths) {
		l.Log(logger.Warn, "you are using one or more authentication-related deprecated parameters "+
			"(publishUser, publishPass, publishIPs, readUser, readPass, readIPs). "+
			"These have been replaced by 'authInternalUsers'")

		if conf.AuthInternalUsers != nil && !reflect.DeepEqual(conf.AuthInternalUsers, defaultAuthInternalUsers) {
			return fmt.Errorf("authInternalUsers and legacy credentials " +
				"(publishUser, publishPass, publishIPs, readUser, readPass, readIPs) cannot be used together")
		}

		conf.AuthInternalUsers = []AuthInternalUser{
			{
				User: "any",
				Permissions: []AuthInternalUserPermission{
					{
						Action: AuthActionPlayback,
					},
				},
			},
			{
				User: "any",
				IPs:  IPNetworks{mustParseCIDR("127.0.0.1/32"), mustParseCIDR("::1/128")},
				Permissions: []AuthInternalUserPermission{
					{
						Action: AuthActionAPI,
					},
					{
						Action: AuthActionMetrics,
					},
					{
						Action: AuthActionPprof,
					},
				},
			},
		}
		deprecatedCredentialsMode = true
	}

	// Authentication

	switch conf.AuthMethod {
	case AuthMethodInternal:
		for _, u := range conf.AuthInternalUsers {
			// https://github.com/bluenviron/gortsplib/blob/55556f1ecfa2bd51b29fe14eddd70512a0361cbd/server_conn.go#L155-L156
			if u.User == "" {
				return fmt.Errorf("empty usernames are not supported")
			}

			if u.User == "any" && u.Pass != "" {
				return fmt.Errorf("using a password with 'any' user is not supported")
			}
		}

	case AuthMethodHTTP:
		if conf.AuthHTTPAddress == "" {
			return fmt.Errorf("'authHTTPAddress' is empty")
		}

		if conf.AuthHTTPAddress != "" &&
			!strings.HasPrefix(conf.AuthHTTPAddress, "http://") &&
			!strings.HasPrefix(conf.AuthHTTPAddress, "https://") {
			return fmt.Errorf("'externalAuthenticationURL' must be a HTTP URL")
		}

	case AuthMethodJWT:
		if conf.AuthJWTJWKS == "" {
			return fmt.Errorf("'authJWTJWKS' is empty")
		}

		if conf.AuthJWTJWKS != "" &&
			!strings.HasPrefix(conf.AuthJWTJWKS, "http://") &&
			!strings.HasPrefix(conf.AuthJWTJWKS, "https://") {
			return fmt.Errorf("'authJWTJWKS' must be a HTTP URL")
		}

		if conf.AuthJWTClaimKey == "" {
			return fmt.Errorf("'authJWTClaimKey' is empty")
		}
	}

	if conf.AuthJWTInHTTPQuery != nil {
		l.Log(logger.Warn, "parameter 'authJWTInHTTPQuery' is deprecated and will be removed in a future release")
	}

	// Control API (deprecated params)

	if conf.APIAllowOrigin != nil {
		l.Log(logger.Warn, "parameter 'apiAllowOrigin' is deprecated and has been replaced with 'apiAllowOrigins'")
		conf.APIAllowOrigins = []string{*conf.APIAllowOrigin}
	}

	// Control API

	if conf.API {
		if conf.APIAddress == "" {
			return fmt.Errorf("'apiAddress' must be set when API is enabled")
		}
	}

	// Metrics (deprecated params)

	if conf.MetricsAllowOrigin != nil {
		l.Log(logger.Warn, "parameter 'metricsAllowOrigin' is deprecated and has been replaced with 'metricsAllowOrigins'")
		conf.MetricsAllowOrigins = []string{*conf.MetricsAllowOrigin}
	}

	// Metrics

	if conf.Metrics {
		if conf.MetricsAddress == "" {
			return fmt.Errorf("'metricsAddress' must be set when metrics are enabled")
		}
	}

	// PPROF (deprecated params)

	if conf.PPROFAllowOrigin != nil {
		l.Log(logger.Warn, "parameter 'pprofAllowOrigin' is deprecated and has been replaced with 'pprofAllowOrigins'")
		conf.PPROFAllowOrigins = []string{*conf.PPROFAllowOrigin}
	}

	// PPROF

	if conf.PPROF {
		if conf.PPROFAddress == "" {
			return fmt.Errorf("'pprofAddress' must be set when pprof is enabled")
		}
	}

	// Playback (deprecated params)

	if conf.PlaybackAllowOrigin != nil {
		l.Log(logger.Warn, "parameter 'playbackAllowOrigin' is deprecated and has been replaced with 'playbackAllowOrigins'")
		conf.PlaybackAllowOrigins = []string{*conf.PlaybackAllowOrigin}
	}

	// Playback

	if conf.Playback {
		if conf.PlaybackAddress == "" {
			return fmt.Errorf("'playbackAddress' must be set when playback is enabled")
		}
	}

	// RTSP server (deprecated params)

	if conf.RTSPDisable != nil {
		l.Log(logger.Warn, "parameter 'rtspDisabled' is deprecated and has been replaced with 'rtsp'")
		conf.RTSP = !*conf.RTSPDisable
	}

	if conf.Protocols != nil {
		l.Log(logger.Warn, "parameter 'protocols' is deprecated and has been replaced with 'rtspTransports'")
		conf.RTSPTransports = *conf.Protocols
	}

	if conf.Encryption != nil {
		l.Log(logger.Warn, "parameter 'encryption' is deprecated and has been replaced with 'rtspEncryption'")
		conf.RTSPEncryption = *conf.Encryption
	}

	if conf.AuthMethods != nil {
		l.Log(logger.Warn, "parameter 'authMethods' is deprecated and has been replaced with 'rtspAuthMethods'")
		conf.RTSPAuthMethods = *conf.AuthMethods
	}

	if conf.ServerCert != nil {
		l.Log(logger.Warn, "parameter 'serverCert' is deprecated and has been replaced with 'rtspServerCert'")
		conf.RTSPServerCert = *conf.ServerCert
	}

	if conf.ServerKey != nil {
		l.Log(logger.Warn, "parameter 'serverKey' is deprecated and has been replaced with 'rtspServerKey'")
		conf.RTSPServerKey = *conf.ServerKey
	}

	// RTSP server

	if conf.RTSP {
		if conf.RTSPEncryption == EncryptionNo || conf.RTSPEncryption == EncryptionOptional {
			if conf.RTSPAddress == "" {
				return fmt.Errorf("'rtspAddress' must be set when RTSP is enabled and RTSP encryption is 'no' or 'optional'")
			}

			if _, ok := conf.RTSPTransports[gortsplib.ProtocolUDP]; ok {
				if conf.RTPAddress == "" {
					return fmt.Errorf("'rtpAddress' must be set when UDP is enabled and RTSP encryption is 'no' or 'optional'")
				}
				if conf.RTCPAddress == "" {
					return fmt.Errorf("'rtcpAddress' must be set when UDP is enabled and RTSP encryption is 'no' or 'optional'")
				}
			}

			if _, ok := conf.RTSPTransports[gortsplib.ProtocolUDPMulticast]; ok {
				if conf.MulticastIPRange == "" {
					return fmt.Errorf("'multicastIPRange' must be set when UDP multicast is enabled" +
						" and RTSP encryption is 'no' or 'optional'")
				}
				if conf.MulticastRTPPort == 0 {
					return fmt.Errorf("'multicastRTPPort' must be set when UDP multicast is enabled" +
						" and RTSP encryption is 'no' or 'optional'")
				}
				if conf.MulticastRTCPPort == 0 {
					return fmt.Errorf("'multicastRTCPPort' must be set when UDP multicast is enabled" +
						" and RTSP encryption is 'no' or 'optional'")
				}
			}
		}

		if conf.RTSPEncryption == EncryptionOptional || conf.RTSPEncryption == EncryptionStrict {
			if conf.RTSPSAddress == "" {
				return fmt.Errorf("'rtspsAddress' must be set when RTSP is enabled and RTSP encryption is 'optional' or 'strict'")
			}

			if _, ok := conf.RTSPTransports[gortsplib.ProtocolUDP]; ok {
				if conf.SRTPAddress == "" {
					return fmt.Errorf("'srtpAddress' must be set when UDP is enabled" +
						" and RTSP encryption is 'optional' or 'strict'")
				}
				if conf.SRTCPAddress == "" {
					return fmt.Errorf("'srtcpAddress' must be set when UDP is enabled" +
						" and RTSP encryption is 'optional' or 'strict'")
				}
			}

			if _, ok := conf.RTSPTransports[gortsplib.ProtocolUDPMulticast]; ok {
				if conf.MulticastIPRange == "" {
					return fmt.Errorf("'multicastIPRange' must be set when UDP multicast is enabled" +
						" and RTSP encryption is 'optional' or 'strict'")
				}
				if conf.MulticastSRTPPort == 0 {
					return fmt.Errorf("'multicastSRTPPort' must be set when UDP multicast is enabled" +
						" and RTSP encryption is 'optional' or 'strict'")
				}
				if conf.MulticastSRTCPPort == 0 {
					return fmt.Errorf("'multicastSRTCPPort' must be set when UDP multicast is enabled" +
						" and RTSP encryption is 'optional' or 'strict'")
				}
			}
		}

		if len(conf.RTSPAuthMethods) == 0 {
			return fmt.Errorf("at least one 'rtspAuthMethods' must be provided")
		}

		if slices.Contains(conf.RTSPAuthMethods, RTSPAuthMethod(auth.VerifyMethodDigestMD5)) {
			if conf.AuthMethod != AuthMethodInternal {
				return fmt.Errorf("when RTSP digest is enabled, the only supported auth method is 'internal'")
			}
			for _, user := range conf.AuthInternalUsers {
				if user.User.IsHashed() || user.Pass.IsHashed() {
					return fmt.Errorf("when RTSP digest is enabled, hashed credentials cannot be used")
				}
			}
		}
	}

	// RTMP (deprecated params)

	if conf.RTMPDisable != nil {
		l.Log(logger.Warn, "parameter 'rtmpDisabled' is deprecated and has been replaced with 'rtmp'")
		conf.RTMP = !*conf.RTMPDisable
	}

	// RTMP

	if conf.RTMP {
		if conf.RTMPAddress == "" {
			return fmt.Errorf("'rtmpAddress' must be set when RTMP is enabled")
		}
	}

	// HLS (deprecated params)

	if conf.HLSDisable != nil {
		l.Log(logger.Warn, "parameter 'hlsDisable' is deprecated and has been replaced with 'hls'")
		conf.HLS = !*conf.HLSDisable
	}

	if conf.HLSAllowOrigin != nil {
		l.Log(logger.Warn, "parameter 'hlsAllowOrigin' is deprecated and has been replaced with 'hlsAllowOrigins'")
		conf.HLSAllowOrigins = []string{*conf.HLSAllowOrigin}
	}

	// HLS

	if conf.HLS {
		if conf.HLSAddress == "" {
			return fmt.Errorf("'hlsAddress' must be set when HLS is enabled")
		}
	}

	if conf.HLSCDNSecret != "" {
		if !rePlainCredential.MatchString(conf.HLSCDNSecret) {
			return fmt.Errorf("'hlsCDNSecret' contains unsupported characters. Supported are: %s", plainCredentialSupportedChars)
		}
	}

	// WebRTC (deprecated params)

	if conf.WebRTCDisable != nil {
		l.Log(logger.Warn, "parameter 'webrtcDisable' is deprecated and has been replaced with 'webrtc'")
		conf.WebRTC = !*conf.WebRTCDisable
	}

	if conf.WebRTCICEUDPMuxAddress != nil {
		l.Log(logger.Warn, "parameter 'webrtcICEUDPMuxAdderss' is deprecated "+
			"and has been replaced with 'webrtcLocalUDPAddress'")
		conf.WebRTCLocalUDPAddress = *conf.WebRTCICEUDPMuxAddress
	}

	if conf.WebRTCICETCPMuxAddress != nil {
		l.Log(logger.Warn, "parameter 'webrtcICETCPMuxAddress' is deprecated "+
			"and has been replaced with 'webrtcLocalTCPAddress'")
		conf.WebRTCLocalTCPAddress = *conf.WebRTCICETCPMuxAddress
	}

	if conf.WebRTCICEHostNAT1To1IPs != nil {
		l.Log(logger.Warn, "parameter 'webrtcICEHostNAT1To1IPs' is deprecated "+
			"and has been replaced with 'webrtcAdditionalHosts'")
		conf.WebRTCAdditionalHosts = *conf.WebRTCICEHostNAT1To1IPs
	}

	if conf.WebRTCICEServers != nil {
		l.Log(logger.Warn, "parameter 'webrtcICEServers' is deprecated "+
			"and has been replaced with 'webrtcICEServers2'")

		for _, server := range *conf.WebRTCICEServers {
			parts := strings.Split(server, ":")
			if len(parts) == 5 {
				conf.WebRTCICEServers2 = append(conf.WebRTCICEServers2, WebRTCICEServer{
					URL:      parts[0] + ":" + parts[3] + ":" + parts[4],
					Username: parts[1],
					Password: parts[2],
				})
			} else {
				conf.WebRTCICEServers2 = append(conf.WebRTCICEServers2, WebRTCICEServer{
					URL: server,
				})
			}
		}
	}

	if conf.WebRTCAllowOrigin != nil {
		l.Log(logger.Warn, "parameter 'webrtcAllowOrigin' is deprecated and has been replaced with 'webrtcAllowOrigins'")
		conf.WebRTCAllowOrigins = []string{*conf.WebRTCAllowOrigin}
	}

	// WebRTC

	if conf.WebRTC {
		if conf.WebRTCAddress == "" {
			return fmt.Errorf("'webrtcAddress' must be set when WebRTC is enabled")
		}

		for _, server := range conf.WebRTCICEServers2 {
			if !strings.HasPrefix(server.URL, "stun:") &&
				!strings.HasPrefix(server.URL, "turn:") &&
				!strings.HasPrefix(server.URL, "turns:") {
				return fmt.Errorf("invalid ICE server: '%s'", server.URL)
			}
		}

		if conf.WebRTCLocalUDPAddress == "" &&
			conf.WebRTCLocalTCPAddress == "" &&
			len(conf.WebRTCICEServers2) == 0 {
			return fmt.Errorf("at least one between 'webrtcLocalUDPAddress'," +
				" 'webrtcLocalTCPAddress' or 'webrtcICEServers2' must be filled")
		}

		// webrtcAdditionalHosts empty with webrtcIPsFromInterfaces explicitly
		// false is a redundant/contradictory combination (there'd be no way
		// to discover any host candidate at all); silently fall back to
		// discovering IPs from interfaces instead of refusing to start.
		if conf.WebRTCLocalUDPAddress != "" || conf.WebRTCLocalTCPAddress != "" {
			if !conf.WebRTCIPsFromInterfaces && len(conf.WebRTCAdditionalHosts) == 0 {
				conf.WebRTCIPsFromInterfaces = true
			}
		}
	}

	// Forward (Tencent Cloud WHIP relay)
	if conf.SplitRecAuthMode != "simple" && conf.SplitRecAuthMode != "advance" {
		return fmt.Errorf("'splitRecAuthMode' must be either 'simple' or 'advance'")
	}

	if conf.WebRTCDegradeEnable && conf.WebRTCDegradeWSSecret == "" {
		return fmt.Errorf("WHIP_WS_SECRET must be set when webrtcDegradeEnable is true")
	}
	if conf.WebRTCDegradeEnable && conf.WebRTCRecoverInstantLossPct > conf.WebRTCDegradeInstantLossPct {
		return fmt.Errorf("'webrtcRecoverInstantLossPct' must be <= 'webrtcDegradeInstantLossPct' " +
			"(recovering must require loss to fall back to a rate at least as strict as the one that triggered degrading)")
	}
	if conf.WebRTCDegradeEnable && conf.WebRTCRecoverAvgLossPct > conf.WebRTCDegradeAvgLossPct {
		return fmt.Errorf("'webrtcRecoverAvgLossPct' must be <= 'webrtcDegradeAvgLossPct'")
	}

	// SRT transport tuning (see the field doc comments). The FC-vs-buffer
	// check mirrors SRT's own requirement: a receive buffer bigger than
	// the flow-control window is silently clamped to it, so a config
	// where they disagree would not actually buffer what it claims to.
	// srtReceiverBufferSize needs no >0 check: StringSize's own parser
	// already rejects zero/negative values with a clearer message.
	if conf.SRTLatency <= 0 {
		return fmt.Errorf("'srtLatency' must be greater than zero")
	}
	if conf.SRTFlowControlWindow <= 0 {
		return fmt.Errorf("'srtFlowControlWindow' must be greater than zero")
	}
	if bufPackets := int(conf.SRTReceiverBufferSize) / srtMinPayloadSize; conf.SRTFlowControlWindow < bufPackets {
		return fmt.Errorf("'srtFlowControlWindow' (%d) is too small for 'srtReceiverBufferSize' (%d): "+
			"it must be at least %d packets, otherwise SRT clamps the usable receive buffer to the window",
			conf.SRTFlowControlWindow, int(conf.SRTReceiverBufferSize), bufPackets)
	}

	// SRT adaptive receive latency (see the field doc comments and
	// docs/srt-adaptive-latency-design.md). Only validated when enabled -
	// an auto-tune-disabled deployment never reads these.
	if conf.SRTLatencyAutoTune {
		if conf.SRTLatencyMin <= 0 {
			return fmt.Errorf("'srtLatencyMin' must be greater than zero")
		}
		if conf.SRTLatencyMax < conf.SRTLatencyMin {
			return fmt.Errorf("'srtLatencyMax' must be >= 'srtLatencyMin'")
		}
		if conf.SRTLatency < conf.SRTLatencyMin {
			return fmt.Errorf("'srtLatency' must be >= 'srtLatencyMin', otherwise a newly seen " +
				"path's initial value is immediately out of its own tunable range")
		}
		if conf.SRTLatencyStep <= 0 {
			return fmt.Errorf("'srtLatencyStep' must be greater than zero")
		}
		if conf.SRTLatencyRaiseStep <= 0 {
			return fmt.Errorf("'srtLatencyRaiseStep' must be greater than zero")
		}
		if conf.SRTLatencyLowerPct >= conf.SRTLatencyRaisePct {
			return fmt.Errorf("'srtLatencyLowerPct' must be < 'srtLatencyRaisePct', " +
				"otherwise there is no hysteresis band and the tuned value oscillates")
		}
	}

	if conf.SRTDegradeEnable && conf.WebRTCDegradeWSSecret == "" {
		return fmt.Errorf("WHIP_WS_SECRET must be set when srtDegradeEnable is true")
	}
	if conf.SRTDegradeEnable && !conf.WebRTC {
		return fmt.Errorf("'webrtc' must be enabled when srtDegradeEnable is true (the degrade WS channel is served by the WebRTC server)")
	}
	if conf.SRTDegradeEnable && conf.SRTRecoverInstantLossPct > conf.SRTDegradeInstantLossPct {
		return fmt.Errorf("'srtRecoverInstantLossPct' must be <= 'srtDegradeInstantLossPct' " +
			"(recovering must require loss to fall back to a rate at least as strict as the one that triggered degrading)")
	}
	if conf.SRTDegradeEnable && conf.SRTRecoverAvgLossPct > conf.SRTDegradeAvgLossPct {
		return fmt.Errorf("'srtRecoverAvgLossPct' must be <= 'srtDegradeAvgLossPct'")
	}

	if conf.MMXControl {
		u, err := url.Parse(conf.MMXControlURL)
		if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
			return fmt.Errorf("'mmxControlURL' must be a valid ws or wss URL")
		}
		if conf.MMXNodeRole != "NODE_ROLE_ORIGIN" && conf.MMXNodeRole != "NODE_ROLE_EDGE" && conf.MMXNodeRole != "NODE_ROLE_RECORDER" {
			return fmt.Errorf("'mmxNodeRole' must be NODE_ROLE_ORIGIN, NODE_ROLE_EDGE or NODE_ROLE_RECORDER")
		}
		if strings.TrimSpace(conf.MMXNodeSecret) == "" {
			return fmt.Errorf("MMX_NODE_SECRET must be set (in .env) when mmxControl is true")
		}
		if strings.TrimSpace(conf.MMXNodeRegion) == "" {
			return fmt.Errorf("'mmxNodeRegion' is required")
		}
		if conf.MMXNodeCapacity <= 0 {
			return fmt.Errorf("'mmxNodeCapacity' must be greater than zero")
		}
		if conf.MMXHeartbeatInterval <= 0 {
			return fmt.Errorf("'mmxHeartbeatInterval' must be greater than zero")
		}
		if conf.MMXNodeRole == "NODE_ROLE_EDGE" && strings.TrimSpace(conf.MMXWebRTCBaseURL) == "" {
			return fmt.Errorf("'mmxWebRTCBaseURL' is required for Edge nodes")
		}
	}

	if conf.MMXRecordingSyncEnabled {
		u, err := url.Parse(conf.MMXRecordingSyncURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("'mmxRecordingSyncURL' must be a valid http or https URL")
		}
		if len(strings.TrimSpace(conf.MMXRecordingSyncBearerToken)) < 16 {
			return fmt.Errorf("'mmxRecordingSyncBearerToken' must be at least 16 characters")
		}
		if conf.MMXRecordingSyncPollInterval <= 0 {
			return fmt.Errorf("'mmxRecordingSyncPollInterval' must be greater than zero")
		}
	}

	if conf.TencentWHIPEnable {
		if conf.TencentWHIPEndpoint == "" {
			return fmt.Errorf("'tencentWHIPEndpoint' must be set when tencentWHIPEnable is true")
		}
		if conf.TencentWHIPDomain == "" {
			return fmt.Errorf("'tencentWHIPDomain' must be set when tencentWHIPEnable is true")
		}
		if conf.TencentWHIPApp == "" {
			return fmt.Errorf("'tencentWHIPApp' must be set when tencentWHIPEnable is true")
		}
		if conf.TencentWHIPSecretKey == "" {
			return fmt.Errorf("'tencentWHIPSecretKey' must be set when tencentWHIPEnable is true")
		}
		if conf.TencentWHIPTokenDays <= 0 {
			return fmt.Errorf("'tencentWHIPTokenDays' must be greater than zero")
		}
	}

	if conf.MoQHTTPS2Address != nil {
		l.Log(logger.Warn, "parameter 'moqHTTPS2Address' is deprecated "+
			"and has been replaced with 'moqHTTP2Address'")
		conf.MoQHTTP2Address = *conf.MoQHTTPS2Address
	}

	if conf.MoQHTTPS3Address != nil {
		l.Log(logger.Warn, "parameter 'moqHTTPS3Address' is deprecated "+
			"and has been replaced with 'moqHTTP3Address'")
		conf.MoQHTTP3Address = *conf.MoQHTTPS3Address
	}

	// Record (deprecated)

	if conf.Record != nil {
		l.Log(logger.Warn, "parameter 'record' is deprecated "+
			"and has been replaced with 'pathDefaults.record'")
		conf.PathDefaults.Record = *conf.Record
	}

	if conf.RecordPath != nil {
		l.Log(logger.Warn, "parameter 'recordPath' is deprecated "+
			"and has been replaced with 'pathDefaults.recordPath'")
		conf.PathDefaults.RecordPath = *conf.RecordPath
	}

	if conf.RecordFormat != nil {
		l.Log(logger.Warn, "parameter 'recordFormat' is deprecated "+
			"and has been replaced with 'pathDefaults.recordFormat'")
		conf.PathDefaults.RecordFormat = *conf.RecordFormat
	}

	if conf.RecordPartDuration != nil {
		l.Log(logger.Warn, "parameter 'recordPartDuration' is deprecated "+
			"and has been replaced with 'pathDefaults.recordPartDuration'")
		conf.PathDefaults.RecordPartDuration = *conf.RecordPartDuration
	}

	if conf.RecordSegmentDuration != nil {
		l.Log(logger.Warn, "parameter 'recordSegmentDuration' is deprecated "+
			"and has been replaced with 'pathDefaults.recordSegmentDuration'")
		conf.PathDefaults.RecordSegmentDuration = *conf.RecordSegmentDuration
	}

	if conf.RecordDeleteAfter != nil {
		l.Log(logger.Warn, "parameter 'recordDeleteAfter' is deprecated "+
			"and has been replaced with 'pathDefaults.recordDeleteAfter'")
		conf.PathDefaults.RecordDeleteAfter = *conf.RecordDeleteAfter
	}

	// paths

	hasAllOthers := false
	for name := range conf.OptionalPaths {
		if name == "all" || name == "all_others" || name == "~^.*$" {
			if hasAllOthers {
				return fmt.Errorf("all_others, all and '~^.*$' are aliases")
			}
			hasAllOthers = true
		}
	}

	conf.Paths = make(map[string]*Path)

	for _, name := range sortedKeys(conf.OptionalPaths) {
		optional := conf.OptionalPaths[name]
		if optional == nil {
			optional = &OptionalPath{
				Values: newOptionalPathValues(),
			}
			conf.OptionalPaths[name] = optional
		}

		pconf := newPath(&conf.PathDefaults, optional)
		conf.Paths[name] = pconf
	}

	for _, name := range sortedKeys(conf.OptionalPaths) {
		err := conf.Paths[name].validate(conf, name, deprecatedCredentialsMode, l)
		if err != nil {
			return err
		}
	}

	return nil
}

// Global returns the global part of Conf.
func (conf *Conf) Global() *Global {
	g := &Global{
		Values: newGlobalValues(),
	}
	copyStructFields(g.Values, conf)
	return g
}

// PatchGlobal patches the global configuration.
func (conf *Conf) PatchGlobal(optional *OptionalGlobal) {
	copyStructFields(conf, optional.Values)
}

// PatchPathDefaults patches path default settings.
func (conf *Conf) PatchPathDefaults(optional *OptionalPath) {
	copyStructFields(&conf.PathDefaults, optional.Values)
}

// AddPath adds a path.
func (conf *Conf) AddPath(name string, p *OptionalPath) error {
	if _, ok := conf.OptionalPaths[name]; ok {
		return fmt.Errorf("path already exists")
	}

	if conf.OptionalPaths == nil {
		conf.OptionalPaths = make(map[string]*OptionalPath)
	}

	conf.OptionalPaths[name] = p
	return nil
}

// PatchPath patches a path.
func (conf *Conf) PatchPath(name string, optional2 *OptionalPath) error {
	optional, ok := conf.OptionalPaths[name]
	if !ok {
		return ErrPathNotFound
	}

	copyStructFields(optional.Values, optional2.Values)
	return nil
}

// ReplacePath replaces a path.
func (conf *Conf) ReplacePath(name string, optional2 *OptionalPath) error {
	if conf.OptionalPaths == nil {
		conf.OptionalPaths = make(map[string]*OptionalPath)
	}

	conf.OptionalPaths[name] = optional2
	return nil
}

// RemovePath removes a path.
func (conf *Conf) RemovePath(name string) error {
	if _, ok := conf.OptionalPaths[name]; !ok {
		return ErrPathNotFound
	}

	delete(conf.OptionalPaths, name)
	return nil
}
