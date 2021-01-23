// TODO(u): Evaluate storing the samples (and residuals) during frame audio
// decoding in a buffer allocated for the stream. This buffer would be allocated
// using BlockSize and NChannels from the StreamInfo block, and it could be
// reused in between calls to Next and ParseNext. This should reduce GC
// pressure.

// TODO: Remove note about encoder API.

// Package flac provides access to FLAC (Free Lossless Audio Codec) streams.
//
// A brief introduction of the FLAC stream format [1] follows. Each FLAC stream
// starts with a 32-bit signature ("fLaC"), followed by one or more metadata
// blocks, and then one or more audio frames. The first metadata block
// (StreamInfo) describes the basic properties of the audio stream and it is the
// only mandatory metadata block. Subsequent metadata blocks may appear in an
// arbitrary order.
//
// Please refer to the documentation of the meta [2] and the frame [3] packages
// for a brief introduction of their respective formats.
//
//    [1]: https://www.xiph.org/flac/format.html#stream
//    [2]: https://godoc.org/github.com/mewkiz/flac/meta
//    [3]: https://godoc.org/github.com/mewkiz/flac/frame
//
// Note: the Encoder API is experimental until the 1.1.x release. As such, it's
// API is expected to change.
package flac

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// A Stream contains the metadata blocks and provides access to the audio frames
// of a FLAC stream.
//
// ref: https://www.xiph.org/flac/format.html#stream
type Stream struct {
	// The StreamInfo metadata block describes the basic properties of the FLAC
	// audio stream.
	Info *meta.StreamInfo
	// Zero or more metadata blocks.
	Blocks []*meta.Block

	// seekTable contains one or more pre-calculated audio frame seek points of the stream; nil if uninitialized.
	seekTable *meta.SeekTable
	// seekTableSize determines how many seek points the seekTable should have if the flac file does not include one
	// in the metadata.
	seekTableSize int
	// dataStart is the offset of the first frame header since SeekPoint.Offset is relative to this position.
	dataStart int64

	// Underlying io.Reader.
	r io.Reader
	// Underlying io.Closer of file if opened with Open and ParseFile, and nil
	// otherwise.
	c io.Closer
}

// New creates a new Stream for accessing the audio samples of r. It reads and
// parses the FLAC signature and the StreamInfo metadata block, but skips all
// other metadata blocks.
//
// Call Stream.Next to parse the frame header of the next audio frame, and call
// Stream.ParseNext to parse the entire next frame including audio samples.
func New(r io.Reader) (stream *Stream, err error) {
	// Verify FLAC signature and parse the StreamInfo metadata block.
	br := bufio.NewReader(r)
	stream = &Stream{r: br}
	block, err := stream.parseStreamInfo()
	if err != nil {
		return nil, err
	}

	// Skip the remaining metadata blocks.
	for !block.IsLast {
		block, err = meta.New(br)
		if err != nil && err != meta.ErrReservedType {
			return stream, err
		}
		if err = block.Skip(); err != nil {
			return stream, err
		}
	}

	return stream, nil
}

// NewSeek returns a Stream that has seeking enabled.  The incoming
// io.ReadSeeker will not be buffered, which might result in performance issues.
// Using an in-memory buffer like *bytes.Reader should work well.
func NewSeek(r io.Reader) (stream *Stream, err error) {
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		return stream, ErrNoSeeker
	}

	stream = &Stream{r: rs, seekTableSize: defaultSeekTableSize}

	// Verify FLAC signature and parse the StreamInfo metadata block.
	block, err := stream.parseStreamInfo()
	if err != nil {
		return stream, err
	}

	for !block.IsLast {
		block, err = meta.Parse(stream.r)
		if err != nil {
			if err != meta.ErrReservedType {
				return stream, err
			} else {
				if err = block.Skip(); err != nil {
					return stream, err
				}
			}
		}

		if block.Header.Type == meta.TypeSeekTable {
			stream.seekTable = block.Body.(*meta.SeekTable)
		}
	}

	// Record file offset of the first frame header.
	stream.dataStart, err = rs.Seek(0, io.SeekCurrent)
	return stream, err
}

var (
	// flacSignature marks the beginning of a FLAC stream.
	flacSignature = []byte("fLaC")

	// id3Signature marks the beginning of an ID3 stream, used to skip over ID3 data.
	id3Signature = []byte("ID3")

	ErrInvalidSeek = errors.New("stream.Seek: out of stream seek")
	ErrNoSeeker    = errors.New("stream.Seek: not a Seeker")
)

