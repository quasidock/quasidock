package estargz

import (
	"sort"

	ce "github.com/containerd/stargz-snapshotter/estargz"
)

const (
	MaxRequests = 64
	MinGapSize  = 64 * 1024 // 64 KiB
)

type PieceType int

const (
	PieceFetch PieceType = iota
	PieceCopy
)

// Piece represents a slice of bytes in the reconstructed blob.
type Piece struct {
	Type         PieceType
	Offset       int64 // In the new blob
	Length       int64
	SourceIndex  int   // For PieceCopy: index in oldReaders
	SourceOffset int64 // For PieceCopy: offset in the old blob
}

// Plan defines how to assemble a new blob from existing local blobs and remote Range fetches.
type Plan struct {
	Pieces    []Piece
	TotalSize int64
}

// Copied returns the total number of bytes copied from local blobs.
func (p *Plan) Copied() int64 {
	var total int64
	for _, pc := range p.Pieces {
		if pc.Type == PieceCopy {
			total += pc.Length
		}
	}
	return total
}

// Fetched returns the total number of bytes fetched from the remote registry.
func (p *Plan) Fetched() int64 {
	var total int64
	for _, pc := range p.Pieces {
		if pc.Type == PieceFetch {
			total += pc.Length
		}
	}
	return total
}

// NumRequests returns the number of HTTP Range fetch pieces in the plan.
func (p *Plan) NumRequests() int {
	var count int
	for _, pc := range p.Pieces {
		if pc.Type == PieceFetch {
			count++
		}
	}
	return count
}

type oldMember struct {
	sourceIndex  int
	sourceOffset int64
	length       int64
}

// MakePlan constructs an update plan for assembling newReader's blob from oldReaders.
func MakePlan(newReader *Reader, oldReaders []*Reader, totalSize int64) (*Plan, error) {
	// Index old readers by (digest, compressed_length, uncompressed_size)
	type memberKey struct {
		digest   string
		compLen  int64
		uncompSz int64
	}

	oldMap := make(map[memberKey]oldMember)
	for oldIdx, oldR := range oldReaders {
		entries := oldR.AllEntries()
		for _, e := range entries {
			if e.Offset <= 0 {
				continue
			}
			compLen := min(e.NextOffset(), oldR.payloadEnd) - e.Offset
			if compLen <= 0 {
				continue
			}
			d := e.ChunkDigest
			if d == "" {
				d = e.Digest
			}
			if d == "" {
				continue
			}
			sz := e.ChunkSize
			if sz == 0 {
				sz = e.Size
			}
			key := memberKey{
				digest:   d,
				compLen:  compLen,
				uncompSz: sz,
			}
			if _, exists := oldMap[key]; !exists {
				oldMap[key] = oldMember{
					sourceIndex:  oldIdx,
					sourceOffset: e.Offset,
					length:       compLen,
				}
			}
		}
	}

	// Iterate through newReader's members in offset order
	newEntries := newReader.AllEntries()
	var sortedEntries []*ce.TOCEntry
	for _, e := range newEntries {
		if e.Offset >= 0 {
			sortedEntries = append(sortedEntries, e)
		}
	}
	sort.Slice(sortedEntries, func(i, j int) bool {
		return sortedEntries[i].Offset < sortedEntries[j].Offset
	})

	var pieces []Piece
	var curOffset int64

	for _, e := range sortedEntries {
		if e.Offset < curOffset {
			continue
		}
		// If there is a gap before this member, fetch it
		if e.Offset > curOffset {
			pieces = append(pieces, Piece{
				Type:   PieceFetch,
				Offset: curOffset,
				Length: e.Offset - curOffset,
			})
			curOffset = e.Offset
		}

		compLen := min(e.NextOffset(), newReader.payloadEnd) - e.Offset
		if compLen <= 0 {
			continue
		}

		d := e.ChunkDigest
		if d == "" {
			d = e.Digest
		}
		sz := e.ChunkSize
		if sz == 0 {
			sz = e.Size
		}

		key := memberKey{
			digest:   d,
			compLen:  compLen,
			uncompSz: sz,
		}

		if matched, ok := oldMap[key]; ok && d != "" {
			pieces = append(pieces, Piece{
				Type:         PieceCopy,
				Offset:       e.Offset,
				Length:       compLen,
				SourceIndex:  matched.sourceIndex,
				SourceOffset: matched.sourceOffset,
			})
		} else {
			pieces = append(pieces, Piece{
				Type:   PieceFetch,
				Offset: e.Offset,
				Length: compLen,
			})
		}
		curOffset = e.Offset + compLen
	}

	// Any remaining bytes (TOC and footer) up to totalSize are fetched
	if curOffset < totalSize {
		pieces = append(pieces, Piece{
			Type:   PieceFetch,
			Offset: curOffset,
			Length: totalSize - curOffset,
		})
	}

	p := &Plan{
		Pieces:    pieces,
		TotalSize: totalSize,
	}
	p.mergeAdjacent()
	return p, nil
}

