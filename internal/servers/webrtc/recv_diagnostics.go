package webrtc

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bluenviron/gortsplib/v5/pkg/rtpreceiver"

	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
)

// nackDeltaSampler turns the PeerConnection's cumulative NACK counters and
// per-track receive counters into the per-interval detail appended to the
// "[recv-stats]" line (see recvstats.Snapshot.Extra).
//
// Why this exists: the aggregate loss figure sums every Simulcast layer and
// counts a packet as "lost" whether or not NACK retransmission recovered it
// in time. Neither tells you what an operator actually needs to know when a
// downstream node reports "invalid FU-A packet (non-starting)" - namely
// whether the hop is losing packets at all, whether NACK is being used to
// repair them, and whether it's one layer misbehaving or all of them. Those
// three signals are exactly what distinguishes "the link is dropping
// packets" from "the sender is emitting bad fragments".
type nackDeltaSampler struct {
	lastRequested uint64
	lastReceived  uint64
	lastPerTrack  map[string]trackCounters
	seeded        bool
}

type trackCounters struct {
	received uint64
	lost     uint64
}

func (n *nackDeltaSampler) seed(st *webrtc.Stats) {
	n.lastRequested = st.NACKPacketsRequested
	n.lastReceived = st.NACKPacketsReceived
	n.seeded = true
}

// extra renders this interval's NACK deltas plus a per-layer loss breakdown.
// Counter resets (a reconnect rebuilding the PeerConnection) are treated as
// a fresh baseline rather than producing a bogus negative delta, matching
// recvstats.Sampler's behaviour.
func (n *nackDeltaSampler) extra(st *webrtc.Stats, perTrack map[string]*rtpreceiver.Stats) string {
	var sb strings.Builder

	if n.seeded && st.NACKPacketsRequested >= n.lastRequested && st.NACKPacketsReceived >= n.lastReceived {
		fmt.Fprintf(&sb, " nack=%d/%d",
			st.NACKPacketsRequested-n.lastRequested,
			st.NACKPacketsReceived-n.lastReceived)
	}
	n.lastRequested = st.NACKPacketsRequested
	n.lastReceived = st.NACKPacketsReceived
	n.seeded = true

	if detail := n.perTrackDetail(perTrack); detail != "" {
		sb.WriteString(" layers[" + detail + "]")
	}

	return sb.String()
}

func (n *nackDeltaSampler) perTrackDetail(perTrack map[string]*rtpreceiver.Stats) string {
	if len(perTrack) == 0 {
		return ""
	}

	labels := make([]string, 0, len(perTrack))
	for label := range perTrack {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	current := make(map[string]trackCounters, len(perTrack))
	parts := make([]string, 0, len(labels))

	for _, label := range labels {
		st := perTrack[label]
		current[label] = trackCounters{received: st.Received, lost: st.Lost}

		prev, ok := n.lastPerTrack[label]
		if !ok || st.Received < prev.received || st.Lost < prev.lost {
			// first sighting or counter reset: no delta to report yet
			continue
		}

		dRecv := st.Received - prev.received
		dLost := st.Lost - prev.lost
		loss := float64(0)
		if denom := dRecv + dLost; denom > 0 {
			loss = float64(dLost) / float64(denom) * 100
		}

		parts = append(parts, fmt.Sprintf("%s:recv=%d lost=%d (%.2f%%)", label, dRecv, dLost, loss))
	}

	n.lastPerTrack = current

	return strings.Join(parts, " ")
}
