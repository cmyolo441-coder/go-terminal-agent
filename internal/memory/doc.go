// Package memory is the agent's long-term semantic memory: facts,
// preferences, decisions and episodes that survive across sessions and are
// recalled into the prompt when they are relevant to the task at hand.
//
// # Architecture
//
// A [Store] keeps every live [Record] in memory together with two indexes:
//
//   - an HNSW approximate-nearest-neighbour graph ([HNSW]) over unit-length
//     embeddings produced by an [Embedder] (the offline [HashEmbedder] by
//     default, or [HTTPEmbedder] for an OpenAI-compatible endpoint), and
//   - a BM25 inverted index ([BM25]) over the same text and tags.
//
// [Store.Recall] runs both, fuses the two rankings with Reciprocal Rank
// Fusion, weights the fused relevance by recency and importance and finally
// diversifies the list with Maximal Marginal Relevance. Every [Hit] carries
// an [Explanation] of how its score came about, and [FormatHits] renders hits
// for prompt injection within a token budget.
//
// # Safety
//
// Every piece of text that enters the store (record text, tags, namespaces,
// consolidation summaries and recall queries) first passes through the
// mandatory Options.Redact function, so secrets never reach the disk or a
// remote embedding endpoint.
//
// # Durability
//
// A persistent store lives in one directory:
//
//	LOCK                  advisory inter-process lock (held only while writing)
//	MANIFEST.json         generation, file names, sizes and SHA-256 of the segment
//	seg-<gen>.bin         snapshot: checksummed record frames + the HNSW graph
//	wal-<gen>.log         write-ahead log of changes since the snapshot
//
// Every change is appended to the write-ahead log (and fsynced, unless
// disabled) before it becomes visible. A snapshot writes a new segment and an
// empty log under a new generation, switches the manifest with an atomic
// rename and only then removes the previous generation, so a crash at any
// point leaves either the old or the new generation intact. Torn log tails
// are detected by per-frame CRC-32C and truncated; a corrupted snapshot is
// reported as [ErrCorrupt] (or salvaged with Options.RecoverCorrupt).
//
// Several gta processes can share one store directory: writers serialise on
// the LOCK file and every process replays the log entries written by others
// before it changes anything, and before a recall when the files changed.
package memory
