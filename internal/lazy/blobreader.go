package lazy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/registry"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

const (
	BlockSize       uint64 = 1 << 20 // 1 MiB
	DemandPiece     uint64 = 4 << 20 // 4 MiB
	BackgroundPiece uint64 = 8 << 20 // 8 MiB
	QuietDuration          = 1000 * time.Millisecond
	MapMagic               = "ZIGBLOB1"
)

var (
	ErrBlobMoved = errors.New("blob moved to store")
	ErrStopped   = errors.New("blob reader stopped")
)

type BlockState uint8

const (
	BlockMissing BlockState = iota
	BlockQueuedBackground
	BlockQueuedDemand
	BlockFetching
	BlockHave
)

type Piece struct {
	Start uint64
	End   uint64
}

type Priority int

const (
	PriorityDemand Priority = iota
	PriorityBackground
)

type BlobReader struct {
	Ref       registry.Reference
	Digest    string
	Size      uint64
	Tail      []byte
	TailStart uint64

	mu         sync.Mutex
	cond       *sync.Cond
	blocks     []BlockState
	haveBlocks int
	haveBytes  uint64
	demand     []Piece
	background []Piece

	backgroundOut  int
	backgroundBusy int
	demandBusy     int
	demandDoneAt   time.Time
	lastError      error
	stopping       bool

	fileMu   sync.RWMutex
	file     *os.File
	mem      [][]byte
	mapFile  *os.File
	dataPath string
	mapPath  string
	donePath string

	Requests   atomic.Uint64
	OnDemand   atomic.Uint64
	Downloaded atomic.Uint64
	Earlier    uint64

	regPool []*registry.Registry
	workers sync.WaitGroup
	quitCh  chan struct{}
}

func NewBlobReader(ref registry.Reference, digest string, size uint64, tail []byte, concurrency int) (*BlobReader, error) {
	if concurrency <= 0 {
		concurrency = 3
	}
	tLen := uint64(len(tail))
	tailStart := size - tLen
	if size < tLen {
		tailStart = 0
	}

	b := &BlobReader{
		Ref:       ref,
		Digest:    digest,
		Size:      size,
		Tail:      tail,
		TailStart: tailStart,
		quitCh:    make(chan struct{}),
	}
	b.cond = sync.NewCond(&b.mu)

	for i := 0; i < concurrency; i++ {
		r, err := registry.New(ref, "pull", false)
		if err != nil {
			return nil, err
		}
		b.regPool = append(b.regPool, r)
	}

	return b, nil
}

func (b *BlobReader) BlockCount() int {
	if b.TailStart == 0 {
		return 0
	}
	return int((b.TailStart + BlockSize - 1) / BlockSize)
}

func (b *BlobReader) BlockLen(idx int) uint64 {
	start := uint64(idx) * BlockSize
	if start+BlockSize > b.TailStart {
		return b.TailStart - start
	}
	return BlockSize
}

func (b *BlobReader) Attach(dataPath, mapPath string) (uint64, error) {
	b.dataPath = dataPath
	b.mapPath = mapPath

	n := b.BlockCount()
	b.blocks = make([]BlockState, n)

	f, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return 0, err
	}
	_ = setSparse(f)

	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return 0, err
	}
	if uint64(stat.Size()) != b.Size {
		_ = f.Truncate(int64(b.Size))
	}

	// Write tail
	_, _ = f.WriteAt(b.Tail, int64(b.TailStart))

	mf, err := os.OpenFile(mapPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		_ = f.Close()
		return 0, err
	}

	b.file = f
	b.mapFile = mf

	b.loadMap()

	for i, s := range b.blocks {
		if s == BlockHave {
			b.haveBlocks++
			b.haveBytes += b.BlockLen(i)
		}
	}
	b.Earlier = b.haveBytes
	b.writeMap()

	b.startWorkers()
	return b.haveBytes, nil
}

func (b *BlobReader) AttachMemory() {
	n := b.BlockCount()
	b.blocks = make([]BlockState, n)
	b.mem = make([][]byte, n)
	b.startWorkers()
}

