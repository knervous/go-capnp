package capnp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"

	"capnproto.org/go/capnp/v3/exc"
	"capnproto.org/go/capnp/v3/exp/bufferpool"
	"capnproto.org/go/capnp/v3/internal/str"
	"capnproto.org/go/capnp/v3/packed"
)

// A Decoder represents a framer that deserializes a particular Cap'n
// Proto input stream.
type Decoder struct {
	r io.Reader

	wordbuf [wordSize]byte
	hdrbuf  []byte

	// Maximum number of bytes that can be read per call to Decode.
	// If not set, a reasonable default is used.
	MaxMessageSize uint64
}

// NewDecoder creates a new Cap'n Proto framer that reads from r.
// The returned decoder will only read as much data as necessary to
// decode the message.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: r}
}

// NewPackedDecoder creates a new Cap'n Proto framer that reads from a
// packed stream r.  The returned decoder may read more data than
// necessary from r.
func NewPackedDecoder(r io.Reader) *Decoder {
	return NewDecoder(packed.NewReader(bufio.NewReader(r)))
}

// Decode reads a message from the decoder stream.  The error is io.EOF
// only if no bytes were read.
func (d *Decoder) Decode() (*Message, error) {
	maxSize := d.MaxMessageSize
	if maxSize == 0 {
		maxSize = defaultDecodeLimit
	} else if maxSize < uint64(len(d.wordbuf)) {
		return nil, errors.New("decode: max message size is smaller than header size")
	}

	hdr, err := d.readHeader(maxSize)
	if err != nil {
		return nil, err
	}

	total, err := hdr.totalSize()
	if err != nil {
		return nil, exc.WrapError("decode", err)
	}

	// Special case an empty message to return a new MultiSegment message
	// ready for writing. This maintains compatibility to tests and older
	// implementation of message and arenas.
	if hdr.maxSegment() == 0 && total == 0 {
		msg, _ := NewMultiSegmentMessage(nil)
		return msg, nil
	}

	// TODO(someday): if total size is greater than can fit in one buffer,
	// attempt to allocate buffer per segment.
	if total > maxSize-uint64(len(hdr)) || total > uint64(maxInt) {
		return nil, errors.New("decode: message too large")
	}

	// Read segments.
	bp := &bufferpool.Default
	buf := bp.Get(int(total))
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return nil, exc.WrapError("decode: read segments", err)
	}

	arena := MultiSegment(nil)
	if err = arena.demux(hdr, buf, bp); err != nil {
		return nil, exc.WrapError("decode", err)
	}

	msg, _, err := NewMessage(arena)
	return msg, err
}

func (d *Decoder) readHeader(maxSize uint64) (streamHeader, error) {
	// Read first word (number of segments and first segment size).
	// For single-segment messages, this will be sufficient.
	maxSeg, err := d.readMaxSeg()
	if err != nil {
		return nil, err
	}

	// single-segment message?
	if maxSeg == 0 {
		return d.wordbuf[:], nil
	}

	// Read the rest of the header if more than one segment.
	hdrSize := streamHeaderSize(maxSeg)
	if hdrSize > maxSize || hdrSize > uint64(maxInt) {
		return nil, errors.New("decode: message too large")
	}

	d.hdrbuf = resizeSlice(d.hdrbuf, int(hdrSize))
	copy(d.hdrbuf, d.wordbuf[:])
	if _, err := io.ReadFull(d.r, d.hdrbuf[len(d.wordbuf):]); err != nil {
		return nil, exc.WrapError("decode: read header", err)
	}

	return d.hdrbuf, nil
}

func (d *Decoder) readMaxSeg() (SegmentID, error) {
	if _, err := io.ReadFull(d.r, d.wordbuf[:]); err == io.EOF {
		return 0, io.EOF
	} else if err != nil {
		return 0, exc.WrapError("decode: read header", err)
	}

	maxSeg := SegmentID(binary.LittleEndian.Uint32(d.wordbuf[:]))
	if maxSeg > maxStreamSegments {
		return 0, errSegIDTooLarge(maxSeg)
	}

	return maxSeg, nil
}

type errSegIDTooLarge SegmentID

func (err errSegIDTooLarge) Error() string {
	id := str.Utod(err)
	max := str.Itod(maxStreamSegments)
	return "decode: segment id" + id + "exceeds max segment count (max=" + max + ")"
}

func resizeSlice(b []byte, size int) []byte {
	if cap(b) < size {
		bufferpool.Default.Put(b)
		return bufferpool.Default.Get(size)
	}
	return b[:size]
}

