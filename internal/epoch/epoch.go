package epoch

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

// formatTag domain-separates the hash and makes the framing version explicit.
//
// Changing anything about how [Compute] frames its input — the field set, their order, the
// framing itself — changes every epoch in the fleet. During a rolling upgrade a target running
// the new format reports an epoch that an older initiator recomputes differently and refuses,
// so an algorithm change is a **breaking protocol change** and must come with a bump of
// api.ProtocolVersion, not just a version of this tag.
const formatTag = "mxl-replicator/epoch/v1"

// nonceSeparator splits the incarnation nonce from the digest in an epoch string. Safe as a
// separator because [NewNonce] returns base32 (A-Z, 2-7), which contains no colon.
const nonceSeparator = ":"

// NewNonce returns a fresh incarnation nonce: 128 random bits, base32.
//
// The agent generates one **per target worker start** and holds it in memory alongside the
// process handle. Nothing persists it, and nothing needs to: an agent restart implies a worker
// restart implies a fresh nonce implies a new epoch implies the initiator reconnects, which is
// exactly the desired behaviour (§6.1).
func NewNonce() string { return rand.Text() }

// Compute returns the epoch for one target worker incarnation.
//
// The result is `<nonce>:<sha256 hex>`, where the digest covers the nonce *and* the fields of
// the blob that describe the endpoint and its memory registrations.
//
// # Why a content hash rather than a counter
//
// The initiator's reconcile rule is an *equality* test, not an ordering test — it never needs
// to know which epoch is newer, only whether the one it is running matches the one it was
// assigned. That buys three properties a counter does not have: the agent stays stateless,
// with no counter to persist across restarts; a mismatched or truncated blob is detectable
// before it reaches a worker ([Verify]); and it cannot desynchronise, whereas a counter that
// resets, or is bumped on a path that did not actually change the registration, produces
// either a missed reconnect or a spurious glitch. The flapping signal a counter would have
// given is recovered by counting epoch *transitions* server-side (§12).
//
// # Why the nonce is not redundant
//
// Hashing the content alone is *nearly* sufficient, but the guarantee it needs — that some
// hashed field always differs across a target restart — is not promised by anything. Measured
// against a real tcp target on mxl 1.1.0-rc1, two consecutive incarnations of the same worker
// produced a byte-identical `fabricAddress` (the agent deliberately reuses the port, §7.4), a
// byte-identical `len`, and `addr` of `"0"` in every region — the provider does not report a
// mapping address at all, so the ASLR entropy §5.2 assumes is simply absent. Only the rkeys
// differed, and nothing contractually promises they will.
//
// The failure mode if they ever collide is the worst one in the system: an initiator running
// happily against a dead endpoint, moving no data, with nothing reporting an error. The nonce
// costs one random string and removes the possibility entirely.
//
// # Why the nonce is also in the epoch string
//
// §5.2 defines the epoch as a hash *over* the nonce, which alone would make it unverifiable —
// the initiator holds the blob but never the peer's nonce, so it could not recompute anything.
// Carrying the nonce as a plain prefix makes [Verify] possible with no extra field on the
// wire and no extra plumbing through the server, while the digest still covers it exactly as
// §5.2 specifies. The nonce is not a secret; it is an incarnation discriminator.
func Compute(nonce string, info *TargetInfo) string {
	return nonce + nonceSeparator + digest(nonce, info)
}

// Verify recomputes the epoch from a blob and checks it against the epoch that was assigned.
//
// The source agent calls this before starting an initiator (§5.3 step 6). It catches a
// mismatched or truncated target_info before it is handed to a worker that would otherwise
// silently fail to move data — which, with no reconnect logic anywhere in the worker, presents
// as a healthy-looking process transferring nothing.
//
// It checks one (epoch, blob) pair for internal consistency, and deliberately not more than
// that: a pair that agrees with itself but is *stale* verifies happily. Noticing staleness is
// the reconcile loop's job, and it is a different test — the epoch this agent is running for a
// session against the epoch it was assigned for it.
func Verify(assigned string, info *TargetInfo) error {
	nonce, _, found := strings.Cut(assigned, nonceSeparator)
	if !found || nonce == "" {
		return fmt.Errorf("epoch %q is malformed: expected <nonce>%s<digest>", assigned, nonceSeparator)
	}

	if recomputed := Compute(nonce, info); recomputed != assigned {
		return fmt.Errorf("epoch mismatch: assigned %s, target info yields %s", assigned, recomputed)
	}
	return nil
}

// digest hashes the fields that identify an incarnation.
//
// Field selection follows §5.2. fabricAddress, addr and rkey are the load-bearing ones — they
// identify the endpoint and the remote memory registration. Two more are included on purpose:
//
//   - len, because a shrunk region turns into an RDMA protection error rather than corruption
//     (the NIC bounds-checks against the region), and a restart loop is a worse way to discover
//     that than a reconnect;
//   - bounceBufferInfo, which is the important one. The initiator computes scatter-gather
//     offsets *within* the bounce buffer ring from entrySize and entryCount, so a stale value
//     puts writes at the wrong offsets inside a correctly-registered region. The NIC sees
//     nothing wrong and the target unpacks garbage into the audio flow. It is the one field
//     whose omission causes silent data corruption rather than a visible failure.
//
// Every field is length-prefixed so the concatenation is unambiguous, and the region count is
// framed explicitly so that regions cannot be regrouped without changing the digest.
func digest(nonce string, info *TargetInfo) string {
	sum := sha256.New()

	writeBytes(sum, []byte(formatTag))
	writeBytes(sum, []byte(nonce))
	writeBytes(sum, []byte(info.FabricAddress))

	writeU64(sum, uint64(len(info.Regions)))
	for _, region := range info.Regions {
		writeU64(sum, uint64(region.Addr))
		writeU64(sum, uint64(region.Len))
		writeU64(sum, uint64(region.RKey))
	}

	// An absent bounce buffer hashes as a zero-sized one, which is what it is.
	var entryCount, entrySize U64
	if info.BounceBuffer != nil {
		entryCount, entrySize = info.BounceBuffer.EntryCount, info.BounceBuffer.EntrySize
	}
	writeU64(sum, uint64(entryCount))
	writeU64(sum, uint64(entrySize))

	return hex.EncodeToString(sum.Sum(nil))
}

func writeBytes(sum hash.Hash, value []byte) {
	writeU64(sum, uint64(len(value)))
	sum.Write(value)
}

func writeU64(sum hash.Hash, value uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], value)
	sum.Write(buf[:])
}