func (b *BlobReader) startWorkers() {
	for _, reg := range b.regPool {
		b.workers.Add(1)
		go func(r *registry.Registry) {
			defer b.workers.Done()
			b.workerLoop(r)
		}(reg)
	}
}

func (b *BlobReader) Close() {
	b.mu.Lock()
	if b.stopping {
		b.mu.Unlock()
		return
	}
	b.stopping = true
	close(b.quitCh)
	b.cond.Broadcast()
	b.mu.Unlock()

	b.workers.Wait()

	b.fileMu.Lock()
	if b.file != nil {
		_ = b.file.Close()
		b.file = nil
	}
	if b.mapFile != nil {
		_ = b.mapFile.Close()
		b.mapFile = nil
	}
	b.fileMu.Unlock()
}

func setSparse(f *os.File) error {
	var in uint32 = 1
	var returned uint32
	return win32.DeviceIoControl(
		win32.HANDLE(f.Fd()),
		win32.FSCTL_SET_SPARSE,
		unsafe.Pointer(&in),
		uint32(unsafe.Sizeof(in)),
		nil,
		0,
		(*win32.DWORD)(&returned),
		nil,
	)
}

func (b *BlobReader) loadMap() {
	if b.mapFile == nil {
		return
	}
	n := len(b.blocks)
	data := make([]byte, 24+(n+7)/8)
	read, err := b.mapFile.ReadAt(data, 0)
	if err != nil || read != len(data) {
		return
	}
	if string(data[:8]) != MapMagic {
		return
	}
	bs := binary.LittleEndian.Uint64(data[8:16])
	sz := binary.LittleEndian.Uint64(data[16:24])
	if bs != BlockSize || sz != b.Size {
		return
	}
	for i := 0; i < n; i++ {
		byteIdx := 24 + i/8
		bit := byte(1 << (i % 8))
		if data[byteIdx]&bit != 0 {
			b.blocks[i] = BlockHave
		}
	}
}

func (b *BlobReader) writeMap() {
	if b.mapFile == nil {
		return
	}
	n := len(b.blocks)
	data := make([]byte, 24+(n+7)/8)
	copy(data[:8], MapMagic)
	binary.LittleEndian.PutUint64(data[8:16], BlockSize)
	binary.LittleEndian.PutUint64(data[16:24], b.Size)

	for i := 0; i < n; i++ {
		if b.blocks[i] == BlockHave {
			data[24+i/8] |= byte(1 << (i % 8))
		}
	}
	_, _ = b.mapFile.WriteAt(data, 0)
}

func (b *BlobReader) HaveBytes() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.donePath != "" {
		return b.Size
	}
	return b.haveBytes + uint64(len(b.Tail))
}

func (b *BlobReader) SetDone(donePath string) {
	b.fileMu.Lock()
	b.mu.Lock()
	b.donePath = donePath
	b.mu.Unlock()
	b.fileMu.Unlock()
}

func (b *BlobReader) ReadAt(p []byte, offset int64) (int, error) {
	uOffset := uint64(offset)
	if uOffset >= b.Size || len(p) == 0 {
		return 0, io.EOF
	}

	if uOffset >= b.TailStart {
		tailSlice := b.Tail[uOffset-b.TailStart:]
		n := copy(p, tailSlice)
		return n, nil
	}

	wantLen := uint64(len(p))
	if wantLen > b.TailStart-uOffset {
		wantLen = b.TailStart - uOffset
	}

	// Try reading from done path if available
	if b.readDone(uOffset, p[:wantLen]) {
		return int(wantLen), nil
	}

	if len(b.blocks) == 0 {
		// No blocks configured, direct fetch
		err := b.fetchRange(uOffset, p[:wantLen])
		if err != nil {
			return 0, err
		}
		b.OnDemand.Add(wantLen)
		return int(wantLen), nil
	}

	if err := b.request(uOffset, uOffset+wantLen, PriorityDemand); err != nil {
		return 0, err
	}
	if err := b.waitFor(uOffset, uOffset+wantLen); err != nil {
		return 0, err
	}

	if len(b.mem) > 0 {
		at := uOffset
		for at < uOffset+wantLen {
			idx := int(at / BlockSize)
			block := b.mem[idx]
			within := at - uint64(idx)*BlockSize
			n := copy(p[at-uOffset:], block[within:])
			at += uint64(n)
		}
		return int(wantLen), nil
	}

	b.fileMu.RLock()
	f := b.file
	b.fileMu.RUnlock()
	if f != nil {
		n, err := f.ReadAt(p[:wantLen], int64(uOffset))
		if err == nil || err == io.EOF {
			return n, nil
		}
	}

	if b.readDone(uOffset, p[:wantLen]) {
		return int(wantLen), nil
	}

	return 0, ErrBlobMoved
}