// Unmarshal reads an unpacked serialized stream into a message.  No
// copying is performed, so the objects in the returned message read
// directly from data.
func Unmarshal(data []byte) (*Message, error) {
	if len(data) == 0 {
		return nil, io.EOF
	}
	if len(data) < int(wordSize) {
		return nil, errors.New("unmarshal: short header section")
	}
	maxSeg := SegmentID(binary.LittleEndian.Uint32(data))
	hdrSize := streamHeaderSize(maxSeg)
	if uint64(len(data)) < hdrSize {
		return nil, errors.New("unmarshal: short header section")
	}
	hdr := streamHeader(data[:hdrSize])
	data = data[hdrSize:]
	if total, err := hdr.totalSize(); err != nil {
		return nil, exc.WrapError("unmarshal", err)
	} else if total > uint64(len(data)) {
		return nil, errors.New("unmarshal: short data section")
	}

	arena := MultiSegment(nil)
	if err := arena.demux(hdr, data, nil); err != nil {
		return nil, exc.WrapError("unmarshal", err)
	}

	msg, _, err := NewMessage(arena)
	return msg, err
}

// UnmarshalPacked reads a packed serialized stream into a message.
func UnmarshalPacked(data []byte) (*Message, error) {
	if len(data) == 0 {
		return nil, io.EOF
	}
	data, err := packed.Unpack(nil, data)
	if err != nil {
		return nil, exc.WrapError("unmarshal", err)
	}
	return Unmarshal(data)
}

// UnmarshalPackedZeroTo unpacks `packedData` into your reusable unpack buffer,
// then does a zero‐copy header demux into your reusable hdrScratch, and finally
// ResetNoAlloc’s your msg to point at the new arena.  No new heap allocations
// beyond your two backing arrays.
//
//	var hdrScratch   []byte
//	var unpackScratch []byte
//	var msg *capnp.Message // pre-allocated once
//
// Each call will do:
//
//	unpackScratch = unpackScratch[:0]
//	hdrScratch    = hdrScratch[:0]         // not strictly needed but keeps it clear
//	err := UnmarshalPackedZeroTo(msg, &hdrScratch, &unpackScratch, packedData)
func UnmarshalPackedZeroTo(
	msg *Message,
	hdrScratch *[]byte,
	unpackScratch *[]byte,
	packedData []byte,
) error {
	// 1) Unpack into your scratch, re-using its backing array.
	*unpackScratch = (*unpackScratch)[:0]
	out, err := packed.Unpack(*unpackScratch, packedData)
	if err != nil {
		return exc.WrapError("unmarshalPackedZero: unpack", err)
	}
	*unpackScratch = out

	// 2) Delegate to UnmarshalZeroTo logic: zero-copy header, in-place demux, ResetNoAlloc
	return func() error {
		data := out
		// must have at least one word
		if len(data) < int(wordSize) {
			return errors.New("unmarshalPackedZero: short header")
		}
		// figure header size
		maxSeg := SegmentID(binary.LittleEndian.Uint32(data[:4]))
		hdrSize := streamHeaderSize(maxSeg)
		if uint64(len(data)) < hdrSize {
			return errors.New("unmarshalPackedZero: short header")
		}
		// zero-copy header slice
		*hdrScratch = data[:hdrSize:hdrSize]
		// demux payload
		payload := data[hdrSize:]
		arena := MultiSegment(nil)
		if err := arena.demux(streamHeader(*hdrScratch), payload, nil); err != nil {
			return exc.WrapError("unmarshalPackedZero: demux", err)
		}
		// reset your pre-allocated Message without allocations
		msg.ResetNoAlloc(arena)
		return nil
	}()
}

// UnmarshalZeroTo re-uses both the caller’s header scratch buffer
// and a pre-allocated *Message.  No heap allocations beyond
// those slices’ backing arrays ever happen.
func UnmarshalZeroTo(msg *Message, hdrScratch *[]byte, data []byte) error {
	// 1) must have at least one word for the header
	if len(data) == 0 {
		return io.EOF
	}
	if len(data) < int(wordSize) {
		return errors.New("unmarshal zero: short header section")
	}

	// 2) figure out how big the header is
	maxSeg := SegmentID(binary.LittleEndian.Uint32(data[:4]))
	hdrSize := streamHeaderSize(maxSeg)
	if uint64(len(data)) < hdrSize {
		return errors.New("unmarshal zero: short header section")
	}

	// 3) zero-copy into the scratch: just re-slice data
	*hdrScratch = data[:hdrSize:hdrSize]

	// 4) demux in place
	payload := data[hdrSize:]
	arena := MultiSegment(nil)
	if err := arena.demux(streamHeader(*hdrScratch), payload, nil); err != nil {
		return exc.WrapError("unmarshal zero", err)
	}

	// 5) reset existing Message to point at the new arena
	msg.Reset(arena)
	return nil
}