// Coalesce coalesces fetch operations by bridging small copy gaps,
// limiting maximum requests to MaxRequests, and adjusting for bytes already fetched (haveFrom).
func (p *Plan) Coalesce(haveFrom int64) {
	p.mergeAdjacent()

	// 1. Merge small copy gaps (<= MinGapSize) between fetches
	changed := true
	for changed {
		changed = false
		for i := 1; i < len(p.Pieces)-1; i++ {
			if p.Pieces[i-1].Type == PieceFetch &&
				p.Pieces[i].Type == PieceCopy &&
				p.Pieces[i].Length <= MinGapSize &&
				p.Pieces[i+1].Type == PieceFetch {
				p.Pieces[i].Type = PieceFetch
				changed = true
			}
		}
		if changed {
			p.mergeAdjacent()
		}
	}

	// 2. Reduce number of fetch requests to MaxRequests by converting shortest copy gaps
	for p.NumRequests() > MaxRequests {
		shortestIdx := -1
		var shortestLen int64 = 1<<62 - 1

		for i := 1; i < len(p.Pieces)-1; i++ {
			if p.Pieces[i-1].Type == PieceFetch &&
				p.Pieces[i].Type == PieceCopy &&
				p.Pieces[i+1].Type == PieceFetch {
				if p.Pieces[i].Length < shortestLen {
					shortestLen = p.Pieces[i].Length
					shortestIdx = i
				}
			}
		}

		if shortestIdx == -1 {
			break
		}
		p.Pieces[shortestIdx].Type = PieceFetch
		p.mergeAdjacent()
	}

	// 3. Serve the already downloaded tail through the fetcher's cache. Keep
	// it in the plan so the reconstructed blob includes its TOC and footer.
	if haveFrom > 0 && haveFrom < p.TotalSize {
		var newPieces []Piece
		for _, pc := range p.Pieces {
			if pc.Offset >= haveFrom {
				// Past haveFrom; already downloaded (e.g. tail)
				continue
			}
			if pc.Offset+pc.Length > haveFrom {
				pc.Length = haveFrom - pc.Offset
			}
			if pc.Length > 0 {
				newPieces = append(newPieces, pc)
			}
		}
		newPieces = append(newPieces, Piece{Type: PieceFetch, Offset: haveFrom, Length: p.TotalSize - haveFrom})
		p.Pieces = newPieces
		p.mergeAdjacent()
	}
}

func (p *Plan) mergeAdjacent() {
	if len(p.Pieces) <= 1 {
		return
	}
	var merged []Piece
	current := p.Pieces[0]

	for i := 1; i < len(p.Pieces); i++ {
		next := p.Pieces[i]
		if current.Type == next.Type {
			if current.Type == PieceFetch && current.Offset+current.Length == next.Offset {
				current.Length += next.Length
				continue
			}
			if current.Type == PieceCopy &&
				current.SourceIndex == next.SourceIndex &&
				current.Offset+current.Length == next.Offset &&
				current.SourceOffset+current.Length == next.SourceOffset {
				current.Length += next.Length
				continue
			}
		}
		merged = append(merged, current)
		current = next
	}
	merged = append(merged, current)
	p.Pieces = merged
}
