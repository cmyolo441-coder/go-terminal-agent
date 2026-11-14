// Package collab adds real-time multi-user collaboration to an agent
// session: several people join a room to watch the agent work, chat,
// co-edit the next prompt, and vote on tool calls before they run.
//
// # Replicated data types
//
// The shared state is built from conflict-free replicated data types
// written for this package:
//
//   - Text, a sequence CRDT (RGA) for the shared prompt. Characters are
//     named by Lamport timestamps; consecutive insertions are stored and
//     sent as runs; deletions leave tombstones that CollectGarbage removes
//     once they are causally stable.
//   - ORSet, an observed-remove (add-wins) set, used for the shared list of
//     context items.
//   - LWWMap, a last-writer-wins map stamped by a hybrid logical Clock,
//     used for room settings.
//
// All three number their operations per replica and track what they have
// applied in a VersionVector, so a peer can ask for exactly the operations
// it is missing (Delta) and operations may be delivered late, out of order
// or more than once.
//
// # Rooms
//
// A Hub hosts Rooms. Each Room is an actor: one goroutine owns all of its
// state and processes requests in order, which totally orders events.
// Participants hold bearer tokens and one of three roles, enforced by the
// server: owners manage the room, editors edit and vote, viewers watch and
// chat. Tool-call approvals are decided by a QuorumPolicy such as "any
// owner, or two editors".
//
// # Transport
//
// Hub.Handler serves an HTTP API: Server-Sent Events downstream and JSON
// POSTs upstream, both of which survive ordinary HTTP proxies. Sequenced
// events carry their room sequence number as the SSE id, a bounded ring
// buffer replays what a reconnecting client missed (Last-Event-ID), and a
// client too slow to keep up is disconnected with a reason so that it
// resumes instead of stalling the room. Upstream messages are rate limited
// per participant.
//
// # Recording
//
// A Recorder receives every event for later replay. LogRecorder writes a
// hash-chained NDJSON log; Replayer reads it back with adjustable speed.
//
// # Client protocol
//
// A client opens GET .../events and treats the first event, welcome, as
// authoritative: when welcome.resumed is false it replaces its replicas
// with the snapshots in the welcome and re-issues any edits the server had
// not acknowledged as new operations. It then applies ops events (and its
// own operations locally), sending its version vectors in heartbeats so
// the server can collect garbage. A 409 resync_required reply means the
// client must send sync with reset and rebase in the same way.
package collab
