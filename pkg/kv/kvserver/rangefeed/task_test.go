// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package rangefeed

import (
	"context"
	"sort"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeKV(key, val string, ts int64) storage.MVCCKeyValue {
	return storage.MVCCKeyValue{
		Key: storage.MVCCKey{
			Key:       roachpb.Key(key),
			Timestamp: hlc.Timestamp{WallTime: ts},
		},
		Value: []byte(val),
	}
}

func makeProvisionalKV(key, val string, ts int64) storage.MVCCKeyValue {
	return makeKV(key, val, ts)
}

func makeMetaKV(key string, meta enginepb.MVCCMetadata) storage.MVCCKeyValue {
	b, err := protoutil.Marshal(&meta)
	if err != nil {
		panic(err)
	}
	return storage.MVCCKeyValue{
		Key: storage.MVCCKey{
			Key: roachpb.Key(key),
		},
		Value: b,
	}
}

func makeInline(key, val string) storage.MVCCKeyValue {
	return makeMetaKV(key, enginepb.MVCCMetadata{
		RawBytes: []byte(val),
	})
}

func makeIntent(key string, txnID uuid.UUID, txnKey string, txnTS int64) storage.MVCCKeyValue {
	return makeMetaKV(key, enginepb.MVCCMetadata{
		Txn: &enginepb.TxnMeta{
			ID:             txnID,
			Key:            []byte(txnKey),
			WriteTimestamp: hlc.Timestamp{WallTime: txnTS},
			MinTimestamp:   hlc.Timestamp{WallTime: txnTS},
		},
		Timestamp: hlc.LegacyTimestamp{WallTime: txnTS},
	})
}

type testIterator struct {
	kvs        []storage.MVCCKeyValue
	cur        int
	upperBound roachpb.Key

	closed bool
	err    error
	block  chan struct{}
	done   chan struct{}
}

func newTestIterator(kvs []storage.MVCCKeyValue, upperBound roachpb.Key) *testIterator {
	// Ensure that the key-values are sorted.
	if !sort.SliceIsSorted(kvs, func(i, j int) bool {
		return kvs[i].Key.Less(kvs[j].Key)
	}) {
		panic("unsorted kvs")
	}

	// Ensure that every intent has a matching MVCCMetadata key
	// and provisional key-value pair.
	const missingErr = "missing provisional kv (makeProvisionalKV) for intent meta key (makeIntent)"
	var meta enginepb.MVCCMetadata
	for i := 0; i < len(kvs); i++ {
		kv := kvs[i]
		if !kv.Key.IsValue() {
			if err := protoutil.Unmarshal(kv.Value, &meta); err != nil {
				panic(err)
			}
			if !meta.IsInline() {
				i++
				if i == len(kvs) {
					panic(missingErr)
				}
				expNextKey := storage.MVCCKey{
					Key:       kv.Key.Key,
					Timestamp: meta.Timestamp.ToTimestamp(),
				}
				if !kvs[i].Key.Equal(expNextKey) {
					panic(missingErr)
				}
			}
		}
	}

	return &testIterator{
		kvs:        kvs,
		cur:        -1,
		done:       make(chan struct{}),
		upperBound: upperBound,
	}
}

func (s *testIterator) Close() {
	s.closed = true
	close(s.done)
}

func (s *testIterator) SeekGE(key storage.MVCCKey) {
	if s.closed {
		panic("testIterator closed")
	}
	if s.block != nil {
		<-s.block
	}
	if s.err != nil {
		return
	}
	if s.cur == -1 {
		s.cur++
	}
	for ; s.cur < len(s.kvs); s.cur++ {
		if !s.curKV().Key.Less(key) {
			break
		}
	}
}

func (s *testIterator) Valid() (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if s.cur == -1 || s.cur >= len(s.kvs) {
		return false, nil
	}
	if s.upperBound != nil && !s.curKV().Key.Less(storage.MVCCKey{Key: s.upperBound}) {
		return false, nil
	}
	return true, nil
}

func (s *testIterator) Next() { s.cur++ }

func (s *testIterator) NextKey() {
	if s.cur == -1 {
		s.cur = 0
		return
	}
	origKey := s.curKV().Key.Key
	for s.cur++; s.cur < len(s.kvs); s.cur++ {
		if !s.curKV().Key.Key.Equal(origKey) {
			break
		}
	}
}

func (s *testIterator) UnsafeKey() storage.MVCCKey {
	return s.curKV().Key
}

func (s *testIterator) UnsafeValue() []byte {
	return s.curKV().Value
}

func (s *testIterator) curKV() storage.MVCCKeyValue {
	return s.kvs[s.cur]
}

func TestInitResolvedTSScan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	startKey := roachpb.RKey("d")
	endKey := roachpb.RKey("w")

	makeTxn := func(key string, id uuid.UUID, ts hlc.Timestamp) roachpb.Transaction {
		txnMeta := enginepb.TxnMeta{
			Key:            []byte(key),
			ID:             id,
			Epoch:          1,
			WriteTimestamp: ts,
			MinTimestamp:   ts,
		}
		return roachpb.Transaction{
			TxnMeta:       txnMeta,
			ReadTimestamp: ts,
		}
	}

	txn1ID := uuid.MakeV4()
	txn1TS := hlc.Timestamp{WallTime: 15}
	txn1Key := "txnKey1"
	txn1 := makeTxn(txn1Key, txn1ID, txn1TS)

	txn2ID := uuid.MakeV4()
	txn2TS := hlc.Timestamp{WallTime: 21}
	txn2Key := "txnKey2"
	txn2 := makeTxn(txn2Key, txn2ID, txn2TS)

	type op struct {
		kv  storage.MVCCKeyValue
		txn *roachpb.Transaction
	}

	makeEngine := func() storage.Engine {
		ctx := context.Background()
		engine := storage.NewDefaultInMemForTesting()
		testData := []op{
			{kv: makeKV("a", "val1", 10)},
			{kv: makeInline("b", "val2")},
			{kv: makeKV("c", "val4", 9)},
			{kv: makeKV("c", "val3", 11)},
			{
				txn: &txn1,
				kv:  makeProvisionalKV("c", "txnKey1", 15),
			},
			{kv: makeKV("d", "val6", 19)},
			{kv: makeKV("d", "val5", 20)},
			{
				txn: &txn2,
				kv:  makeProvisionalKV("d", "txnKey2", 21),
			},
			{kv: makeInline("g", "val7")},
			{kv: makeKV("m", "val8", 1)},
			{
				txn: &txn1,
				kv:  makeProvisionalKV("n", "txnKey1", 15),
			},
			{kv: makeKV("r", "val9", 4)},
			{
				txn: &txn1,
				kv:  makeProvisionalKV("r", "txnKey1", 15),
			},
			{
				txn: &txn1,
				kv:  makeProvisionalKV("w", "txnKey1", 15),
			},
			{kv: makeInline("x", "val10")},
			{kv: makeKV("z", "val11", 4)},
			{
				txn: &txn2,
				kv:  makeProvisionalKV("z", "txnKey2", 21),
			},
		}
		for _, op := range testData {
			kv := op.kv
			err := storage.MVCCPut(ctx, engine, nil, kv.Key.Key, kv.Key.Timestamp, roachpb.Value{RawBytes: kv.Value}, op.txn)
			require.NoError(t, err)
		}
		return engine
	}

	expEvents := []*event{
		{ops: []enginepb.MVCCLogicalOp{
			writeIntentOpWithKey(txn2ID, []byte("txnKey2"), hlc.Timestamp{WallTime: 21}),
		}},
		{ops: []enginepb.MVCCLogicalOp{
			writeIntentOpWithKey(txn1ID, []byte("txnKey1"), hlc.Timestamp{WallTime: 15}),
		}},
		{ops: []enginepb.MVCCLogicalOp{
			writeIntentOpWithKey(txn1ID, []byte("txnKey1"), hlc.Timestamp{WallTime: 15}),
		}},
		{initRTS: true},
	}

	testCases := map[string]struct {
		intentScanner func() (IntentScanner, func())
	}{
		"legacy intent scanner": {
			intentScanner: func() (IntentScanner, func()) {
				engine := makeEngine()
				iter := engine.NewMVCCIterator(storage.MVCCKeyAndIntentsIterKind, storage.IterOptions{
					UpperBound: endKey.AsRawKey(),
				})
				return NewLegacyIntentScanner(iter), func() { engine.Close() }
			},
		},
		"separated intent scanner": {
			intentScanner: func() (IntentScanner, func()) {
				engine := makeEngine()
				lowerBound, _ := keys.LockTableSingleKey(startKey.AsRawKey(), nil)
				upperBound, _ := keys.LockTableSingleKey(endKey.AsRawKey(), nil)
				iter := engine.NewEngineIterator(storage.IterOptions{
					LowerBound: lowerBound,
					UpperBound: upperBound,
				})
				return NewSeparatedIntentScanner(iter), func() { engine.Close() }
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			// Mock processor. We just needs its eventC.
			p := Processor{
				Config: Config{
					Span: roachpb.RSpan{
						Key:    startKey,
						EndKey: endKey,
					},
				},
				eventC: make(chan *event, 100),
			}
			isc, cleanup := tc.intentScanner()
			defer cleanup()
			initScan := newInitResolvedTSScan(&p, isc)
			initScan.Run(context.Background())
			// Compare the event channel to the expected events.
			assert.Equal(t, len(expEvents), len(p.eventC))
			for _, expEvent := range expEvents {
				assert.Equal(t, expEvent, <-p.eventC)
			}

		})
	}
}

