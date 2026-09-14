package degrade

import (
	"sync"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// Manager is the per-path registry of degrade State, shared by every
// ingest protocol that feeds it (and by whichever protocol server serves
// the WS delivery channel). Plain mutex-guarded map, independent of any
// session-manager actor loop: degrade state outlives individual publish
// sessions/connections and doesn't need to interact with their create/
// close events.
type Manager struct {
	log logger.Writer

	mu     sync.Mutex
	states map[string]*State
}

func NewManager(log logger.Writer) *Manager {
	return &Manager{
		log:    log,
		states: make(map[string]*State),
	}
}

// GetOrCreate returns the degrade State for path, creating it on first use.
func (m *Manager) GetOrCreate(path string) *State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.states[path]; ok {
		return st
	}
	st := newState(path, m.log)
	m.states[path] = st
	return st
}

// RecordSample feeds one loss sample into path's degrade State, using
// thresholds supplied by the calling protocol (see Thresholds' doc
// comment).
func (m *Manager) RecordSample(path string, cumLost, cumReceived uint64, t Thresholds) {
	m.GetOrCreate(path).Sample(cumLost, cumReceived, t)
}

// ObserveSessionLayers reports a fresh publish session/connection's real
// negotiated layer count for path.
func (m *Manager) ObserveSessionLayers(path string, realLayers int) {
	m.GetOrCreate(path).ObserveSessionLayerCount(realLayers)
}