func (b *BlobReader) readDone(offset uint64, buf []byte) bool {
	b.fileMu.RLock()
	dp := b.donePath
	b.fileMu.RUnlock()
	if dp == "" {
		return false
	}
	f, err := os.Open(dp)
	if err != nil {
		return false
	}
	defer f.Close()

	n, err := f.ReadAt(buf, int64(offset))
	return err == nil && n == len(buf)
}

func (b *BlobReader) fetchRange(offset uint64, buf []byte) error {
	b.Requests.Add(1)
	end := offset + uint64(len(buf))
	span := registry.Span{Start: offset, End: &end}
	w := &fixedBufWriter{buf: buf}

	r := b.regPool[0]
	ok, err := r.GetBlob(b.Digest, span, w)
	if err != nil {
		return err
	}
	if !ok || w.n != len(buf) {
		return fmt.Errorf("range fetch for %s failed", oci.ShortDigest(b.Digest))
	}
	return nil
}

type fixedBufWriter struct {
	buf []byte
	n   int
}

func (w *fixedBufWriter) Write(p []byte) (int, error) {
	n := copy(w.buf[w.n:], p)
	w.n += n
	return n, nil
}

func (b *BlobReader) Prefetch(start, end uint64) {
	if len(b.blocks) == 0 {
		return
	}
	limit := end
	if limit > b.TailStart {
		limit = b.TailStart
	}
	_ = b.request(start, limit, PriorityDemand)
}

func (b *BlobReader) Ensure(start, end uint64) error {
	if len(b.blocks) == 0 {
		return nil
	}
	limit := end
	if limit > b.TailStart {
		limit = b.TailStart
	}
	if start >= limit {
		return nil
	}
	if err := b.request(start, limit, PriorityDemand); err != nil {
		return err
	}
	return b.waitFor(start, limit)
}

func (b *BlobReader) request(start, end uint64, prio Priority) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	first := int(start / BlockSize)
	last := int((end - 1) / BlockSize)

	var runStart uint64 = 0
	inRun := false

	enqueue := func(pStart, pEnd uint64) {
		piece := Piece{Start: pStart, End: pEnd}
		if prio == PriorityDemand {
			b.demand = append(b.demand, piece)
		} else {
			b.background = append(b.background, piece)
		}
		b.cond.Signal()
	}

	for i := first; i <= last; i++ {
		s := b.blocks[i]
		need := (s == BlockMissing || (prio == PriorityDemand && s == BlockQueuedBackground))
		if need {
			if !inRun {
				runStart = uint64(i) * BlockSize
				inRun = true
			}
			maxSpan := DemandPiece
			if prio == PriorityBackground {
				maxSpan = BackgroundPiece
			}
			runEnd := uint64(i+1) * BlockSize
			if runEnd > b.TailStart {
				runEnd = b.TailStart
			}
			if runEnd-runStart >= maxSpan {
				enqueue(runStart, runEnd)
				inRun = false
			}
			if prio == PriorityDemand {
				b.blocks[i] = BlockQueuedDemand
			} else {
				b.blocks[i] = BlockQueuedBackground
			}
		} else {
			if inRun {
				runEnd := uint64(i) * BlockSize
				if runEnd > b.TailStart {
					runEnd = b.TailStart
				}
				enqueue(runStart, runEnd)
				inRun = false
			}
		}
	}
	if inRun {
		runEnd := uint64(last+1) * BlockSize
		if runEnd > b.TailStart {
			runEnd = b.TailStart
		}
		enqueue(runStart, runEnd)
	}

	return nil
}