type testTxnPusher struct {
	pushTxnsFn       func([]enginepb.TxnMeta, hlc.Timestamp) ([]*roachpb.Transaction, error)
	resolveIntentsFn func(ctx context.Context, intents []roachpb.LockUpdate) error
}

func (tp *testTxnPusher) PushTxns(
	ctx context.Context, txns []enginepb.TxnMeta, ts hlc.Timestamp,
) ([]*roachpb.Transaction, error) {
	return tp.pushTxnsFn(txns, ts)
}

func (tp *testTxnPusher) ResolveIntents(ctx context.Context, intents []roachpb.LockUpdate) error {
	return tp.resolveIntentsFn(ctx, intents)
}

func (tp *testTxnPusher) mockPushTxns(
	fn func([]enginepb.TxnMeta, hlc.Timestamp) ([]*roachpb.Transaction, error),
) {
	tp.pushTxnsFn = fn
}

func (tp *testTxnPusher) mockResolveIntentsFn(
	fn func(context.Context, []roachpb.LockUpdate) error,
) {
	tp.resolveIntentsFn = fn
}

func (tp *testTxnPusher) intentsToTxns(intents []roachpb.LockUpdate) []enginepb.TxnMeta {
	txns := make([]enginepb.TxnMeta, 0)
	txnIDs := make(map[uuid.UUID]struct{})
	for _, intent := range intents {
		txn := intent.Txn
		if _, ok := txnIDs[txn.ID]; ok {
			continue
		}
		txns = append(txns, txn)
		txnIDs[txn.ID] = struct{}{}
	}
	return txns
}

