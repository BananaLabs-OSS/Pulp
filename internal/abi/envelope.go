package abi

import "encoding/binary"

// StepEnvelope is the universal header passed to every pulp_step call.
//
// Layout (little-endian, fixed):
//
//	call_number  uint64  — how many times step has been called
//	wall_time    uint64  — unix nanoseconds when Go called step
//	payload_len  uint32  — length of the following payload bytes
//	payload      []byte  — trigger data, plugin-defined
type StepEnvelope struct {
	CallNumber uint64
	WallTime   uint64
	Payload    []byte
}

const HeaderSize = 8 + 8 + 4

func (e StepEnvelope) Encode() []byte {
	buf := make([]byte, HeaderSize+len(e.Payload))
	binary.LittleEndian.PutUint64(buf[0:8], e.CallNumber)
	binary.LittleEndian.PutUint64(buf[8:16], e.WallTime)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(e.Payload)))
	copy(buf[HeaderSize:], e.Payload)
	return buf
}