func (b *BlobReader) waitFor(start, end uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	first := int(start / BlockSize)
	last := int((end - 1) / BlockSize)

	for {
		if b.stopping {
			return ErrStopped
		}
		allHave := true
		for i := first; i <= last; i++ {
			if b.blocks[i] != BlockHave {
				allHave = false
				break
			}
		}
		if allHave {
			return nil
		}
		if b.lastError != nil {
			return b.lastError
		}
		b.cond.Wait()
	}
}

func (b *BlobReader) DownloadAll() error {
	if err := b.request(0, b.TailStart, PriorityBackground); err != nil {
		return err
	}
	return b.waitFor(0, b.TailStart)
}

func (b *BlobReader) workerLoop(reg *registry.Registry) {
	for {
		b.mu.Lock()
		for len(b.demand) == 0 && len(b.background) == 0 && !b.stopping {
			b.cond.Wait()
		}
		if b.stopping {
			b.mu.Unlock()
			return
		}

		var piece Piece
		var prio Priority

		if len(b.demand) > 0 {
			piece = b.demand[0]
			b.demand = b.demand[1:]
			prio = PriorityDemand
			b.demandBusy++
		} else if len(b.background) > 0 {
			// Check quiet period
			if time.Since(b.demandDoneAt) < QuietDuration && b.demandBusy == 0 {
				b.mu.Unlock()
				time.Sleep(100 * time.Millisecond)
				continue
			}
			piece = b.background[0]
			b.background = b.background[1:]
			prio = PriorityBackground
			b.backgroundBusy++
		}
		b.mu.Unlock()

		b.fetchPiece(reg, piece, prio)

		b.mu.Lock()
		if prio == PriorityDemand {
			b.demandBusy--
			b.demandDoneAt = time.Now()
		} else {
			b.backgroundBusy--
		}
		b.cond.Broadcast()
		b.mu.Unlock()
	}
}

func (b *BlobReader) fetchPiece(reg *registry.Registry, piece Piece, prio Priority) {
	length := piece.End - piece.Start
	buf := make([]byte, length)
	span := registry.Span{Start: piece.Start, End: &piece.End}
	w := &fixedBufWriter{buf: buf}

	b.Requests.Add(1)
	ok, err := reg.GetBlob(b.Digest, span, w)
	if err != nil || !ok || w.n != int(length) {
		b.mu.Lock()
		if err == nil {
			err = fmt.Errorf("range fetch failed for %s", oci.ShortDigest(b.Digest))
		}
		b.lastError = err
		b.mu.Unlock()
		return
	}

	if prio == PriorityDemand {
		b.OnDemand.Add(length)
	} else {
		b.Downloaded.Add(length)
	}

	// Store data
	if len(b.mem) > 0 {
		b.mu.Lock()
		first := int(piece.Start / BlockSize)
		last := int((piece.End - 1) / BlockSize)
		for i := first; i <= last; i++ {
			bStart := uint64(i) * BlockSize
			bLen := b.BlockLen(i)
			bData := make([]byte, bLen)
			copy(bData, buf[bStart-piece.Start:bStart-piece.Start+bLen])
			b.mem[i] = bData
			b.blocks[i] = BlockHave
			b.haveBlocks++
			b.haveBytes += bLen
		}
		b.mu.Unlock()
		return
	}

	b.fileMu.RLock()
	f := b.file
	b.fileMu.RUnlock()
	if f != nil {
		_, _ = f.WriteAt(buf, int64(piece.Start))
	}

	b.mu.Lock()
	first := int(piece.Start / BlockSize)
	last := int((piece.End - 1) / BlockSize)
	for i := first; i <= last; i++ {
		if b.blocks[i] != BlockHave {
			b.blocks[i] = BlockHave
			b.haveBlocks++
			b.haveBytes += b.BlockLen(i)
		}
	}
	b.writeMap()
	b.mu.Unlock()
}
