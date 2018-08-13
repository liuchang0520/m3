	"github.com/m3db/m3/src/dbnode/clock"
	"github.com/m3db/m3/src/dbnode/encoding"
	"github.com/m3db/m3/src/dbnode/sharding"
	"github.com/m3db/m3/src/dbnode/ts"
	// WriteData writes the data as data files.
	WriteData(
		ns ident.ID, shards sharding.ShardSet, data SeriesBlocksByStart) error

	// WriteSnapshot writes the data as snapshot files.
	WriteSnapshot(
		ns ident.ID,
		shards sharding.ShardSet,
		data SeriesBlocksByStart,
		snapshotInterval time.Duration,
	) error

	// WriteDataWithPredicate writes all data that passes the predicate test as data files.
	WriteDataWithPredicate(
		ns ident.ID,
		shards sharding.ShardSet,
		data SeriesBlocksByStart,
		pred WriteDatapointPredicate,
	) error

	// WriteSnapshotWithPredicate writes all data that passes the predicate test as snapshot files.
	WriteSnapshotWithPredicate(
		ns ident.ID,
		shards sharding.ShardSet,
		data SeriesBlocksByStart,
		pred WriteDatapointPredicate,
		snapshotInterval time.Duration,
	) error
	// SetWriteSnapshot sets whether writes are written as snapshot files
	SetWriteSnapshot(bool) Options

	// WriteSnapshots returns whether writes are written as snapshot files
	WriteSnapshot() bool


// WriteDatapointPredicate returns a boolean indicating whether a datapoint should be written.
type WriteDatapointPredicate func(dp ts.Datapoint) bool