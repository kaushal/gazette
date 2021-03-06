package recoverylog

import (
	"bytes"
	"io/ioutil"
	"net"
	"os"
	"testing"
	"time"

	gc "github.com/go-check/check"
	rocks "github.com/tecbot/gorocksdb"

	"github.com/LiveRamp/gazette/envflag"
	"github.com/LiveRamp/gazette/envflagfactory"
	"github.com/LiveRamp/gazette/gazette"
	"github.com/LiveRamp/gazette/journal"
	"github.com/LiveRamp/gazette/topic"
)

const (
	kTestLogName journal.Name = "pippio-journals/integration-tests/recovery-log"
)

type RecoveryLogSuite struct {
	gazette struct {
		*gazette.Client
		*gazette.WriteService
	}
}

func (s *RecoveryLogSuite) SetUpSuite(c *gc.C) {
	var gazetteEndpoint = envflagfactory.NewGazetteServiceEndpoint()
	envflag.CommandLine.Parse()

	var err error
	s.gazette.Client, err = gazette.NewClient(*gazetteEndpoint)
	c.Assert(err, gc.IsNil)

	// Skip if in Short mode, or if a Gazette endpoint is not reach-able.
	if testing.Short() {
		c.Skip("skipping recoverylog integration tests in short mode")
	}
	result, _ := s.gazette.Head(journal.ReadArgs{Journal: kTestLogName, Offset: -1})
	if _, ok := result.Error.(net.Error); ok {
		c.Skip("Gazette not available: " + result.Error.Error())
		return
	}

	s.gazette.WriteService = gazette.NewWriteService(s.gazette.Client)
	s.gazette.WriteService.Start()
}

func (s *RecoveryLogSuite) TearDownSuite(c *gc.C) {
	if s.gazette.WriteService != nil {
		s.gazette.WriteService.Stop()
	}
}

func (s *RecoveryLogSuite) TestSimpleStopAndStart(c *gc.C) {
	env := testEnv{c, s.gazette}

	replica1 := NewTestReplica(&env)
	defer replica1.teardown()

	replica1.startReading(FSMHints{Log: kTestLogName})
	c.Assert(replica1.makeLive(), gc.IsNil)

	replica1.put("key3", "value three!")
	replica1.put("key1", "value one")
	replica1.put("key2", "value2")

	replica2 := NewTestReplica(&env)
	defer replica2.teardown()

	hints := replica1.recorder.BuildHints()

	// |replica1| was initialized from empty hints and began writing at the
	// recoverylog head (offset -1). However, expect that the produced hints
	// reference absolute offsets of the log.
	c.Assert(hints.LiveNodes, gc.NotNil)
	for _, node := range hints.LiveNodes {
		for _, s := range node.Segments {
			c.Check(s.FirstOffset >= 0, gc.Equals, true)
		}
	}

	replica2.startReading(hints)
	c.Assert(replica2.makeLive(), gc.IsNil)

	replica2.expectValues(map[string]string{
		"key1": "value one",
		"key2": "value2",
		"key3": "value three!",
	})

	// Expect |replica1| & |replica2| share identical non-empty properties.
	c.Check(replica1.recorder.fsm.Properties, gc.Not(gc.HasLen), 0)
	c.Check(replica1.recorder.fsm.Properties, gc.DeepEquals,
		replica2.recorder.fsm.Properties)
}

func (s *RecoveryLogSuite) TestSimpleWarmStandby(c *gc.C) {
	env := testEnv{c, s.gazette}

	replica1 := NewTestReplica(&env)
	defer replica1.teardown()
	replica2 := NewTestReplica(&env)
	defer replica2.teardown()

	// Both replicas begin reading at the same time.
	replica1.startReading(FSMHints{Log: kTestLogName})
	replica2.startReading(FSMHints{Log: kTestLogName})

	// |replica1| is made live and writes content, while |replica2| is reading.
	c.Assert(replica1.makeLive(), gc.IsNil)
	replica1.put("key foo", "baz")
	replica1.put("key bar", "bing")

	// Make |replica2| live. Expect |replica1|'s content to be present.
	c.Assert(replica2.makeLive(), gc.IsNil)
	replica2.expectValues(map[string]string{
		"key foo": "baz",
		"key bar": "bing",
	})

	// Expect |replica1| & |replica2| share identical non-empty properties.
	c.Check(replica1.recorder.fsm.Properties, gc.Not(gc.HasLen), 0)
	c.Check(replica1.recorder.fsm.Properties, gc.DeepEquals,
		replica2.recorder.fsm.Properties)
}

func (s *RecoveryLogSuite) TestResolutionOfConflictingWriters(c *gc.C) {
	var env = testEnv{c, s.gazette}

	// Begin with two replicas, both reading from the initial state.
	var replica1 = NewTestReplica(&env)
	defer replica1.teardown()
	var replica2 = NewTestReplica(&env)
	defer replica2.teardown()

	replica1.startReading(FSMHints{Log: kTestLogName})
	replica2.startReading(FSMHints{Log: kTestLogName})

	// |replica1| begins as master.
	c.Assert(replica1.makeLive(), gc.IsNil)
	replica1.put("key one", "value one")

	// |replica2| now becomes live. |replica1| and |replica2| intersperse writes.
	c.Assert(replica2.makeLive(), gc.IsNil)
	replica1.put("rep1 foo", "value foo")
	replica2.put("rep2 bar", "value bar")
	replica1.put("rep1 baz", "value baz")
	replica2.put("rep2 bing", "value bing")

	// Ensure all writes are sync'd to the recoverylog.
	var flushOpts = rocks.NewDefaultFlushOptions()
	flushOpts.SetWait(true)
	replica1.db.Flush(flushOpts)
	replica2.db.Flush(flushOpts)
	flushOpts.Destroy()

	// New |replica3| is hinted from |replica1|, and |replica4| from |replica2|.
	var replica3 = NewTestReplica(&env)
	defer replica3.teardown()
	var replica4 = NewTestReplica(&env)
	defer replica4.teardown()

	replica3.startReading(replica1.recorder.BuildHints())
	c.Assert(replica3.makeLive(), gc.IsNil)
	replica4.startReading(replica2.recorder.BuildHints())
	c.Assert(replica4.makeLive(), gc.IsNil)

	// Expect |replica3| recovered |replica1| history.
	replica3.expectValues(map[string]string{
		"key one":  "value one",
		"rep1 foo": "value foo",
		"rep1 baz": "value baz",
	})
	// Expect |replica4| recovered |replica2| history.
	replica4.expectValues(map[string]string{
		"key one":   "value one",
		"rep2 bar":  "value bar",
		"rep2 bing": "value bing",
	})
}

