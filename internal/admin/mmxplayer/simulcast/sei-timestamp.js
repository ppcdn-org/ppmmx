'use strict';

// Reads the OBS abs-timestamp SEI (see docs/obs-abs-timestamp-protocol.md
// in the OBS repo) directly out of the received H.264 bitstream via
// WebCodecs Insertable Streams (RTCRtpReceiver.createEncodedStreams()).
//
// Unlike the "obs-timestamp" DataChannel relay (see mmxplayer.js /
// obs_timestamp_broadcast.go), the SEI travels inside the encoded frame
// itself, so it survives mmx-to-mmx cascading (origin -> edge -> ...)
// without any server-side relay code: mmx's WHIP/WHEP path is pure RTP
// passthrough (UseRTPPackets: true end to end), and the ABR track
// selector's remuxer (unitRemuxerH264) only strips SPS/PPS/AUD, never SEI.
// It's also inherently correct for whichever simulcast layer is currently
// being decoded - no rid matching needed, since these are the frames the
// browser is actually about to decode.
//
// Chromium-only (createEncodedStreams requires the PeerConnection to be
// constructed with { encodedInsertableStreams: true }, see mmxplayer.js);
// on browsers without it, attachSeiTimestampReader() is a no-op returning
// false, and the caller should keep relying on the DataChannel relay.

const OBS_ABS_TS_SEI_UUID = new Uint8Array([
    0x4F, 0x42, 0x53, 0x2D, 0x41, 0x42, 0x53, 0x54, // "OBS-ABST"
    0x53, 0x2D, 0x53, 0x45, 0x49, 0x2D, 0x76, 0x31, // "S-SEI-v1"
]);

function bytesEqualAt(data, offset, needle) {
    if (offset + needle.length > data.length) return false;
    for (let i = 0; i < needle.length; i++) {
        if (data[offset + i] !== needle[i]) return false;
    }
    return true;
}

// Removes H.264 emulation-prevention bytes (00 00 03 -> 00 00) from a NAL's
// raw bytes, needed before interpreting SEI payload type/size/content:
// escaping is part of RBSP encoding itself, independent of Annex-B framing.
function unescapeRBSP(nal) {
    const out = new Uint8Array(nal.length);
    let o = 0;
    let zeroRun = 0;
    for (let i = 0; i < nal.length; i++) {
        const b = nal[i];
        if (zeroRun >= 2 && b === 0x03 && i + 1 < nal.length && nal[i + 1] <= 0x03) {
            zeroRun = 0;
            continue; // drop emulation_prevention_three_byte
        }
        out[o++] = b;
        zeroRun = (b === 0) ? zeroRun + 1 : 0;
    }
    return out.subarray(0, o);
}

// Finds Annex-B start codes (00 00 01 / 00 00 00 01) in `data` and returns
// the byte range of each NAL (header + RBSP, excluding the start code).
function* iterateAnnexBNALs(data) {
    const len = data.length;
    let i = 0;
    let nalStart = -1;
    while (i + 2 < len) {
        if (data[i] === 0 && data[i + 1] === 0 && data[i + 2] === 1) {
            if (nalStart >= 0) yield data.subarray(nalStart, i);
            nalStart = i + 3;
            i += 3;
            continue;
        }
        i++;
    }
    if (nalStart >= 0 && nalStart < len) yield data.subarray(nalStart, len);
}

// Scans one encoded H.264 access unit for a user_data_unregistered SEI NAL
// (payload type 5) carrying the OBS abs-timestamp UUID, returning the
// embedded timestamp_ms (see protocol doc section 2), or null if absent.
function findObsAbsTimestamp(data) {
    for (const rawNal of iterateAnnexBNALs(data)) {
        if (rawNal.length < 2) continue;
        // strip trailing zero-padding some encoders leave before the next start code
        let end = rawNal.length;
        while (end > 0 && rawNal[end - 1] === 0) end--;
        if (end < 2) continue;

        const nalType = rawNal[0] & 0x1F;
        if (nalType !== 6) continue; // SEI

        const nal = unescapeRBSP(rawNal.subarray(0, end));
        let p = 1; // skip NAL header byte

        let payloadType = 0;
        while (p < nal.length && nal[p] === 0xFF) { payloadType += 255; p++; }
        if (p >= nal.length) continue;
        payloadType += nal[p]; p++;

        let payloadSize = 0;
        while (p < nal.length && nal[p] === 0xFF) { payloadSize += 255; p++; }
        if (p >= nal.length) continue;
        payloadSize += nal[p]; p++;

        if (payloadType !== 5 || payloadSize < 24 || p + 24 > nal.length) continue;
        if (!bytesEqualAt(nal, p, OBS_ABS_TS_SEI_UUID)) continue;

        let ts = 0;
        for (let k = 0; k < 8; k++) {
            ts = ts * 256 + nal[p + 16 + k];
        }
        return ts;
    }
    return null;
}

// Attaches an Insertable Streams passthrough transform to a video
// RTCRtpReceiver: every encoded frame is scanned for the OBS abs-timestamp
// SEI and, if found, reported via onTimestamp(timestampMs); frames are
// always forwarded unmodified. Returns false (no-op) if this browser
// doesn't support createEncodedStreams.
function attachSeiTimestampReader(receiver, onTimestamp) {
    if (!receiver || typeof receiver.createEncodedStreams !== 'function') return false;

    let streams;
    try {
        streams = receiver.createEncodedStreams();
    } catch (e) {
        console.warn('[SEI] createEncodedStreams failed:', e && e.message);
        return false;
    }

    const transform = new TransformStream({
        transform(frame, controller) {
            try {
                const ts = findObsAbsTimestamp(new Uint8Array(frame.data));
                if (ts !== null) onTimestamp(ts);
            } catch (e) {
                // A parse error must never break playback - just skip this frame.
            }
            controller.enqueue(frame);
        }
    });

    streams.readable.pipeThrough(transform).pipeTo(streams.writable).catch((e) => {
        console.warn('[SEI] insertable-streams pipeline error:', e && e.message);
    });

    return true;
}

window.attachSeiTimestampReader = attachSeiTimestampReader;