func TestTxnPushAttempt(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// Create a set of transactions.
	txn1, txn2, txn3, txn4 := uuid.MakeV4(), uuid.MakeV4(), uuid.MakeV4(), uuid.MakeV4()
	ts1, ts2, ts3, ts4 := hlc.Timestamp{WallTime: 1}, hlc.Timestamp{WallTime: 2}, hlc.Timestamp{WallTime: 3}, hlc.Timestamp{WallTime: 4}
	txn2LockSpans := []roachpb.Span{
		{Key: roachpb.Key("b"), EndKey: roachpb.Key("c")},
		{Key: roachpb.Key("d"), EndKey: roachpb.Key("e")},
		{Key: roachpb.Key("y"), EndKey: roachpb.Key("z")}, // ignored
	}
	txn4LockSpans := []roachpb.Span{
		{Key: roachpb.Key("f"), EndKey: roachpb.Key("g")},
		{Key: roachpb.Key("h"), EndKey: roachpb.Key("i")},
		{Key: roachpb.Key("a"), EndKey: roachpb.Key("d")}, // truncated at beginning
		{Key: roachpb.Key("j"), EndKey: roachpb.Key("q")}, // truncated at end
		{Key: roachpb.Key("a"), EndKey: roachpb.Key("z")}, // truncated at beginning and end
	}
	txn1Meta := enginepb.TxnMeta{ID: txn1, Key: keyA, WriteTimestamp: ts1, MinTimestamp: ts1}
	txn2Meta := enginepb.TxnMeta{ID: txn2, Key: keyB, WriteTimestamp: ts2, MinTimestamp: ts2}
	txn3Meta := enginepb.TxnMeta{ID: txn3, Key: keyC, WriteTimestamp: ts3, MinTimestamp: ts3}
	txn4Meta := enginepb.TxnMeta{ID: txn4, Key: keyC, WriteTimestamp: ts3, MinTimestamp: ts4}
	txn1Proto := &roachpb.Transaction{TxnMeta: txn1Meta, Status: roachpb.PENDING}
	txn2Proto := &roachpb.Transaction{TxnMeta: txn2Meta, Status: roachpb.COMMITTED, LockSpans: txn2LockSpans}
	txn3Proto := &roachpb.Transaction{TxnMeta: txn3Meta, Status: roachpb.ABORTED}
	// txn4 has its LockSpans populated, simulated a transaction that has been
	// rolled back by its coordinator (which populated the LockSpans), but then
	// not GC'ed for whatever reason.
	txn4Proto := &roachpb.Transaction{TxnMeta: txn4Meta, Status: roachpb.ABORTED, LockSpans: txn4LockSpans}

	// Run a txnPushAttempt.
	var tp testTxnPusher
	tp.mockPushTxns(func(txns []enginepb.TxnMeta, ts hlc.Timestamp) ([]*roachpb.Transaction, error) {
		require.Equal(t, 4, len(txns))
		require.Equal(t, txn1Meta, txns[0])
		require.Equal(t, txn2Meta, txns[1])
		require.Equal(t, txn3Meta, txns[2])
		require.Equal(t, txn4Meta, txns[3])
		require.Equal(t, hlc.Timestamp{WallTime: 15}, ts)

		// Return all four protos. The PENDING txn is pushed.
		txn1ProtoPushed := txn1Proto.Clone()
		txn1ProtoPushed.WriteTimestamp = ts
		return []*roachpb.Transaction{txn1ProtoPushed, txn2Proto, txn3Proto, txn4Proto}, nil
	})
	tp.mockResolveIntentsFn(func(ctx context.Context, intents []roachpb.LockUpdate) error {
		require.Len(t, intents, 7)
		require.Equal(t, txn2LockSpans[0], intents[0].Span)
		require.Equal(t, txn2LockSpans[1], intents[1].Span)
		require.Equal(t, txn4LockSpans[0], intents[2].Span)
		require.Equal(t, txn4LockSpans[1], intents[3].Span)
		require.Equal(t, func() roachpb.Span {
			s := txn4LockSpans[2] // truncated at beginning
			s.Key = roachpb.Key("b")
			return s
		}(), intents[4].Span)
		require.Equal(t, func() roachpb.Span {
			s := txn4LockSpans[3] // truncated at end
			s.EndKey = roachpb.Key("m")
			return s
		}(), intents[5].Span)
		require.Equal(t, func() roachpb.Span {
			s := txn4LockSpans[4] // truncated at beginning and end
			s.Key = roachpb.Key("b")
			s.EndKey = roachpb.Key("m")
			return s
		}(), intents[6].Span)
		txns := tp.intentsToTxns(intents)
		require.Equal(t, 2, len(txns))
		require.Equal(t, txn2Meta, txns[0])
		// Note that we don't expect intents for txn3 to be resolved since that txn
		// doesn't have its LockSpans populated.
		require.Equal(t, txn4Meta, txns[1])
		return nil
	})

	// Mock processor. We configure its key span to exclude one of txn2's lock
	// spans and a portion of three of txn4's lock spans.
	p := Processor{eventC: make(chan *event, 100)}
	p.Span = roachpb.RSpan{Key: roachpb.RKey("b"), EndKey: roachpb.RKey("m")}
	p.TxnPusher = &tp

	txns := []enginepb.TxnMeta{txn1Meta, txn2Meta, txn3Meta, txn4Meta}
	doneC := make(chan struct{})
	pushAttempt := newTxnPushAttempt(&p, txns, hlc.Timestamp{WallTime: 15}, doneC)
	pushAttempt.Run(context.Background())
	<-doneC // check if closed

	// Compare the event channel to the expected events.
	expEvents := []*event{
		{ops: []enginepb.MVCCLogicalOp{
			updateIntentOp(txn1, hlc.Timestamp{WallTime: 15}),
			updateIntentOp(txn2, hlc.Timestamp{WallTime: 2}),
			abortTxnOp(txn3),
			abortTxnOp(txn4),
		}},
	}
	require.Equal(t, len(expEvents), len(p.eventC))
	for _, expEvent := range expEvents {
		require.Equal(t, expEvent, <-p.eventC)
	}
}
