// Copyright 2016 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package integration

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/coreos/etcd/etcdserver/api/v3rpc"
	"github.com/coreos/etcd/etcdserver/api/v3rpc/rpctypes"
	pb "github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/pkg/testutil"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// TestV3PutOverwrite puts a key with the v3 api to a random cluster member,
// overwrites it, then checks that the change was applied.
func TestV3PutOverwrite(t *testing.T) {
	defer testutil.AfterTest(t)
	clus := NewClusterV3(t, &ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	kvc := toGRPC(clus.RandClient()).KV
	key := []byte("foo")
	reqput := &pb.PutRequest{Key: key, Value: []byte("bar")}

	respput, err := kvc.Put(context.TODO(), reqput)
	if err != nil {
		t.Fatalf("couldn't put key (%v)", err)
	}

	// overwrite
	reqput.Value = []byte("baz")
	respput2, err := kvc.Put(context.TODO(), reqput)
	if err != nil {
		t.Fatalf("couldn't put key (%v)", err)
	}
	if respput2.Header.Revision <= respput.Header.Revision {
		t.Fatalf("expected newer revision on overwrite, got %v <= %v",
			respput2.Header.Revision, respput.Header.Revision)
	}

	reqrange := &pb.RangeRequest{Key: key}
	resprange, err := kvc.Range(context.TODO(), reqrange)
	if err != nil {
		t.Fatalf("couldn't get key (%v)", err)
	}
	if len(resprange.Kvs) != 1 {
		t.Fatalf("expected 1 key, got %v", len(resprange.Kvs))
	}

	kv := resprange.Kvs[0]
	if kv.ModRevision <= kv.CreateRevision {
		t.Errorf("expected modRev > createRev, got %d <= %d",
			kv.ModRevision, kv.CreateRevision)
	}
	if !reflect.DeepEqual(reqput.Value, kv.Value) {
		t.Errorf("expected value %v, got %v", reqput.Value, kv.Value)
	}
}

func TestV3TxnTooManyOps(t *testing.T) {
	defer testutil.AfterTest(t)
	clus := NewClusterV3(t, &ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	kvc := toGRPC(clus.RandClient()).KV

	// unique keys
	i := new(int)
	keyf := func() []byte {
		*i++
		return []byte(fmt.Sprintf("key-%d", i))
	}

	addCompareOps := func(txn *pb.TxnRequest) {
		txn.Compare = append(txn.Compare,
			&pb.Compare{
				Result: pb.Compare_GREATER,
				Target: pb.Compare_CREATE,
				Key:    keyf(),
			})
	}
	addSuccessOps := func(txn *pb.TxnRequest) {
		txn.Success = append(txn.Success,
			&pb.RequestUnion{
				Request: &pb.RequestUnion_RequestPut{
					RequestPut: &pb.PutRequest{
						Key:   keyf(),
						Value: []byte("bar"),
					},
				},
			})
	}
	addFailureOps := func(txn *pb.TxnRequest) {
		txn.Failure = append(txn.Failure,
			&pb.RequestUnion{
				Request: &pb.RequestUnion_RequestPut{
					RequestPut: &pb.PutRequest{
						Key:   keyf(),
						Value: []byte("bar"),
					},
				},
			})
	}

	tests := []func(txn *pb.TxnRequest){
		addCompareOps,
		addSuccessOps,
		addFailureOps,
	}

	for i, tt := range tests {
		txn := &pb.TxnRequest{}
		for j := 0; j < v3rpc.MaxOpsPerTxn+1; j++ {
			tt(txn)
		}

		_, err := kvc.Txn(context.Background(), txn)
		if err != rpctypes.ErrTooManyOps {
			t.Errorf("#%d: err = %v, want %v", i, err, rpctypes.ErrTooManyOps)
		}
	}
}

func TestV3TxnDuplicateKeys(t *testing.T) {
	defer testutil.AfterTest(t)
	clus := NewClusterV3(t, &ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	putreq := &pb.RequestUnion{Request: &pb.RequestUnion_RequestPut{RequestPut: &pb.PutRequest{Key: []byte("abc"), Value: []byte("def")}}}
	delKeyReq := &pb.RequestUnion{Request: &pb.RequestUnion_RequestDeleteRange{
		RequestDeleteRange: &pb.DeleteRangeRequest{
			Key: []byte("abc"),
		},
	},
	}
	delInRangeReq := &pb.RequestUnion{Request: &pb.RequestUnion_RequestDeleteRange{
		RequestDeleteRange: &pb.DeleteRangeRequest{
			Key: []byte("a"), RangeEnd: []byte("b"),
		},
	},
	}
	delOutOfRangeReq := &pb.RequestUnion{Request: &pb.RequestUnion_RequestDeleteRange{
		RequestDeleteRange: &pb.DeleteRangeRequest{
			Key: []byte("abb"), RangeEnd: []byte("abc"),
		},
	},
	}

	kvc := toGRPC(clus.RandClient()).KV
	tests := []struct {
		txnSuccess []*pb.RequestUnion

		werr error
	}{
		{
			txnSuccess: []*pb.RequestUnion{putreq, putreq},

			werr: rpctypes.ErrDuplicateKey,
		},
		{
			txnSuccess: []*pb.RequestUnion{putreq, delKeyReq},

			werr: rpctypes.ErrDuplicateKey,
		},
		{
			txnSuccess: []*pb.RequestUnion{putreq, delInRangeReq},

			werr: rpctypes.ErrDuplicateKey,
		},
		{
			txnSuccess: []*pb.RequestUnion{delKeyReq, delInRangeReq, delKeyReq, delInRangeReq},

			werr: nil,
		},
		{
			txnSuccess: []*pb.RequestUnion{putreq, delOutOfRangeReq},

			werr: nil,
		},
	}
	for i, tt := range tests {
		txn := &pb.TxnRequest{Success: tt.txnSuccess}
		_, err := kvc.Txn(context.Background(), txn)
		if err != tt.werr {
			t.Errorf("#%d: err = %v, want %v", i, err, tt.werr)
		}
	}
}

// Testv3TxnRevision tests that the transaction header revision is set as expected.
func TestV3TxnRevision(t *testing.T) {
	defer testutil.AfterTest(t)
	clus := NewClusterV3(t, &ClusterConfig{Size: 1})
	defer clus.Terminate(t)

	kvc := toGRPC(clus.RandClient()).KV
	pr := &pb.PutRequest{Key: []byte("abc"), Value: []byte("def")}
	presp, err := kvc.Put(context.TODO(), pr)
	if err != nil {
		t.Fatal(err)
	}

	txnget := &pb.RequestUnion{Request: &pb.RequestUnion_RequestRange{RequestRange: &pb.RangeRequest{Key: []byte("abc")}}}
	txn := &pb.TxnRequest{Success: []*pb.RequestUnion{txnget}}
	tresp, err := kvc.Txn(context.TODO(), txn)
	if err != nil {
		t.Fatal(err)
	}

	// did not update revision
	if presp.Header.Revision != tresp.Header.Revision {
		t.Fatalf("got rev %d, wanted rev %d", tresp.Header.Revision, presp.Header.Revision)
	}

	txnput := &pb.RequestUnion{Request: &pb.RequestUnion_RequestPut{RequestPut: &pb.PutRequest{Key: []byte("abc"), Value: []byte("123")}}}
	txn = &pb.TxnRequest{Success: []*pb.RequestUnion{txnput}}
	tresp, err = kvc.Txn(context.TODO(), txn)
	if err != nil {
		t.Fatal(err)
	}

	// updated revision
	if tresp.Header.Revision != presp.Header.Revision+1 {
		t.Fatalf("got rev %d, wanted rev %d", tresp.Header.Revision, presp.Header.Revision+1)
	}
}

// TestV3PutMissingLease ensures that a Put on a key with a bogus lease fails.
func TestV3PutMissingLease(t *testing.T) {
	defer testutil.AfterTest(t)
	clus := NewClusterV3(t, &ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	kvc := toGRPC(clus.RandClient()).KV
	key := []byte("foo")
	preq := &pb.PutRequest{Key: key, Lease: 123456}
	tests := []func(){
		// put case
		func() {
			if presp, err := kvc.Put(context.TODO(), preq); err == nil {
				t.Errorf("succeeded put key. req: %v. resp: %v", preq, presp)
			}
		},
		// txn success case
		func() {
			txn := &pb.TxnRequest{}
			txn.Success = append(txn.Success, &pb.RequestUnion{
				Request: &pb.RequestUnion_RequestPut{
					RequestPut: preq}})
			if tresp, err := kvc.Txn(context.TODO(), txn); err == nil {
				t.Errorf("succeeded txn success. req: %v. resp: %v", txn, tresp)
			}
		},
		// txn failure case
		func() {
			txn := &pb.TxnRequest{}
			txn.Failure = append(txn.Failure, &pb.RequestUnion{
				Request: &pb.RequestUnion_RequestPut{
					RequestPut: preq}})
			cmp := &pb.Compare{
				Result: pb.Compare_GREATER,
				Target: pb.Compare_CREATE,
				Key:    []byte("bar"),
			}
			txn.Compare = append(txn.Compare, cmp)
			if tresp, err := kvc.Txn(context.TODO(), txn); err == nil {
				t.Errorf("succeeded txn failure. req: %v. resp: %v", txn, tresp)
			}
		},
		// ignore bad lease in failure on success txn
		func() {
			txn := &pb.TxnRequest{}
			rreq := &pb.RangeRequest{Key: []byte("bar")}
			txn.Success = append(txn.Success, &pb.RequestUnion{
				Request: &pb.RequestUnion_RequestRange{
					RequestRange: rreq}})
			txn.Failure = append(txn.Failure, &pb.RequestUnion{
				Request: &pb.RequestUnion_RequestPut{
					RequestPut: preq}})
			if tresp, err := kvc.Txn(context.TODO(), txn); err != nil {
				t.Errorf("failed good txn. req: %v. resp: %v", txn, tresp)
			}
		},
	}

	for i, f := range tests {
		f()
		// key shouldn't have been stored
		rreq := &pb.RangeRequest{Key: key}
		rresp, err := kvc.Range(context.TODO(), rreq)
		if err != nil {
			t.Errorf("#%d. could not rangereq (%v)", i, err)
		} else if len(rresp.Kvs) != 0 {
			t.Errorf("#%d. expected no keys, got %v", i, rresp)
		}
	}
}

// TestV3DeleteRange tests various edge cases in the DeleteRange API.
func TestV3DeleteRange(t *testing.T) {
	defer testutil.AfterTest(t)
	tests := []struct {
		keySet []string
		begin  string
		end    string

		wantSet [][]byte
		deleted int64
	}{
		// delete middle
		{
			[]string{"foo", "foo/abc", "fop"},
			"foo/", "fop",
			[][]byte{[]byte("foo"), []byte("fop")}, 1,
		},
		// no delete
		{
			[]string{"foo", "foo/abc", "fop"},
			"foo/", "foo/",
			[][]byte{[]byte("foo"), []byte("foo/abc"), []byte("fop")}, 0,
		},
		// delete first
		{
			[]string{"foo", "foo/abc", "fop"},
			"fo", "fop",
			[][]byte{[]byte("fop")}, 2,
		},
		// delete tail
		{
			[]string{"foo", "foo/abc", "fop"},
			"foo/", "fos",
			[][]byte{[]byte("foo")}, 2,
		},
		// delete exact
		{
			[]string{"foo", "foo/abc", "fop"},
			"foo/abc", "",
			[][]byte{[]byte("foo"), []byte("fop")}, 1,
		},
		// delete none, [x,x)
		{
			[]string{"foo"},
			"foo", "foo",
			[][]byte{[]byte("foo")}, 0,
		},
	}

	for i, tt := range tests {
		clus := NewClusterV3(t, &ClusterConfig{Size: 3})
		kvc := toGRPC(clus.RandClient()).KV

		ks := tt.keySet
		for j := range ks {
			reqput := &pb.PutRequest{Key: []byte(ks[j]), Value: []byte{}}
			_, err := kvc.Put(context.TODO(), reqput)
			if err != nil {
				t.Fatalf("couldn't put key (%v)", err)
			}
		}

		dreq := &pb.DeleteRangeRequest{
			Key:      []byte(tt.begin),
			RangeEnd: []byte(tt.end)}
		dresp, err := kvc.DeleteRange(context.TODO(), dreq)
		if err != nil {
			t.Fatalf("couldn't delete range on test %d (%v)", i, err)
		}
		if tt.deleted != dresp.Deleted {
			t.Errorf("expected %d on test %v, got %d", tt.deleted, i, dresp.Deleted)
		}

		rreq := &pb.RangeRequest{Key: []byte{0x0}, RangeEnd: []byte{0xff}}
		rresp, err := kvc.Range(context.TODO(), rreq)
		if err != nil {
			t.Errorf("couldn't get range on test %v (%v)", i, err)
		}
		if dresp.Header.Revision != rresp.Header.Revision {
			t.Errorf("expected revision %v, got %v",
				dresp.Header.Revision, rresp.Header.Revision)
		}

		keys := [][]byte{}
		for j := range rresp.Kvs {
			keys = append(keys, rresp.Kvs[j].Key)
		}
		if !reflect.DeepEqual(tt.wantSet, keys) {
			t.Errorf("expected %v on test %v, got %v", tt.wantSet, i, keys)
		}

		// can't defer because tcp ports will be in use
		clus.Terminate(t)
	}
}

// TestV3TxnInvaildRange tests txn
func TestV3TxnInvaildRange(t *testing.T) {
	defer testutil.AfterTest(t)
	clus := NewClusterV3(t, &ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	kvc := toGRPC(clus.RandClient()).KV
	preq := &pb.PutRequest{Key: []byte("foo"), Value: []byte("bar")}

	for i := 0; i < 3; i++ {
		_, err := kvc.Put(context.Background(), preq)
		if err != nil {
			t.Fatalf("couldn't put key (%v)", err)
		}
	}

	_, err := kvc.Compact(context.Background(), &pb.CompactionRequest{Revision: 2})
	if err != nil {
		t.Fatalf("couldn't compact kv space (%v)", err)
	}

	// future rev
	txn := &pb.TxnRequest{}
	txn.Success = append(txn.Success, &pb.RequestUnion{
		Request: &pb.RequestUnion_RequestPut{
			RequestPut: preq}})

	rreq := &pb.RangeRequest{Key: []byte("foo"), Revision: 100}
	txn.Success = append(txn.Success, &pb.RequestUnion{
		Request: &pb.RequestUnion_RequestRange{
			RequestRange: rreq}})

	if _, err := kvc.Txn(context.TODO(), txn); err != rpctypes.ErrFutureRev {
		t.Errorf("err = %v, want %v", err, rpctypes.ErrFutureRev)
	}

	// compacted rev
	tv, _ := txn.Success[1].Request.(*pb.RequestUnion_RequestRange)
	tv.RequestRange.Revision = 1
	if _, err := kvc.Txn(context.TODO(), txn); err != rpctypes.ErrCompacted {
		t.Errorf("err = %v, want %v", err, rpctypes.ErrCompacted)
	}
}

func TestV3TooLargeRequest(t *testing.T) {
	defer testutil.AfterTest(t)

	clus := NewClusterV3(t, &ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	kvc := toGRPC(clus.RandClient()).KV

	// 2MB request value
	largeV := make([]byte, 2*1024*1024)
	preq := &pb.PutRequest{Key: []byte("foo"), Value: largeV}

	_, err := kvc.Put(context.Background(), preq)
	if err != rpctypes.ErrRequestTooLarge {
		t.Errorf("err = %v, want %v", err, rpctypes.ErrRequestTooLarge)
	}
}

// TestV3Hash tests hash.
func TestV3Hash(t *testing.T) {
	defer testutil.AfterTest(t)
	clus := NewClusterV3(t, &ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	cli := clus.RandClient()
	kvc := toGRPC(cli).KV
	m := toGRPC(cli).Maintenance

	preq := &pb.PutRequest{Key: []byte("foo"), Value: []byte("bar")}

	for i := 0; i < 3; i++ {
		_, err := kvc.Put(context.Background(), preq)
		if err != nil {
			t.Fatalf("couldn't put key (%v)", err)
		}
	}

	resp, err := m.Hash(context.Background(), &pb.HashRequest{})
	if err != nil || resp.Hash == 0 {
		t.Fatalf("couldn't hash (%v, hash %d)", err, resp.Hash)
	}
}

// TestV3StorageQuotaAPI tests the V3 server respects quotas at the API layer
func TestV3StorageQuotaAPI(t *testing.T) {
	defer testutil.AfterTest(t)

	clus := NewClusterV3(t, &ClusterConfig{Size: 3})

	clus.Members[0].QuotaBackendBytes = 64 * 1024
	clus.Members[0].Stop(t)
	clus.Members[0].Restart(t)

	defer clus.Terminate(t)
	kvc := toGRPC(clus.Client(0)).KV

	key := []byte("abc")

	// test small put that fits in quota
	smallbuf := make([]byte, 512)
	if _, err := kvc.Put(context.TODO(), &pb.PutRequest{Key: key, Value: smallbuf}); err != nil {
		t.Fatal(err)
	}

	// test big put
	bigbuf := make([]byte, 64*1024)
	_, err := kvc.Put(context.TODO(), &pb.PutRequest{Key: key, Value: bigbuf})
	if err == nil || err != rpctypes.ErrNoSpace {
		t.Fatalf("big put got %v, expected %v", err, rpctypes.ErrNoSpace)
	}

	// test big txn
	puttxn := &pb.RequestUnion{
		Request: &pb.RequestUnion_RequestPut{
			RequestPut: &pb.PutRequest{
				Key:   key,
				Value: bigbuf,
			},
		},
	}
	txnreq := &pb.TxnRequest{}
	txnreq.Success = append(txnreq.Success, puttxn)
	_, txnerr := kvc.Txn(context.TODO(), txnreq)
	if txnerr == nil || err != rpctypes.ErrNoSpace {
		t.Fatalf("big txn got %v, expected %v", err, rpctypes.ErrNoSpace)
	}
}

// TestV3StorageQuotaApply tests the V3 server respects quotas during apply
func TestV3StorageQuotaApply(t *testing.T) {
	testutil.AfterTest(t)

	clus := NewClusterV3(t, &ClusterConfig{Size: 2})
	defer clus.Terminate(t)
	kvc0 := toGRPC(clus.Client(0)).KV
	kvc1 := toGRPC(clus.Client(1)).KV

	// force a node to have a different quota
	clus.Members[0].QuotaBackendBytes = 64 * 1024
	clus.Members[0].Stop(t)
	clus.Members[0].Restart(t)
	clus.waitLeader(t, clus.Members)

	key := []byte("abc")

	// test small put still works
	smallbuf := make([]byte, 1024)
	_, serr := kvc0.Put(context.TODO(), &pb.PutRequest{Key: key, Value: smallbuf})
	if serr != nil {
		t.Fatal(serr)
	}

	// test big put
	bigbuf := make([]byte, 64*1024)
	_, err := kvc1.Put(context.TODO(), &pb.PutRequest{Key: key, Value: bigbuf})
	if err != nil {
		t.Fatal(err)
	}

	// small quota machine should reject put
	// first, synchronize with the cluster via quorum get
	kvc0.Range(context.TODO(), &pb.RangeRequest{Key: []byte("foo")})
	if _, err := kvc0.Put(context.TODO(), &pb.PutRequest{Key: key, Value: smallbuf}); err == nil {
		t.Fatalf("past-quota instance should reject put")
	}

	// large quota machine should reject put
	if _, err := kvc1.Put(context.TODO(), &pb.PutRequest{Key: key, Value: smallbuf}); err == nil {
		t.Fatalf("past-quota instance should reject put")
	}

	// reset large quota node to ensure alarm persisted
	clus.Members[1].Stop(t)
	clus.Members[1].Restart(t)
	clus.waitLeader(t, clus.Members)

	if _, err := kvc1.Put(context.TODO(), &pb.PutRequest{Key: key, Value: smallbuf}); err == nil {
		t.Fatalf("alarmed instance should reject put after reset")
	}
}

// TestV3AlarmDeactivate ensures that space alarms can be deactivated so puts go through.
func TestV3AlarmDeactivate(t *testing.T) {
	clus := NewClusterV3(t, &ClusterConfig{Size: 3})
	defer clus.Terminate(t)
	kvc := toGRPC(clus.RandClient()).KV
	mt := toGRPC(clus.RandClient()).Maintenance

	alarmReq := &pb.AlarmRequest{
		MemberID: 123,
		Action:   pb.AlarmRequest_ACTIVATE,
		Alarm:    pb.AlarmType_NOSPACE,
	}
	if _, err := mt.Alarm(context.TODO(), alarmReq); err != nil {
		t.Fatal(err)
	}

	key := []byte("abc")
	smallbuf := make([]byte, 512)
	_, err := kvc.Put(context.TODO(), &pb.PutRequest{Key: key, Value: smallbuf})
	if err == nil && err != rpctypes.ErrNoSpace {
		t.Fatalf("put got %v, expected %v", err, rpctypes.ErrNoSpace)
	}

	alarmReq.Action = pb.AlarmRequest_DEACTIVATE
	if _, err = mt.Alarm(context.TODO(), alarmReq); err != nil {
		t.Fatal(err)
	}

	if _, err = kvc.Put(context.TODO(), &pb.PutRequest{Key: key, Value: smallbuf}); err != nil {
		t.Fatal(err)
	}
}

func TestV3RangeRequest(t *testing.T) {
	defer testutil.AfterTest(t)
	tests := []struct {
		putKeys []string
		reqs    []pb.RangeRequest

		wresps [][]string
		wmores []bool
	}{
		// single key
		{
			[]string{"foo", "bar"},
			[]pb.RangeRequest{
				// exists
				{Key: []byte("foo")},
				// doesn't exist
				{Key: []byte("baz")},
			},

			[][]string{
				{"foo"},
				{},
			},
			[]bool{false, false},
		},
		// multi-key
		{
			[]string{"a", "b", "c", "d", "e"},
			[]pb.RangeRequest{
				// all in range
				{Key: []byte("a"), RangeEnd: []byte("z")},
				// [b, d)
				{Key: []byte("b"), RangeEnd: []byte("d")},
				// out of range
				{Key: []byte("f"), RangeEnd: []byte("z")},
				// [c,c) = empty
				{Key: []byte("c"), RangeEnd: []byte("c")},
				// [d, b) = empty
				{Key: []byte("d"), RangeEnd: []byte("b")},
				// ["\0", "\0") => all in range
				{Key: []byte{0}, RangeEnd: []byte{0}},
			},

			[][]string{
				{"a", "b", "c", "d", "e"},
				{"b", "c"},
				{},
				{},
				{},
				{"a", "b", "c", "d", "e"},
			},
			[]bool{false, false, false, false, false, false},
		},
		// revision
		{
			[]string{"a", "b", "c", "d", "e"},
			[]pb.RangeRequest{
				{Key: []byte("a"), RangeEnd: []byte("z"), Revision: 0},
				{Key: []byte("a"), RangeEnd: []byte("z"), Revision: 1},
				{Key: []byte("a"), RangeEnd: []byte("z"), Revision: 2},
				{Key: []byte("a"), RangeEnd: []byte("z"), Revision: 3},
			},

			[][]string{
				{"a", "b", "c", "d", "e"},
				{},
				{"a"},
				{"a", "b"},
			},
			[]bool{false, false, false, false},
		},
		// limit
		{
			[]string{"foo", "bar"},
			[]pb.RangeRequest{
				// more
				{Key: []byte("a"), RangeEnd: []byte("z"), Limit: 1},
				// no more
				{Key: []byte("a"), RangeEnd: []byte("z"), Limit: 2},
			},

			[][]string{
				{"bar"},
				{"bar", "foo"},
			},
			[]bool{true, false},
		},
		// sort
		{
			[]string{"b", "a", "c", "d", "c"},
			[]pb.RangeRequest{
				{
					Key: []byte("a"), RangeEnd: []byte("z"),
					Limit:      1,
					SortOrder:  pb.RangeRequest_ASCEND,
					SortTarget: pb.RangeRequest_KEY,
				},
				{
					Key: []byte("a"), RangeEnd: []byte("z"),
					Limit:      1,
					SortOrder:  pb.RangeRequest_DESCEND,
					SortTarget: pb.RangeRequest_KEY,
				},
				{
					Key: []byte("a"), RangeEnd: []byte("z"),
					Limit:      1,
					SortOrder:  pb.RangeRequest_ASCEND,
					SortTarget: pb.RangeRequest_CREATE,
				},
				{
					Key: []byte("a"), RangeEnd: []byte("z"),
					Limit:      1,
					SortOrder:  pb.RangeRequest_DESCEND,
					SortTarget: pb.RangeRequest_MOD,
				},
				{
					Key: []byte("z"), RangeEnd: []byte("z"),
					Limit:      1,
					SortOrder:  pb.RangeRequest_DESCEND,
					SortTarget: pb.RangeRequest_CREATE,
				},
			},

			[][]string{
				{"a"},
				{"d"},
				{"b"},
				{"c"},
				{},
			},
			[]bool{true, true, true, true, false},
		},
	}

	for i, tt := range tests {
		clus := NewClusterV3(t, &ClusterConfig{Size: 3})
		for _, k := range tt.putKeys {
			kvc := toGRPC(clus.RandClient()).KV
			req := &pb.PutRequest{Key: []byte(k), Value: []byte("bar")}
			if _, err := kvc.Put(context.TODO(), req); err != nil {
				t.Fatalf("#%d: couldn't put key (%v)", i, err)
			}
		}

		for j, req := range tt.reqs {
			kvc := toGRPC(clus.RandClient()).KV
			resp, err := kvc.Range(context.TODO(), &req)
			if err != nil {
				t.Errorf("#%d.%d: Range error: %v", i, j, err)
				continue
			}
			if len(resp.Kvs) != len(tt.wresps[j]) {
				t.Errorf("#%d.%d: bad len(resp.Kvs). got = %d, want = %d, ", i, j, len(resp.Kvs), len(tt.wresps[j]))
				continue
			}
			for k, wKey := range tt.wresps[j] {
				respKey := string(resp.Kvs[k].Key)
				if respKey != wKey {
					t.Errorf("#%d.%d: key[%d]. got = %v, want = %v, ", i, j, k, respKey, wKey)
				}
			}
			if resp.More != tt.wmores[j] {
				t.Errorf("#%d.%d: bad more. got = %v, want = %v, ", i, j, resp.More, tt.wmores[j])
			}
			wrev := int64(len(tt.putKeys) + 1)
			if resp.Header.Revision != wrev {
				t.Errorf("#%d.%d: bad header revision. got = %d. want = %d", i, j, resp.Header.Revision, wrev)
			}
		}
		clus.Terminate(t)
	}
}

func newClusterV3NoClients(t *testing.T, cfg *ClusterConfig) *ClusterV3 {
	cfg.UseGRPC = true
	clus := &ClusterV3{cluster: NewClusterByConfig(t, cfg)}
	clus.Launch(t)
	return clus
}

// TestTLSGRPCRejectInsecureClient checks that connection is rejected if server is TLS but not client.
func TestTLSGRPCRejectInsecureClient(t *testing.T) {
	defer testutil.AfterTest(t)

	cfg := ClusterConfig{Size: 3, ClientTLS: &testTLSInfo}
	clus := newClusterV3NoClients(t, &cfg)
	defer clus.Terminate(t)

	// nil out TLS field so client will use an insecure connection
	clus.Members[0].ClientTLSInfo = nil
	client, err := NewClientV3(clus.Members[0])
	if err != nil && err != grpc.ErrClientConnTimeout {
		t.Fatalf("unexpected error (%v)", err)
	} else if client == nil {
		// Ideally, no client would be returned. However, grpc will
		// return a connection without trying to handshake first so
		// the connection appears OK.
		return
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	conn := client.ActiveConnection()
	st, err := conn.State()
	if err != nil {
		t.Fatal(err)
	} else if st != grpc.Ready {
		t.Fatalf("expected Ready, got %v", st)
	}

	// rpc will fail to handshake, triggering a connection state change
	donec := make(chan error, 1)
	go func() {
		reqput := &pb.PutRequest{Key: []byte("foo"), Value: []byte("bar")}
		_, perr := toGRPC(client).KV.Put(ctx, reqput)
		donec <- perr
	}()

	st, err = conn.WaitForStateChange(ctx, st)
	if err != nil {
		t.Fatalf("unexpected error waiting for change (%v)", err)
	} else if st == grpc.Ready {
		t.Fatalf("expected failure state, got %v", st)
	}
	cancel()
	if perr := <-donec; perr == nil {
		t.Fatalf("expected client error on put")
	}
}

// TestTLSGRPCRejectSecureClient checks that connection is rejected if client is TLS but not server.
func TestTLSGRPCRejectSecureClient(t *testing.T) {
	defer testutil.AfterTest(t)

	cfg := ClusterConfig{Size: 3}
	clus := newClusterV3NoClients(t, &cfg)
	defer clus.Terminate(t)

	clus.Members[0].ClientTLSInfo = &testTLSInfo
	client, err := NewClientV3(clus.Members[0])
	if client != nil || err == nil {
		t.Fatalf("expected no client")
	} else if err != grpc.ErrClientConnTimeout {
		t.Fatalf("unexpected error (%v)", err)
	}
}

// TestTLSGRPCAcceptSecureAll checks that connection is accepted if both client and server are TLS
func TestTLSGRPCAcceptSecureAll(t *testing.T) {
	defer testutil.AfterTest(t)

	cfg := ClusterConfig{Size: 3, ClientTLS: &testTLSInfo}
	clus := newClusterV3NoClients(t, &cfg)
	defer clus.Terminate(t)

	client, err := NewClientV3(clus.Members[0])
	if err != nil {
		t.Fatalf("expected tls client (%v)", err)
	}
	defer client.Close()

	reqput := &pb.PutRequest{Key: []byte("foo"), Value: []byte("bar")}
	if _, err := toGRPC(client).KV.Put(context.TODO(), reqput); err != nil {
		t.Fatalf("unexpected error on put over tls (%v)", err)
	}
}
