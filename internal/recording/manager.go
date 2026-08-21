package recording

import (
	"crypto/md5"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PathController is the interface the recording manager uses to control
// recordings on a specific stream path.
type PathController interface {
	// StartOnDemandRecording starts recording, returns the file path.
	StartOnDemandRecording(opts Options) (string, error)
	// StopOnDemandRecording stops an on-demand recording. Returns file size and duration.
	StopOnDemandRecording() (int64, int64, error)
	// SplitRecording closes the current segment and starts a new one. If
	// renameTo is non-empty, the segment that was just closed is renamed to
	// it and its path is returned.
	SplitRecording(renameTo string) (string, error)
	// IsOnline returns true if the stream has a publisher.
	IsOnline() bool
}

// Options carries recording settings from an API request.
type Options struct {
	Format          string
	SegmentDuration time.Duration
	MaxPartSize     int64
	DeleteAfter     time.Duration
	Table           string
	GC              string
	Game            string
}

// Manager controls on-demand recording lifecycle and stores records in SQLite.
type Manager struct {
	store   *Store
	mu      sync.RWMutex
	running map[string]*recordJob // key = path
	closed  bool
}

type recordJob struct {
	ID    string
	Path  string
	opts  Options
	start time.Time
	ctrl  PathController
}

// NewManager creates a new recording manager.
func NewManager(store *Store) *Manager {
	store.RecoverInterrupted() // best effort; Store operations will surface persistent failures.
	return &Manager{
		store:   store,
		running: make(map[string]*recordJob),
	}
}

// Start begins an on-demand recording on the given path.
func (m *Manager) Start(path string, opts Options, ctrl PathController) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, fmt.Errorf("recording manager is closed")
	}
	if _, ok := m.running[path]; ok {
		return nil, fmt.Errorf("path %q is already recording", path)
	}

	if !ctrl.IsOnline() {
		return nil, fmt.Errorf("stream %q is not online", path)
	}

	filePath, err := ctrl.StartOnDemandRecording(opts)
	if err != nil {
		return nil, fmt.Errorf("start recording: %w", err)
	}

	id := fmt.Sprintf("rec_%x", md5.Sum([]byte(uuid.NewString()+path)))[:16]
	startedAt := now()

	r := Record{
		ID:        id,
		Path:      path,
		Table:     opts.Table,
		GC:        opts.GC,
		Game:      opts.Game,
		Format:    opts.Format,
		Status:    "running",
		StartedAt: startedAt,
		FilePath:  filePath,
	}
	if err := m.store.Insert(r); err != nil {
		ctrl.StopOnDemandRecording() // rollback
		return nil, fmt.Errorf("store insert: %w", err)
	}

	m.running[path] = &recordJob{
		ID:    id,
		Path:  path,
		opts:  opts,
		start: startedAt,
		ctrl:  ctrl,
	}
	return &r, nil
}

// Stop stops an on-demand recording on the given path.
func (m *Manager) Stop(path string) (*Record, error) {
	return m.stop(path, "")
}

// StopIfCurrent stops a recording only when the path still points to the expected job.
func (m *Manager) StopIfCurrent(path, expectedID string) (*Record, error) {
	return m.stop(path, expectedID)
}

func (m *Manager) stop(path, expectedID string) (*Record, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("recording manager is closed")
	}
	job, ok := m.running[path]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("recording not found for path %q", path)
	}
	if expectedID != "" && job.ID != expectedID {
		m.mu.Unlock()
		return nil, fmt.Errorf("recording changed for path %q", path)
	}

	fileSize, duration, err := job.ctrl.StopOnDemandRecording()
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("stop recording: %w", err)
	}

	delete(m.running, path)
	m.mu.Unlock()

	r, err := m.store.Get(job.ID)
	if err != nil || r == nil {
		return nil, fmt.Errorf("record not found: %w", err)
	}

	r.Status = "completed"
	r.StoppedAt = now()
	r.Duration = duration
	r.FileSize = fileSize
	if err := m.store.Update(job.ID, "completed", fileSize, duration); err != nil {
		return r, fmt.Errorf("store update: %w", err)
	}

	return r, nil
}

// Close stops active recordings and closes the persistent store.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	jobs := make([]*recordJob, 0, len(m.running))
	for _, job := range m.running {
		jobs = append(jobs, job)
	}
	m.running = make(map[string]*recordJob)
	m.mu.Unlock()
	for _, job := range jobs {
		fileSize, duration, err := job.ctrl.StopOnDemandRecording()
		status := "completed"
		if err != nil {
			status = "error"
		}
		_ = m.store.Update(job.ID, status, fileSize, duration)
	}
	return m.store.Close()
}

// GetRunning returns the running recording for a path, if any.
func (m *Manager) GetRunning(path string) *Record {
	m.mu.RLock()
	defer m.mu.RUnlock()

	job, ok := m.running[path]
	if !ok {
		return nil
	}

	r, _ := m.store.Get(job.ID)
	return r
}

// ListRunning returns all currently running recordings.
func (m *Manager) ListRunning() []Record {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []Record
	for _, job := range m.running {
		r, err := m.store.Get(job.ID)
		if err == nil && r != nil {
			out = append(out, *r)
		}
	}
	return out
}

// Store returns the underlying Store for direct queries.
func (m *Manager) Store() *Store {
	return m.store
}

// HasRunning returns true if a recording is active on the given path.
func (m *Manager) HasRunning(path string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.running[path]
	return ok
}
