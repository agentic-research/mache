package control

// The control block's wire layout, shared with ley-line-open.

const (
	ControlSize = 4096       // 1 page
	Magic       = 0x4C455943 // 'LEYC'
	// Version 3 is the current control-block protocol (ley-line-open
	// v0.20.1+, ley-line-open-49a1ef): the layout is v2's, identity is the
	// BLAKE3 `current_root` of the arena payload, and the word at offset 8 is
	// a seqlock sequence, odd while a writer is changing the payload and even
	// when it is whole. v2 wrote the payload and then bumped the word once,
	// so a reader could not tell a publish in flight from a finished one.
	// Any other version is rejected with a descriptive error, so
	// cross-version mismatches fail loudly (mache-3451a1).
	Version = 3
)

// Field offsets must match LLO `rs/ll-core/core/src/control.rs` exactly.
// Bytes 8..16 (the old `Generation` slot) are the private seqlock sequence.
const (
	offMagic          = 0
	offVersion        = 4
	offSync           = 8 // seqlock sequence (formerly Generation)
	offArenaPath      = 16
	arenaPathLen      = 256
	offArenaSize      = 272
	offInterruptFlags = 280
	offInterruptEpoch = 288
	offInterruptAck   = 296
	offPayloadOffset  = 304
	offPayloadLen     = 312
	offCurrentRoot    = 320
	currentRootLen    = 32
)