const (
	defaultSeekTableSize = 100
)

// parseStreamInfo verifies the signature which marks the beginning of a FLAC
// stream, and parses the StreamInfo metadata block. It returns a boolean value
// which specifies if the StreamInfo block was the last metadata block of the
// FLAC stream.
func (stream *Stream) parseStreamInfo() (block *meta.Block, err error) {
	// Verify FLAC signature.
	r := stream.r
	var buf [4]byte
	if _, err = io.ReadFull(r, buf[:]); err != nil {
		return block, err
	}

	// Skip prepended ID3v2 data.
	if bytes.Equal(buf[:3], id3Signature) {
		if err := stream.skipID3v2(); err != nil {
			return block, err
		}

		// Second attempt at verifying signature.
		if _, err = io.ReadFull(r, buf[:]); err != nil {
			return block, err
		}
	}

	if !bytes.Equal(buf[:], flacSignature) {
		return block, fmt.Errorf("flac.parseStreamInfo: invalid FLAC signature; expected %q, got %q", flacSignature, buf)
	}

	// Parse StreamInfo metadata block.
	block, err = meta.Parse(r)
	if err != nil {
		return block, err
	}
	si, ok := block.Body.(*meta.StreamInfo)
	if !ok {
		return block, fmt.Errorf("flac.parseStreamInfo: incorrect type of first metadata block; expected *meta.StreamInfo, got %T", si)
	}
	stream.Info = si
	return block, nil
}

// skipID3v2 skips ID3v2 data prepended to flac files.
func (stream *Stream) skipID3v2() error {
	r := bufio.NewReader(stream.r)

	// Discard unnecessary data from the ID3v2 header.
	if _, err := r.Discard(2); err != nil {
		return err
	}

	// Read the size from the ID3v2 header.
	var sizeBuf [4]byte
	if _, err := r.Read(sizeBuf[:]); err != nil {
		return err
	}
	// The size is encoded as a synchsafe integer.
	size := int(sizeBuf[0])<<21 | int(sizeBuf[1])<<14 | int(sizeBuf[2])<<7 | int(sizeBuf[3])

	_, err := r.Discard(size)
	return err
}

// Parse creates a new Stream for accessing the metadata blocks and audio
// samples of r. It reads and parses the FLAC signature and all metadata blocks.
//
// Call Stream.Next to parse the frame header of the next audio frame, and call
// Stream.ParseNext to parse the entire next frame including audio samples.
func Parse(r io.Reader) (stream *Stream, err error) {
	// Verify FLAC signature and parse the StreamInfo metadata block.
	br := bufio.NewReader(r)
	stream = &Stream{r: br}
	block, err := stream.parseStreamInfo()
	if err != nil {
		return nil, err
	}

	// Parse the remaining metadata blocks.
	for !block.IsLast {
		block, err = meta.Parse(br)
		if err != nil {
			if err != meta.ErrReservedType {
				return stream, err
			}
			// Skip the body of unknown (reserved) metadata blocks, as stated by
			// the specification.
			//
			// ref: https://www.xiph.org/flac/format.html#format_overview
			if err = block.Skip(); err != nil {
				return stream, err
			}
		}
		stream.Blocks = append(stream.Blocks, block)
	}

	return stream, nil
}

// Open creates a new Stream for accessing the audio samples of path. It reads
// and parses the FLAC signature and the StreamInfo metadata block, but skips
// all other metadata blocks.
//
// Call Stream.Next to parse the frame header of the next audio frame, and call
// Stream.ParseNext to parse the entire next frame including audio samples.
//
// Note: The Close method of the stream must be called when finished using it.
func Open(path string) (stream *Stream, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	stream, err = New(f)
	if err != nil {
		return nil, err
	}
	stream.c = f
	return stream, err
}

// ParseFile creates a new Stream for accessing the metadata blocks and audio
// samples of path. It reads and parses the FLAC signature and all metadata
// blocks.
//
// Call Stream.Next to parse the frame header of the next audio frame, and call
// Stream.ParseNext to parse the entire next frame including audio samples.
//
// Note: The Close method of the stream must be called when finished using it.
func ParseFile(path string) (stream *Stream, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	stream, err = Parse(f)
	if err != nil {
		return nil, err
	}
	stream.c = f
	return stream, err
}

