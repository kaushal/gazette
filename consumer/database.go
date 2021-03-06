package consumer

import (
	rocks "github.com/tecbot/gorocksdb"

	"github.com/LiveRamp/gazette/journal"
	"github.com/LiveRamp/gazette/recoverylog"
)

type database struct {
	recoveryLog journal.Name
	logWriter   journal.Writer
	recorder    *recoverylog.Recorder

	*rocks.DB
	env          *rocks.Env
	options      *rocks.Options
	writeOptions *rocks.WriteOptions
	readOptions  *rocks.ReadOptions
	writeBatch   *rocks.WriteBatch
}

func newDatabase(options *rocks.Options, fsm *recoverylog.FSM, dir string,
	writer journal.Writer) (*database, error) {

	recorder, err := recoverylog.NewRecorder(fsm, len(dir), writer)
	if err != nil {
		return nil, err
	}

	db := &database{
		recoveryLog: fsm.LogMark.Journal,
		logWriter:   writer,
		recorder:    recorder,

		env:          rocks.NewObservedEnv(recorder),
		options:      options,
		readOptions:  rocks.NewDefaultReadOptions(),
		writeOptions: rocks.NewDefaultWriteOptions(),
		writeBatch:   rocks.NewWriteBatch(),
	}

	db.options.SetEnv(db.env)
	db.options.SetCreateIfMissing(true)

	// By default, we instruct RocksDB *not* to perform data syncs. We already
	// capture linearization of file write/rename/link operations via Gazette,
	// and transactions are applied via an atomic write batch. The result is
	// that we'll always recover a consistent database.
	//
	// Note that the consumer loop also installs a write-barrier between
	// transactions, which will block a current transaction from committing
	// until the previous one has been fully synced by Gazette.

	// TODO(johnny): This option has been removed from Rocks. Research if
	// there's another option we should use.
	//db.options.SetDisableDataSync(true)

	// The MANIFEST file is a WAL of database file state, including current live
	// SST files and their begin & ending key ranges. A new MANIFEST-00XYZ is
	// created at database start, where XYZ is the next available sequence number,
	// and CURRENT is updated to point at the live MANIFEST. By default MANIFEST
	// files may grow to 4GB, but they are typically written very slowly and thus
	// artificially inflate the recovery log horizon. We use a much smaller limit
	// to encourage more frequent snapshotting and rolling into new files.
	db.options.SetMaxManifestFileSize(1 << 17) // 131072 bytes.

	db.DB, err = rocks.OpenDb(db.options, dir)
	if err != nil {
		return db, err
	}
	return db, nil
}

func (db *database) commit() (*journal.AsyncAppend, error) {
	if err := db.Write(db.writeOptions, db.writeBatch); err != nil {
		return nil, err
	}
	db.writeBatch.Clear()

	// Issue an empty write. As writes from a client to a journal are applied
	// strictly in order, this is effectively a commit barrier: when it resolves,
	// the client knows the commit has been fully synced by Gazette.
	return db.logWriter.Write(db.recoveryLog, nil)
}

func (db *database) teardown() {
	if db.DB != nil {
		// Blocks until all background compaction has completed.
		db.DB.Close()
		db.DB = nil
	}
	if db.env != nil {
		db.env.Destroy()
		db.env = nil
	}

	db.options.Destroy()
	db.readOptions.Destroy()
	db.writeOptions.Destroy()
	db.writeBatch.Destroy()
}
