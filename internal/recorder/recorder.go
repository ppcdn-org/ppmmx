// Package recorder contains the recorder.
package recorder

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
)

const (
	ntpDriftTolerance = 5 * time.Second
)

// OnSegmentCreateFunc is the prototype of the function passed as OnSegmentCreate
type OnSegmentCreateFunc = func(path string)

// OnSegmentCompleteFunc is the prototype of the function passed as OnSegmentComplete
type OnSegmentCompleteFunc = func(path string, duration time.Duration)

// Recorder writes recordings to disk.
type Recorder struct {
	PathFormat        string
	Format            conf.RecordFormat
	PartDuration      time.Duration
	MaxPartSize       conf.StringSize
	SegmentDuration   time.Duration
	PathName          string
	Stream            *stream.Stream
	OnSegmentCreate   OnSegmentCreateFunc
	OnSegmentComplete OnSegmentCompleteFunc
	Parent            logger.Writer

	restartPause time.Duration

	currentInstance *recorderInstance
	// lastSegmentPath is the on-disk path of the most recently completed
	// segment. Only ever written from inside run() (via
	// trackAndForwardSegmentComplete, which fires synchronously within
	// currentInstance.close()) and read from run() right after, so no lock
	// is needed.
	lastSegmentPath string

	terminate chan struct{}
	done      chan struct{}
	split     chan splitRequest
}

type splitRequest struct {
	// renameTo, if non-empty, renames the segment that is being closed by
	// this split to <renameTo><ext> once it has finished writing. Empty
	// just cuts a new segment without renaming the one that's ending.
	renameTo string
	res      chan splitResult
}

type splitResult struct {
	path string
	err  error
}

var validSegmentName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Initialize initializes Recorder.
func (r *Recorder) Initialize() {
	if r.OnSegmentCreate == nil {
		r.OnSegmentCreate = func(string) {
		}
	}
	if r.OnSegmentComplete == nil {
		r.OnSegmentComplete = func(string, time.Duration) {
		}
	}
	if r.restartPause == 0 {
		r.restartPause = 2 * time.Second
	}

	r.terminate = make(chan struct{})
	r.done = make(chan struct{})
	r.split = make(chan splitRequest)

	r.currentInstance = &recorderInstance{
		pathFormat:        r.PathFormat,
		format:            r.Format,
		partDuration:      r.PartDuration,
		maxPartSize:       r.MaxPartSize,
		segmentDuration:   r.SegmentDuration,
		pathName:          r.PathName,
		stream:            r.Stream,
		onSegmentCreate:   r.OnSegmentCreate,
		onSegmentComplete: r.trackAndForwardSegmentComplete,
		parent:            r,
	}
	r.currentInstance.initialize()

	go r.run()
}

// trackAndForwardSegmentComplete records the path of the segment that just
// finished (so a split request can rename it) before forwarding the event
// to the caller-supplied OnSegmentComplete.
func (r *Recorder) trackAndForwardSegmentComplete(segmentPath string, duration time.Duration) {
	r.lastSegmentPath = segmentPath
	r.OnSegmentComplete(segmentPath, duration)
}

// Log implements logger.Writer.
func (r *Recorder) Log(level logger.Level, format string, args ...any) {
	r.Parent.Log(level, "[recorder] "+format, args...)
}

// Close closes the agent.
func (r *Recorder) Close() {
	r.Log(logger.Info, "recording stopped")
	close(r.terminate)
	<-r.done
}

// Status returns the current recording status.
type Status struct {
	Running  bool
	FilePath string
}

func (r *Recorder) Status() Status {
	if r.currentInstance == nil {
		return Status{}
	}
	return Status{
		Running:  true,
		FilePath: r.PathFormat,
	}
}

// SplitSegment closes the current recording instance and starts a new one.
// Used by /api/split-rec: an empty renameTo just cuts a fresh segment
// (round start); a non-empty renameTo additionally renames the segment
// that was just closed to "<renameTo><ext>" (round end). It returns the
// renamed file's path, or "" when renameTo is empty.
func (r *Recorder) SplitSegment(renameTo string) (string, error) {
	if renameTo != "" && !validSegmentName.MatchString(renameTo) {
		return "", fmt.Errorf("invalid recording file name")
	}
	res := make(chan splitResult, 1)
	select {
	case r.split <- splitRequest{renameTo: renameTo, res: res}:
		result := <-res
		return result.path, result.err
	case <-r.done:
		return "", fmt.Errorf("recorder is closed")
	}
}

func (r *Recorder) run() {
	defer close(r.done)

	for {
		select {
		case <-r.currentInstance.done:
			r.currentInstance.close()
		case req := <-r.split:
			before := r.lastSegmentPath
			r.currentInstance.close()
			var result splitResult
			if req.renameTo != "" {
				result.path, result.err = r.renameLastSegment(before, req.renameTo)
			}
			req.res <- result
		case <-r.terminate:
			r.currentInstance.close()
			return
		}

		select {
		case <-time.After(r.restartPause):
		case <-r.terminate:
			return
		}

		r.currentInstance = &recorderInstance{
			pathFormat:        r.PathFormat,
			format:            r.Format,
			partDuration:      r.PartDuration,
			maxPartSize:       r.MaxPartSize,
			segmentDuration:   r.SegmentDuration,
			pathName:          r.PathName,
			stream:            r.Stream,
			onSegmentCreate:   r.OnSegmentCreate,
			onSegmentComplete: r.trackAndForwardSegmentComplete,
			parent:            r,
		}
		r.currentInstance.initialize()
	}
}

// renameLastSegment renames the segment most recently completed by the
// close() that preceded this call to newBaseName, preserving its
// extension. before is r.lastSegmentPath as it was prior to that close():
// if it didn't change, the closing instance produced no segment (e.g. no
// samples were written since the previous split) and there is nothing to
// rename.
func (r *Recorder) renameLastSegment(before, newBaseName string) (string, error) {
	after := r.lastSegmentPath
	if after == "" || after == before {
		return "", fmt.Errorf("no data recorded since the last split, nothing to finalize")
	}
	target := filepath.Join(filepath.Dir(after), newBaseName+filepath.Ext(after))
	if err := os.Rename(after, target); err != nil {
		return "", fmt.Errorf("rename segment: %w", err)
	}
	r.lastSegmentPath = target
	return target, nil
}
