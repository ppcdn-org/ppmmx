package ingest

import (
	"context"
	"crypto/md5" //nolint:gosec // required by Tencent's anti-leech scheme, not used for security
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
)

const (
	minRetryInterval = 1 * time.Second
	maxRetryInterval = 60 * time.Second
	// runs shorter than this are treated as a failed startup (back off
	// further); longer runs reset the backoff back to minRetryInterval.
	healthyRunThreshold = 30 * time.Second

	cdnNameTencent = "tencent"

	// tencentSignatureTTL is how far in the future txTime is set. A fresh
	// signature is computed right before every connect attempt (see
	// buildPullURL), so this only needs to comfortably outlast a single
	// ffmpeg TCP+RTMP handshake - Tencent's anti-leech check runs once at
	// connect time, not continuously over the life of the stream.
	tencentSignatureTTL = 5 * time.Minute
)

// worker pulls a single CDN source with ffmpeg and republishes it to a local
// RTMP target, restarting with backoff whenever ffmpeg exits.
type worker struct {
	cdnName              string
	rawSource            string // unsigned; never logged with a signature attached
	targetURL            string
	tencentSecretKeyBack string
	parent               logger.Writer

	ctx    context.Context
	cancel func()
}

func newWorker(cdnName, rawSource, targetURL, tencentSecretKeyBack string, parent logger.Writer) *worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &worker{
		cdnName:              cdnName,
		rawSource:            rawSource,
		targetURL:            targetURL,
		tencentSecretKeyBack: tencentSecretKeyBack,
		parent:               parent,
		ctx:                  ctx,
		cancel:               cancel,
	}
}

func (w *worker) log(level logger.Level, format string, args ...any) {
	if w.parent != nil {
		w.parent.Log(level, format, args...)
	}
}

// buildPullURL returns the URL ffmpeg should dial for this attempt. For
// cdnName "tencent" it appends a freshly computed txTime/txSecret pair,
// signed the same way as internal/admin's signedPlayParams (and
// internal/forward/tencent.go's push-side equivalent): txTime is the expiry
// as a hex unix timestamp, txSecret is md5(secretKeyBack + streamName +
// txTime). Any other cdnName is returned unchanged - the caller is expected
// to have embedded whatever auth that CDN needs directly in the URL.
func (w *worker) buildPullURL() string {
	if w.cdnName != cdnNameTencent {
		return w.rawSource
	}

	streamName := lastPathSegment(w.rawSource)
	txTime := fmt.Sprintf("%X", time.Now().Add(tencentSignatureTTL).Unix())
	sum := md5.Sum([]byte(w.tencentSecretKeyBack + streamName + txTime))
	txSecret := fmt.Sprintf("%x", sum)

	sep := "?"
	if strings.Contains(w.rawSource, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%stxTime=%s&txSecret=%s", w.rawSource, sep, txTime, txSecret)
}

func (w *worker) run() {
	w.log(logger.Info, "ingest: %s:%s -> %s", w.cdnName, w.rawSource, w.targetURL)

	retry := minRetryInterval

	for {
		start := time.Now()

		// ffmpeg does the actual pull-and-republish: read the CDN source,
		// copy (no re-encode) into an FLV/RTMP push to our own local
		// listener, so mmx's normal publish path picks it up like any other
		// publisher would. The pull URL is (re)signed fresh for every
		// attempt, not just the first one.
		cmd := exec.CommandContext(w.ctx, "ffmpeg",
			"-loglevel", "fatal",
			"-i", w.buildPullURL(),
			"-c", "copy",
			"-f", "flv",
			w.targetURL,
		)
		err := cmd.Run()

		select {
		case <-w.ctx.Done():
			return
		default:
		}

		elapsed := time.Since(start)
		if err != nil {
			w.log(logger.Warn, "ingest: ffmpeg exited for %s: %v (ran %s)", w.rawSource, err, elapsed.Round(time.Second))
		} else {
			w.log(logger.Debug, "ingest: ffmpeg exited normally for %s (ran %s)", w.rawSource, elapsed.Round(time.Second))
		}

		if elapsed >= healthyRunThreshold {
			retry = minRetryInterval
		} else if retry < maxRetryInterval {
			retry += 2 * time.Second
			if retry > maxRetryInterval {
				retry = maxRetryInterval
			}
		}

		select {
		case <-time.After(retry):
		case <-w.ctx.Done():
			return
		}
	}
}