func (s *RecoveryLogSuite) TestPlayThenCancel(c *gc.C) {
	var r = NewTestReplica(&testEnv{c, s.gazette})
	defer r.teardown()

	var err error
	r.player, err = NewPlayer(FSMHints{Log: kTestLogName}, r.tmpdir)
	c.Assert(err, gc.IsNil)

	// After a delay, write a frame and then Cancel.
	time.AfterFunc(blockInterval/2, func() {
		var frame, err = topic.FixedFraming.Encode(&RecordedOp{}, nil)
		c.Assert(err, gc.IsNil)

		var res = s.gazette.Put(journal.AppendArgs{
			Journal: kTestLogName,
			Content: bytes.NewReader(frame),
		})
		c.Log("Put() result: ", res)

		r.player.Cancel()
	})

	c.Check(r.player.Play(r.gazette), gc.Equals, ErrPlaybackCancelled)

	_, err = r.player.MakeLive()
	c.Check(err, gc.Equals, ErrPlaybackCancelled)

	// Expect the local directory was deleted.
	_, err = os.Stat(r.player.localDir)
	c.Check(os.IsNotExist(err), gc.Equals, true)
}

func (s *RecoveryLogSuite) TestCancelThenPlay(c *gc.C) {
	r := NewTestReplica(&testEnv{c, s.gazette})
	defer r.teardown()

	var err error
	r.player, err = NewPlayer(FSMHints{Log: kTestLogName}, r.tmpdir)
	c.Assert(err, gc.IsNil)

	r.player.Cancel()
	c.Check(r.player.Play(r.gazette), gc.Equals, ErrPlaybackCancelled)

	_, err = r.player.MakeLive()
	c.Check(err, gc.Equals, ErrPlaybackCancelled)
}

// Test state shared by multiple testReplica instances.
type testEnv struct {
	*gc.C
	gazette journal.Client
}

// Models the typical lifetime of an observed rocks database:
//  * Begin by reading from the most-recent available hints.
//  * When ready, make the database "Live".
//  * Perform new writes against the replica, which are recorded in the log.
type testReplica struct {
	*testEnv

	tmpdir string
	dbO    *rocks.Options
	dbWO   *rocks.WriteOptions
	dbRO   *rocks.ReadOptions
	db     *rocks.DB

	recorder *Recorder
	player   *Player
}

func NewTestReplica(env *testEnv) *testReplica {
	r := &testReplica{
		testEnv: env,
	}
	var err error
	r.tmpdir, err = ioutil.TempDir("", "recoverylog-suite")
	r.Assert(err, gc.IsNil)
	return r
}

func (r *testReplica) startReading(hints FSMHints) {
	var err error
	r.player, err = NewPlayer(hints, r.tmpdir)
	r.Assert(err, gc.IsNil)

	go func() {
		r.Assert(r.player.Play(r.gazette), gc.IsNil)
	}()
}

// Finish playback, build a new recorder, and open an observed database.
func (r *testReplica) makeLive() error {
	fsm, err := r.player.MakeLive()
	if err != nil {
		return err
	}
	r.Check(r.player.IsAtLogHead(), gc.Equals, true)

	r.recorder, err = NewRecorder(fsm, len(r.tmpdir), r.gazette)
	r.Assert(err, gc.IsNil)

	r.dbO = rocks.NewDefaultOptions()
	r.dbO.SetCreateIfMissing(true)
	r.dbO.SetEnv(rocks.NewObservedEnv(r.recorder))

	r.dbRO = rocks.NewDefaultReadOptions()

	r.dbWO = rocks.NewDefaultWriteOptions()
	r.dbWO.SetSync(true)

	r.db, err = rocks.OpenDb(r.dbO, r.tmpdir)
	r.Assert(err, gc.IsNil)
	return nil
}

func (r *testReplica) put(key, value string) {
	r.Check(r.db.Put(r.dbWO, []byte(key), []byte(value)), gc.IsNil)
}

func (r *testReplica) expectValues(expect map[string]string) {
	it := r.db.NewIterator(r.dbRO)
	defer it.Close()

	it.SeekToFirst()
	for ; it.Valid(); it.Next() {
		key, value := string(it.Key().Data()), string(it.Value().Data())

		r.Check(expect[key], gc.Equals, value)
		delete(expect, key)
	}
	r.Check(it.Err(), gc.IsNil)
	r.Check(expect, gc.HasLen, 0)
}

func (r *testReplica) teardown() {
	if r.db != nil {
		r.db.Close()
		r.dbRO.Destroy()
		r.dbWO.Destroy()
		r.dbO.Destroy()
	}
	r.Assert(os.RemoveAll(r.tmpdir), gc.IsNil)
}

var _ = gc.Suite(&RecoveryLogSuite{})