// MustUnmarshalRoot reads an unpacked serialized stream and returns
// its root pointer.  If there is any error, it panics.
func MustUnmarshalRoot(data []byte) Ptr {
	msg, err := Unmarshal(data)
	if err != nil {
		panic(err)
	}
	p, err := msg.Root()
	if err != nil {
		panic(err)
	}
	return p
}

var (
	errTooManySegments = errors.New("message has too many segments")
)

// An Encoder represents a framer for serializing a particular Cap'n
// Proto stream.
type Encoder struct {
	w      io.Writer
	hdrbuf []byte
	bufs   [][]byte
}

// NewEncoder creates a new Cap'n Proto framer that writes to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// NewPackedEncoder creates a new Cap'n Proto framer that writes to a
// packed stream w.
func NewPackedEncoder(w io.Writer) *Encoder {
	return NewEncoder(&packed.Writer{Writer: w})
}

// Encode writes a message to the encoder stream.
func (e *Encoder) Encode(m *Message) error {
	nsegs := m.NumSegments()
	if nsegs == 0 {
		return errors.New("encode: message has no segments")
	}
	if nsegs > 1<<32 {
		return exc.WrapError("encode", errTooManySegments)
	}
	e.bufs = append(e.bufs[:0], nil) // first element is placeholder for header
	maxSeg := SegmentID(nsegs - 1)
	hdrSize := streamHeaderSize(maxSeg)
	if hdrSize > uint64(maxInt) {
		return errors.New("encode: header size overflows int")
	}
	e.hdrbuf = resizeSlice(e.hdrbuf, int(hdrSize))
	e.hdrbuf = appendUint32(e.hdrbuf[:0], uint32(maxSeg))
	for i := int64(0); i < nsegs; i++ {
		s, err := m.Segment(SegmentID(i))
		if err != nil {
			return exc.WrapError("encode", err)
		}
		n := len(s.data)
		if int64(n) > int64(maxSegmentSize) {
			return errors.New("encode: segment " + str.Itod(i) + " too large")
		}
		e.hdrbuf = appendUint32(e.hdrbuf, uint32(Size(n)/wordSize))
		e.bufs = append(e.bufs, s.data)
	}
	if len(e.hdrbuf)%int(wordSize) != 0 {
		e.hdrbuf = appendUint32(e.hdrbuf, 0)
	}
	e.bufs[0] = e.hdrbuf

	if err := e.write(e.bufs); err != nil {
		return exc.WrapError("encode", err)
	}

	return nil
}

func (e *Encoder) write(bufs [][]byte) error {
	_, err := (*net.Buffers)(&bufs).WriteTo(e.w)
	return err
}

// streamHeaderSize returns the size of the header, given the lower 32
// bits of the first word of the header (the number of segments minus
// one).
func streamHeaderSize(maxSeg SegmentID) uint64 {
	return ((uint64(maxSeg)+2)*4 + 7) &^ 7
}

// appendUint32 appends a uint32 to a byte slice and returns the
// new slice.
func appendUint32(b []byte, v uint32) []byte {
	b = append(b, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(b[len(b)-4:], v)
	return b
}

// streamHeader holds the framing words at the beginning of a serialized
// Cap'n Proto message.
type streamHeader []byte

// maxSegment returns the number of segments in the message minus one.
func (h streamHeader) maxSegment() SegmentID {
	return SegmentID(binary.LittleEndian.Uint32(h))
}

// segmentSize returns the size of segment i, returning an error if the
// segment overflows maxSegmentSize.
func (h streamHeader) segmentSize(i SegmentID) (Size, error) {
	s := binary.LittleEndian.Uint32(h[4+i*4:])
	if sz, ok := wordSize.times(int32(s)); ok {
		return sz, nil
	}

	return 0, errors.New("segment " + str.Utod(i) + ": overflow size")
}

// totalSize returns the sum of all the segment sizes.  The sum will
// be in the range [0, 0xfffffff800000000].
func (h streamHeader) totalSize() (uint64, error) {
	var sum uint64
	for i := uint64(0); i <= uint64(h.maxSegment()); i++ {
		x, err := h.segmentSize(SegmentID(i))
		if err != nil {
			return sum, err
		}
		sum += uint64(x)
	}
	return sum, nil
}

const maxInt = int(^uint(0) >> 1)
