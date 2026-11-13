package main

// Checks the MPEG-TS coming off the tuner as it's passed on: every 188-byte packet carries a
// 4-bit counter per stream (PID), so a gap in it is a lost packet, and the tuner marks packets it
// couldn't correct (transport error indicator). Weak reception shows up here before anyone
// hears it: the audio skips while the picture holds up.

const (
	tsPacket = 188
	tsSync   = 0x47
	nullPID  = 0x1FFF
)

type tsStats struct {
	packets    int64
	lost       int64 // continuity-counter gaps
	corrupt    int64 // packets the tuner flagged as uncorrectable
	syncLosses int64 // times the 0x47 sync byte went missing and had to be found again
	lostByPID  map[uint16]int64

	counter   map[uint16]byte // last continuity counter seen, per PID
	partial   []byte          // the start of a packet split across reads
	outOfStep bool            // lost sync and still looking for it
}

func newTSStats() *tsStats {
	return &tsStats{lostByPID: map[uint16]int64{}, counter: map[uint16]byte{}}
}

// add checks one chunk of the stream, carrying a packet split across chunks over to the next.
func (s *tsStats) add(chunk []byte) {
	data := chunk
	if len(s.partial) > 0 {
		data = append(s.partial, chunk...)
		s.partial = nil
	}
	i := 0
	for i+tsPacket <= len(data) {
		if data[i] != tsSync {
			// Out of step: find the next sync byte that's followed by another one a packet later.
			if !s.outOfStep {
				s.outOfStep = true
				s.syncLosses++
			}
			j := i + 1
			for j+tsPacket < len(data) && !(data[j] == tsSync && data[j+tsPacket] == tsSync) {
				j++
			}
			i = j
			if j+tsPacket >= len(data) {
				break // not in this chunk: carry the rest over and keep looking
			}
			continue
		}
		s.outOfStep = false
		s.packet(data[i : i+tsPacket])
		i += tsPacket
	}
	if i < len(data) {
		s.partial = append([]byte(nil), data[i:]...)
	}
}

func (s *tsStats) packet(p []byte) {
	s.packets++
	pid := uint16(p[1]&0x1F)<<8 | uint16(p[2])
	if pid == nullPID {
		return
	}
	if p[1]&0x80 != 0 {
		s.corrupt++
	}
	hasPayload := p[3]&0x10 != 0
	discontinuity := p[3]&0x20 != 0 && p[4] > 0 && p[5]&0x80 != 0 // adaptation field says the gap is on purpose
	cc := p[3] & 0x0F
	if !hasPayload {
		return // the counter only moves with a payload
	}
	if last, seen := s.counter[pid]; seen && !discontinuity && cc != (last+1)&0x0F && cc != last {
		missing := int64((cc - last - 1) & 0x0F)
		if missing == 0 {
			missing = 1
		}
		s.lost += missing
		s.lostByPID[pid] += missing
	}
	s.counter[pid] = cc
}

// tsCounts are the totals at one moment; the difference between two says what happened between.
type tsCounts struct{ packets, lost, corrupt, syncLosses int64 }

func (s *tsStats) counts() tsCounts {
	return tsCounts{s.packets, s.lost, s.corrupt, s.syncLosses}
}

func (c tsCounts) minus(o tsCounts) tsCounts {
	return tsCounts{c.packets - o.packets, c.lost - o.lost, c.corrupt - o.corrupt, c.syncLosses - o.syncLosses}
}

func (c tsCounts) troubled() bool { return c.lost > 0 || c.corrupt > 0 || c.syncLosses > 0 }
