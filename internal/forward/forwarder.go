package forward

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	"github.com/bluenviron/mediamtx/internal/stream"
	pwebrtc "github.com/pion/webrtc/v4"
)

const (
	whipHandshakeTimeout = 10 * time.Second
	whipHTTPTimeout      = 10 * time.Second
	restartPause         = 3 * time.Second
)

// forwardTarget is what a single forwarderInstance actually POSTs the WHIP
// offer to, resolved once per (re)connect attempt (Tencent's is
// time-signed, see buildTencentWHIPURL; a mmx target's is static, see
// buildMmxTarget) - this is the one piece that differs between "forward to
// Tencent" and "forward to another mmx node", everything else in
// forwarderInstance.runInner (offer/answer, ICE, reader attach) is shared.
type forwardTarget struct {
	whipURL     string
	bearerToken string
	// logLabel identifies the destination in log lines, e.g.
	// "tencent:mystream_high" or "mmx:http://host:8889/live/x/whip".
	logLabel string
}

// Forwarder pushes a path's stream to another WHIP endpoint - either one
// video layer to Tencent Cloud (see SplitLayers, which is why Tencent needs
// one Forwarder per layer) or the whole multi-layer stream to another mmx
// node in a single session (mmx natively accepts a Simulcast WHIP offer, no
// splitting needed) - restarting automatically on failure. It mirrors
// internal/recorder's Recorder/recorderInstance split: Forwarder is the
// long-lived supervisor, forwarderInstance is a single connect-and-push
// attempt. mmx acts as a WHIP *client* here — the mirror image of the WHIP
// server role it already has for OBS ingest.
//
// Exactly one of Tencent/StreamKey or Mmx should be set by the caller (see
// internal/core/path.go's startTencentForwarding vs startMmxForwarding);
// target() resolves whichever is configured into the shared forwardTarget
// shape.
type Forwarder struct {
	Tencent   TencentConfig
	StreamKey string // fully resolved Tencent stream name for this layer
	Mmx       MmxConfig
	// PathName is the local path being forwarded, e.g. "live/table-view".
	// Only used to fill Mmx.URL's "{path}" placeholder (see
	// mmxPathPlaceholder) - Tencent's StreamKey already carries the
	// per-layer name it needs.
	PathName string
	Desc     *description.Session // Tencent: one video Media + shared audio; mmx: the whole session
	Stream   *stream.Stream
	Parent   logger.Writer

	currentInstance *forwarderInstance
	terminate       chan struct{}
	done            chan struct{}
}

// target resolves this Forwarder's configuration into the (URL, token, log
// label) a forwarderInstance needs, regardless of which destination type is
// configured.
func (f *Forwarder) target() forwardTarget {
	if f.Mmx.Enable {
		whipURL, bearerToken := buildMmxTarget(f.Mmx, f.PathName)
		return forwardTarget{whipURL: whipURL, bearerToken: bearerToken, logLabel: "mmx:" + whipURL}
	}
	bearerToken := buildTencentWHIPURL(f.Tencent, f.StreamKey)
	return forwardTarget{
		whipURL:     tencentWHIPEndpoint,
		bearerToken: bearerToken,
		logLabel:    "tencent:" + f.StreamKey,
	}
}

// Initialize starts the forwarder.
func (f *Forwarder) Initialize() {
	f.terminate = make(chan struct{})
	f.done = make(chan struct{})

	f.currentInstance = f.newInstance()
	f.currentInstance.initialize()

	go f.run()
}

func (f *Forwarder) newInstance() *forwarderInstance {
	return &forwarderInstance{
		target: f.target(),
		desc:   f.Desc,
		stream: f.Stream,
		parent: f,
	}
}

// Log implements logger.Writer.
func (f *Forwarder) Log(level logger.Level, format string, args ...any) {
	f.Parent.Log(level, "[forward %s] "+format, append([]any{f.target().logLabel}, args...)...)
}

// Close stops the forwarder.
func (f *Forwarder) Close() {
	close(f.terminate)
	<-f.done
}

func (f *Forwarder) run() {
	defer close(f.done)

	for {
		select {
		case <-f.currentInstance.done:
			f.currentInstance.close()
		case <-f.terminate:
			f.currentInstance.close()
			return
		}

		select {
		case <-time.After(restartPause):
		case <-f.terminate:
			return
		}

		f.currentInstance = f.newInstance()
		f.currentInstance.initialize()
	}
}

// forwarderInstance is a single WHIP publish attempt: build an offer from
// the path's stream, POST it to target, attach the connection as a stream
// reader once answered, and run until the connection drops or fails.
type forwarderInstance struct {
	target forwardTarget
	desc   *description.Session
	stream *stream.Stream
	parent logger.Writer

	ctx       context.Context
	ctxCancel context.CancelFunc
	done      chan struct{}
}

// Log implements logger.Writer.
func (fi *forwarderInstance) Log(level logger.Level, format string, args ...any) {
	fi.parent.Log(level, format, args...)
}

