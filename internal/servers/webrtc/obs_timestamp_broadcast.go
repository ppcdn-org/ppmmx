package webrtc

import "sync"

// obsTimestampSubs fans out "obs-timestamp" DataChannel messages received
// on a path's WHIP publish session (see session.onInboundDataChannel) to
// every WHEP reader session currently reading that path, over each
// reader's ABR control WebSocket (see abr_ws_handler.go). This lets a
// player compute true end-to-end p2p delay: local render time minus the
// timestamp the publisher embedded when it sent the frame (see
// docs/obs-abs-timestamp-protocol.md in the OBS repo).
//
// Keyed by path name, not tied to any single publish/reader session pair,
// for the same reason degradeState and publishReconnectStats are: readers
// and the publisher connect/reconnect independently of each other.
type obsTimestampSubs struct {
	mu   sync.Mutex
	subs map[string]map[*session]struct{}
}

func newObsTimestampSubs() *obsTimestampSubs {
	return &obsTimestampSubs{subs: make(map[string]map[*session]struct{})}
}

// obsTimestampSubs lazily initializes the fan-out registry. Lazy rather
// than set up in Initialize() so a bare &Server{} built directly by tests
// (bypassing Initialize, see obs_timestamp_test.go) doesn't nil-panic the
// first time a WHIP session's onInboundDataChannel handler fires.
func (s *Server) obsTimestampSubsRegistry() *obsTimestampSubs {
	s.obsTimestampSubsOnce.Do(func() {
		s.obsTimestampSubsVal = newObsTimestampSubs()
	})
	return s.obsTimestampSubsVal
}

// subscribeObsTimestamp implements the sessionParent hook used by a WHEP
// reader session once it starts reading a path.
func (s *Server) subscribeObsTimestamp(pathName string, sx *session) {
	o := s.obsTimestampSubsRegistry()
	o.mu.Lock()
	defer o.mu.Unlock()
	set, ok := o.subs[pathName]
	if !ok {
		set = make(map[*session]struct{})
		o.subs[pathName] = set
	}
	set[sx] = struct{}{}
}

// unsubscribeObsTimestamp implements the sessionParent hook used by a WHEP
// reader session when it stops reading a path (error, close, or normal
// teardown).
func (s *Server) unsubscribeObsTimestamp(pathName string, sx *session) {
	o := s.obsTimestampSubsRegistry()
	o.mu.Lock()
	defer o.mu.Unlock()
	set, ok := o.subs[pathName]
	if !ok {
		return
	}
	delete(set, sx)
	if len(set) == 0 {
		delete(o.subs, pathName)
	}
}

// broadcastObsTimestamp implements the sessionParent hook used by a WHIP
// publish session's onInboundDataChannel handler to relay each
// "obs-timestamp" message to every subscribed WHEP reader of the same
// path.
func (s *Server) broadcastObsTimestamp(pathName string, msg abrMessage) {
	o := s.obsTimestampSubsRegistry()
	o.mu.Lock()
	subs := make([]*session, 0, len(o.subs[pathName]))
	for sx := range o.subs[pathName] {
		subs = append(subs, sx)
	}
	o.mu.Unlock()

	for _, sx := range subs {
		sx.writeABRMessage(msg) //nolint:errcheck
	}
}
