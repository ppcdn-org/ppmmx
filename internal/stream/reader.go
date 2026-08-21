package stream

import (
	"fmt"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/ringbuffer"
	"github.com/bluenviron/mediamtx/internal/counterdumper"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/unit"
)

// OnDataFunc is the callback passed to OnData().
type OnDataFunc func(*unit.Unit) error

// OnDataFilterFunc decides whether a unit must enter the reader queue.
// It runs synchronously in the stream writer and must not block.
type OnDataFilterFunc func(*unit.Unit) bool

// Reader is a stream reader.
type Reader struct {
	SkipOutboundBytes bool
	Parent            logger.Writer
	// MediaFilter runs before queueing and can independently filter medias.
	MediaFilter func(*description.Media, *unit.Unit) bool

	onDatas                 map[*description.Media]map[format.Format]OnDataFunc
	onDataFilters           map[*description.Media]map[format.Format]OnDataFilterFunc
	queueSize               int
	buffer                  *ringbuffer.RingBuffer
	outboundFramesDiscarded *counterdumper.Dumper

	// out
	err chan error
}

// OnData registers a callback that is called when data from given format is available.
func (r *Reader) OnData(origMedia *description.Media, origFormat format.Format, cb OnDataFunc) {
	if r.onDatas == nil {
		r.onDatas = make(map[*description.Media]map[format.Format]OnDataFunc)
	}
	if r.onDatas[origMedia] == nil {
		r.onDatas[origMedia] = make(map[format.Format]OnDataFunc)
	}
	r.onDatas[origMedia][origFormat] = cb
	if r.MediaFilter != nil {
		if r.onDataFilters == nil {
			r.onDataFilters = make(map[*description.Media]map[format.Format]OnDataFilterFunc)
		}
		if r.onDataFilters[origMedia] == nil {
			r.onDataFilters[origMedia] = make(map[format.Format]OnDataFilterFunc)
		}
		r.onDataFilters[origMedia][origFormat] = func(u *unit.Unit) bool {
			return r.MediaFilter(origMedia, u)
		}
	}
}

// OnDataFiltered registers a callback with a filter that runs before queueing.
func (r *Reader) OnDataFiltered(
	origMedia *description.Media,
	origFormat format.Format,
	filter OnDataFilterFunc,
	cb OnDataFunc,
) {
	r.OnData(origMedia, origFormat, cb)
	mediaFilter := r.MediaFilter

	if r.onDataFilters == nil {
		r.onDataFilters = make(map[*description.Media]map[format.Format]OnDataFilterFunc)
	}
	if r.onDataFilters[origMedia] == nil {
		r.onDataFilters[origMedia] = make(map[format.Format]OnDataFilterFunc)
	}
	r.onDataFilters[origMedia][origFormat] = func(u *unit.Unit) bool {
		return (mediaFilter == nil || mediaFilter(origMedia, u)) && filter(u)
	}
}

// Formats returns all formats for which the reader has registered a OnData callback.
func (r *Reader) Formats() []format.Format {
	n := 0
	for _, formats := range r.onDatas {
		for range formats {
			n++
		}
	}

	if n == 0 {
		return nil
	}

	out := make([]format.Format, n)
	n = 0

	for _, formats := range r.onDatas {
		for forma := range formats {
			out[n] = forma
			n++
		}
	}

	return out
}

// OutboundFramesDiscarded returns the number of frames discarded because the reader is too slow.
func (r *Reader) OutboundFramesDiscarded() uint64 {
	return r.outboundFramesDiscarded.Get()
}

// error returns whenever there's an error.
// It can be called only after stream.AddReader().
func (r *Reader) Error() chan error {
	return r.err
}

func (r *Reader) start() {
	buffer, _ := ringbuffer.New(uint64(r.queueSize))
	r.buffer = buffer
	r.err = make(chan error)

	r.outboundFramesDiscarded = &counterdumper.Dumper{
		OnReport: func(val uint64) {
			r.Parent.Log(logger.Warn, "reader is too slow, discarding %d %s",
				val,
				func() string {
					if val == 1 {
						return "frame"
					}
					return "frames"
				}())
		},
	}
	r.outboundFramesDiscarded.Start()

	go r.run()
}

func (r *Reader) stop() {
	r.buffer.Close()
	r.outboundFramesDiscarded.Stop()
	<-r.err
}

func (r *Reader) run() {
	r.err <- r.runInner()
	close(r.err)
}

func (r *Reader) runInner() error {
	for {
		cb, ok := r.buffer.Pull()
		if !ok {
			return fmt.Errorf("terminated")
		}

		err := cb.(func() error)()
		if err != nil {
			return err
		}
	}
}

func (r *Reader) push(cb func() error) {
	ok := r.buffer.Push(cb)
	if !ok {
		r.outboundFramesDiscarded.Increase()
	}
}