func (fi *forwarderInstance) initialize() {
	fi.ctx, fi.ctxCancel = context.WithCancel(context.Background())
	fi.done = make(chan struct{})

	go fi.run()
}

func (fi *forwarderInstance) close() {
	fi.ctxCancel()
	<-fi.done
}

func (fi *forwarderInstance) run() {
	defer close(fi.done)

	err := fi.runInner()
	if err != nil {
		fi.Log(logger.Warn, "%v", err)
	}
}

// setupOutboundTracks builds the offer's outbound tracks from desc. A
// Tencent target's desc is already split to one video media (SplitLayers),
// so webrtc.FromStream's "first matching format" behavior is fine there. A
// mmx target's desc is the path's whole multi-layer session - FromStream
// would silently keep only the first video media, dropping every other
// Simulcast layer, so this counts H264 video medias first and switches to
// webrtc.SetupFromStreamSimulcast (one RID encoding per layer, all sharing
// a single video m-line) whenever there's more than one: the target mmx
// node's own WHIP publish endpoint, like this node's, only ever accepts a
// single video m-line (see TracksAreValid).
func setupOutboundTracks(desc *description.Session, r *stream.Reader, pc *webrtc.PeerConnection) error {
	h264VideoMedias := 0
	for _, media := range desc.Medias {
		if media.Type != description.MediaTypeVideo {
			continue
		}
		for _, forma := range media.Formats {
			if _, ok := forma.(*format.H264); ok {
				h264VideoMedias++
				break
			}
		}
	}

	if h264VideoMedias > 1 {
		return webrtc.SetupFromStreamSimulcast(desc, r, pc)
	}

	return webrtc.FromStream(desc, r, pc)
}

func (fi *forwarderInstance) runInner() error {
	reader := &stream.Reader{Parent: fi}

	pc := &webrtc.PeerConnection{
		// dial-out only: no fixed ICE port to mux, so gather a real UDP
		// candidate (default network types are TCP-only otherwise, see
		// PeerConnection.Start).
		LocalRandomUDP: true,
		Publish:        true,
		Log:            fi,
	}

	err := setupOutboundTracks(fi.desc, reader, pc)
	if err != nil {
		return fmt.Errorf("setting up outbound tracks: %w", err)
	}

	err = pc.Start()
	if err != nil {
		return fmt.Errorf("starting peer connection: %w", err)
	}
	defer pc.Close()

	offer, err := pc.CreateFullOffer()
	if err != nil {
		return fmt.Errorf("creating offer: %w", err)
	}

	fi.Log(logger.Info, "connecting to %s", fi.target.whipURL)

	answerSDP, resourceURL, err := postWebrtc(fi.ctx, fi.target.whipURL, fi.target.bearerToken, offer.SDP)
	if err != nil {
		return fmt.Errorf("WHIP POST to %s failed: %w", fi.target.logLabel, err)
	}
	defer deleteWHIP(resourceURL) //nolint:errcheck

	err = pc.SetAnswer(&pwebrtc.SessionDescription{
		Type: pwebrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	})
	if err != nil {
		return fmt.Errorf("setting answer: %w", err)
	}

	err = pc.WaitUntilConnected(whipHandshakeTimeout)
	if err != nil {
		return fmt.Errorf("waiting for connection: %w", err)
	}

	fi.Log(logger.Info, "forwarding as %s", fi.target.logLabel)

	fi.stream.AddReader(reader)
	defer fi.stream.RemoveReader(reader)

	select {
	case <-pc.Failed():
		return fmt.Errorf("peer connection closed")

	case err := <-reader.Error():
		return err

	case <-fi.ctx.Done():
		return nil
	}
}

// postWebrtc POSTs a SDP offer to a WHIP endpoint and returns the SDP answer
// plus the resource URL (from the Location header, resolved against the
// request URL since WHIP allows it to be relative) used later to DELETE the
// session on shutdown.
func postWebrtc(ctx context.Context, whipURL string, bearerToken string, offerSDP string) (answerSDP string, resourceURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, whipURL, bytes.NewReader([]byte(offerSDP)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	client := &http.Client{Timeout: whipHTTPTimeout}
	res, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", "", err
	}

	if res.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("bad status code %d: %s", res.StatusCode, string(body))
	}

	location := res.Header.Get("Location")
	if location == "" {
		return "", "", fmt.Errorf("missing Location header in WHIP response")
	}

	resourceURL, err = resolveWHIPResourceURL(whipURL, location)
	if err != nil {
		return "", "", fmt.Errorf("invalid Location header %q: %w", location, err)
	}

	return string(body), resourceURL, nil
}

func resolveWHIPResourceURL(requestURL, location string) (string, error) {
	base, err := url.Parse(requestURL)
	if err != nil {
		return "", err
	}

	ref, err := url.Parse(location)
	if err != nil {
		return "", err
	}

	return base.ResolveReference(ref).String(), nil
}

// deleteWHIP tells Tencent the push session has ended cleanly.
func deleteWHIP(resourceURL string) error {
	req, err := http.NewRequest(http.MethodDelete, resourceURL, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: whipHTTPTimeout}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body) //nolint:errcheck

	return nil
}
