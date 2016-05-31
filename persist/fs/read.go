package fs

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"time"

	schema "code.uber.internal/infra/memtsdb/persist/fs/proto"
	xtime "code.uber.internal/infra/memtsdb/x/time"

	"github.com/golang/protobuf/proto"
)

type fileReader func(fd *os.File, buf []byte) (int, error)

var (
	// ErrReadIndexEntryZeroSize returned when size of next index entry is zero
	ErrReadIndexEntryZeroSize = errors.New("next index entry is encoded as zero size")

	// ErrReadNotExpectedSize returned when the size of the next read does not match size specified by the index
	ErrReadNotExpectedSize = errors.New("next read not expected size")

	// ErrReadMarkerNotFound returned when the marker is not found at the beginning of a data record
	ErrReadMarkerNotFound = errors.New("expected marker not found")
)

// ErrReadWrongIdx returned when the wrong idx is read in the data file
type ErrReadWrongIdx struct {
	ExpectedIdx int64
	ActualIdx   int64
}

func (e ErrReadWrongIdx) Error() string {
	return fmt.Sprintf("expected idx %d but found idx %d", e.ExpectedIdx, e.ActualIdx)
}

type reader struct {
	filePathPrefix string
	start          time.Time
	window         time.Duration

	infoFd  *os.File
	indexFd *os.File
	dataFd  *os.File

	read        fileReader
	entries     int
	entriesRead int
	indexUnread []byte
	currEntry   schema.IndexEntry
}

// NewReader returns a new reader for a filePathPrefix, expects all files to exist.  Will
// read the index info.
func NewReader(filePathPrefix string) Reader {
	return &reader{
		filePathPrefix: filePathPrefix,
		read:           readFile,
	}
}

func (r *reader) Open(shard uint32, version int) error {
	shardDir := ShardDirPath(r.filePathPrefix, shard)
	if err := openFiles(os.Open, map[string]**os.File{
		filepathFromVersion(shardDir, version, infoFileSuffix):  &r.infoFd,
		filepathFromVersion(shardDir, version, indexFileSuffix): &r.indexFd,
		filepathFromVersion(shardDir, version, dataFileSuffix):  &r.dataFd,
	}); err != nil {
		return err
	}
	if err := r.readInfo(); err != nil {
		// Try to close if failed to read info
		r.Close()
		return err
	}
	if err := r.readIndex(); err != nil {
		// Try to close if failed to read index
		r.Close()
		return err
	}
	return nil
}

func (r *reader) readInfo() error {
	info, err := ReadInfo(r.infoFd)
	if err != nil {
		return err
	}

	r.start = xtime.FromNanoseconds(info.Start)
	r.window = time.Duration(info.Window)
	r.entries = int(info.Entries)
	r.entriesRead = 0
	return nil
}

func (r *reader) readIndex() error {
	// NB(r): use a bytes.NewReader if/when protobuf library supports buffered reading
	data, err := ioutil.ReadAll(r.indexFd)
	if err != nil {
		return err
	}

	r.indexUnread = data

	return nil
}

func (r *reader) Read() (string, []byte, error) {
	var none string
	entry := &r.currEntry
	entry.Reset()

	size, consumed := proto.DecodeVarint(r.indexUnread)
	r.indexUnread = r.indexUnread[consumed:]
	if consumed < 1 {
		return none, nil, ErrReadIndexEntryZeroSize
	}
	indexEntryData := r.indexUnread[:size]
	if err := proto.Unmarshal(indexEntryData, entry); err != nil {
		return none, nil, err
	}
	r.indexUnread = r.indexUnread[size:]

	expectedSize := markerLen + idxLen + int(entry.Size)
	data := make([]byte, expectedSize)
	n, err := r.read(r.dataFd, data)
	if err != nil {
		return none, nil, err
	}
	if n != expectedSize {
		return none, nil, ErrReadNotExpectedSize
	}

	if !bytes.Equal(data[:markerLen], marker) {
		return none, nil, ErrReadMarkerNotFound
	}

	idx := int64(endianness.Uint64(data[markerLen : markerLen+idxLen]))
	if idx != entry.Idx {
		return none, nil, ErrReadWrongIdx{ExpectedIdx: entry.Idx, ActualIdx: idx}
	}

	r.entriesRead++

	return entry.Key, data[markerLen+idxLen:], nil
}

func (r *reader) Range() xtime.Range {
	return xtime.Range{Start: r.start, End: r.start.Add(r.window)}
}

func (r *reader) Entries() int {
	return r.entries
}

func (r *reader) EntriesRead() int {
	return r.entriesRead
}

func (r *reader) Close() error {
	return closeFiles(r.infoFd, r.indexFd, r.dataFd)
}

func readFile(fd *os.File, buf []byte) (int, error) {
	return fd.Read(buf)
}
