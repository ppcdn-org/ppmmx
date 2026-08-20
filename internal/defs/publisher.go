package defs

// Publisher is an entity that can publish a stream.
type Publisher interface {
	Source
	Close()
	// RequestKeyFrame requests the publisher to generate a keyframe immediately.
	// Used to reduce viewer startup latency.
	RequestKeyFrame() error
}