// Close closes the stream if opened through a call to Open or ParseFile, and
// performs no operation otherwise.
func (stream *Stream) Close() error {
	if stream.c != nil {
		return stream.c.Close()
	}
	return nil
}

// Next parses the frame header of the next audio frame. It returns io.EOF to
// signal a graceful end of FLAC stream.
//
// Call Frame.Parse to parse the audio samples of its subframes.
func (stream *Stream) Next() (f *frame.Frame, err error) {
	return frame.New(stream.r)
}

// ParseNext parses the entire next frame including audio samples. It returns
// io.EOF to signal a graceful end of FLAC stream.
func (stream *Stream) ParseNext() (f *frame.Frame, err error) {
	return frame.Parse(stream.r)
}

// Seek to a specific sample number in the flac stream.
//
// sample is valid if:
// whence == io.SeekEnd and sample is negative
// whence == io.SeekStart and sample is positive
// whence == io.SeekCurrent and sample + current sample > 0 and < stream.Info.NSamples
//
// If sample does not match one of the above conditions then the result will
// probably be seeking to the beginning or very end of the data and no error
// will be returned.
//
// The returned value, result, represents the closest match to sampleNum from the seek table.
// Note that result will always be >= sampleNum
func (stream *Stream) Seek(sampleNum int64, whence int) (result int64, err error) {
	if stream.seekTable == nil && stream.seekTableSize > 0 {
		if err := stream.makeSeekTable(); err != nil {
			return 0, err
		}
	}

	rs := stream.r.(io.ReadSeeker)

	var point meta.SeekPoint
	switch whence {
	case io.SeekStart:
		point = stream.searchFromStart(sampleNum)
	case io.SeekCurrent:
		point, err = stream.searchFromCurrent(sampleNum, rs)
	case io.SeekEnd:
		point = stream.searchFromEnd(sampleNum)
	default:
		return 0, ErrInvalidSeek
	}

	if err != nil {
		return 0, err
	}

	_, err = rs.Seek(stream.dataStart+int64(point.Offset), io.SeekStart)
	return int64(point.SampleNum), err
}

func (stream *Stream) searchFromCurrent(sample int64, rs io.ReadSeeker) (p meta.SeekPoint, err error) {
	o, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return p, err
	}

	offset := o - stream.dataStart
	for _, p = range stream.seekTable.Points {
		if int64(p.Offset) >= offset {
			return stream.searchFromStart(int64(p.SampleNum) + sample), nil
		}
	}
	return p, nil
}

// searchFromEnd expects sample to be negative.
// If it is positive, it's ok, the last seek point will be returned.
func (stream *Stream) searchFromEnd(sample int64) (p meta.SeekPoint) {
	return stream.searchFromStart(int64(stream.Info.NSamples) + sample)
}

func (stream *Stream) searchFromStart(sample int64) (p meta.SeekPoint) {
	var last meta.SeekPoint
	var i int
	for i, p = range stream.seekTable.Points {
		if int64(p.SampleNum) >= sample {
			if i == 0 {
				return p
			}
			return last
		}
		last = p
	}
	return p
}

func (stream *Stream) makeSeekTable() (err error) {
	rs, ok := stream.r.(io.ReadSeeker)
	if !ok {
		return ErrNoSeeker
	}

	pos, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}

	_, err = rs.Seek(stream.dataStart, io.SeekStart)
	if err != nil {
		return err
	}

	var i int
	var sampleNum uint64
	var tmp []meta.SeekPoint
	for {
		f, err := stream.ParseNext()
		if err == io.EOF {
			break
		}

		if err != nil {
			return err
		}

		o, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}

		tmp = append(tmp, meta.SeekPoint{
			SampleNum: sampleNum,
			Offset:    uint64(o - stream.dataStart),
			NSamples:  f.BlockSize,
		})

		sampleNum += uint64(f.BlockSize)
		i++
	}

	// reduce the number of seek points down to the specified resolution
	m := 1
	if len(tmp) > stream.seekTableSize {
		m = len(tmp) / stream.seekTableSize
	}
	points := make([]meta.SeekPoint, 0, stream.seekTableSize+1)
	for i, p := range tmp {
		if i%m == 0 {
			points = append(points, p)
		}
	}

	stream.seekTable = &meta.SeekTable{Points: points}

	_, err = rs.Seek(pos, io.SeekStart)
	return err
}
