// Copyright 2019 PingCAP, Inc.
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

package owner_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	. "github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/infoschema"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/owner"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/store/mockstore"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	"go.etcd.io/etcd/tests/v3/integration"
)

const testLease = 5 * time.Millisecond

type testInfo struct {
	store   kv.Storage
	cluster *integration.ClusterV3
	client  *clientv3.Client
	ddl     DDL
}

func newTestInfo(t *testing.T) *testInfo {
	store, err := mockstore.NewMockStore()
	require.NoError(t, err)
	cluster := integration.NewClusterV3(t, &integration.ClusterConfig{Size: 4})

	cli := cluster.Client(0)
	ic := infoschema.NewCache(2)
	ic.Insert(infoschema.MockInfoSchemaWithSchemaVer(nil, 0), 0)
	d := NewDDL(
		context.Background(),
		WithEtcdClient(cli),
		WithStore(store),
		WithLease(testLease),
		WithInfoCache(ic),
	)

	return &testInfo{
		store:   store,
		cluster: cluster,
		client:  cli,
		ddl:     d,
	}
}

func (ti *testInfo) Close(t *testing.T) {
	err := ti.ddl.Stop()
	require.NoError(t, err)
	err = ti.store.Close()
	require.NoError(t, err)
	ti.cluster.Terminate(t)
}

func TestSingle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration.NewClusterV3 will create file contains a colon which is not allowed on Windows")
	}
	integration.BeforeTestExternal(t)

	tInfo := newTestInfo(t)
	client, d := tInfo.client, tInfo.ddl
	defer tInfo.Close(t)
	require.NoError(t, d.OwnerManager().CampaignOwner())
	isOwner := checkOwner(d, true)
	require.True(t, isOwner)

	// test for newSession failed
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	manager := owner.NewOwnerManager(ctx, client, "ddl", "ddl_id", DDLOwnerKey)
	cancel()

	err := manager.CampaignOwner()
	comment := fmt.Sprintf("campaigned result don't match, err %v", err)
	require.True(t, terror.ErrorEqual(err, context.Canceled) || terror.ErrorEqual(err, context.DeadlineExceeded), comment)

	isOwner = checkOwner(d, true)
	require.True(t, isOwner)

	// The test is used to exit campaign loop.
	d.OwnerManager().Cancel()
	isOwner = checkOwner(d, false)
	require.False(t, isOwner)

	time.Sleep(200 * time.Millisecond)

	// err is ok to be not nil since we canceled the manager.
	ownerID, _ := manager.GetOwnerID(ctx)
	require.Equal(t, "", ownerID)
	op, _ := owner.GetOwnerOpValue(ctx, client, DDLOwnerKey, "log prefix")
	require.Equal(t, op, owner.OpNone)
}

func TestSetAndGetOwnerOpValue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration.NewClusterV3 will create file contains a colon which is not allowed on Windows")
	}
	integration.BeforeTestExternal(t)

	tInfo := newTestInfo(t)
	defer tInfo.Close(t)

	require.NoError(t, tInfo.ddl.OwnerManager().CampaignOwner())
	isOwner := checkOwner(tInfo.ddl, true)
	require.True(t, isOwner)

	// test set/get owner info
	manager := tInfo.ddl.OwnerManager()
	ownerID, err := manager.GetOwnerID(context.Background())
	require.NoError(t, err)
	require.Equal(t, tInfo.ddl.GetID(), ownerID)
	op, err := owner.GetOwnerOpValue(context.Background(), tInfo.client, DDLOwnerKey, "log prefix")
	require.NoError(t, err)
	require.Equal(t, op, owner.OpNone)
	require.False(t, op.IsSyncedUpgradingState())
	err = manager.SetOwnerOpValue(context.Background(), owner.OpSyncUpgradingState)
	require.NoError(t, err)
	op, err = owner.GetOwnerOpValue(context.Background(), tInfo.client, DDLOwnerKey, "log prefix")
	require.NoError(t, err)
	require.Equal(t, op, owner.OpSyncUpgradingState)
	require.True(t, op.IsSyncedUpgradingState())
	// update the same as the original value
	err = manager.SetOwnerOpValue(context.Background(), owner.OpSyncUpgradingState)
	require.NoError(t, err)
	op, err = owner.GetOwnerOpValue(context.Background(), tInfo.client, DDLOwnerKey, "log prefix")
	require.NoError(t, err)
	require.Equal(t, op, owner.OpSyncUpgradingState)
	require.True(t, op.IsSyncedUpgradingState())
	// test del owner key when SetOwnerOpValue
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/owner/MockDelOwnerKey", `return("delOwnerKeyAndNotOwner")`))
	err = manager.SetOwnerOpValue(context.Background(), owner.OpNone)
	require.Error(t, err, "put owner key failed, cmp is false")
	op, err = owner.GetOwnerOpValue(context.Background(), tInfo.client, DDLOwnerKey, "log prefix")
	require.NotNil(t, err)
	require.Equal(t, concurrency.ErrElectionNoLeader.Error(), err.Error())
	require.Equal(t, op, owner.OpNone)
	require.False(t, op.IsSyncedUpgradingState())
	require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/owner/MockDelOwnerKey"))

	// Let ddl run for the owner again.
	require.NoError(t, tInfo.ddl.OwnerManager().CampaignOwner())
	isOwner = checkOwner(tInfo.ddl, true)
	require.True(t, isOwner)
	// Mock the manager become not owner because the owner is deleted(like TTL is timeout).
	// And then the manager campaigns the owner again, and become the owner.
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/owner/MockDelOwnerKey", `return("onlyDelOwnerKey")`))
	err = manager.SetOwnerOpValue(context.Background(), owner.OpSyncUpgradingState)
	require.Error(t, err, "put owner key failed, cmp is false")
	isOwner = checkOwner(tInfo.ddl, true)
	require.True(t, isOwner)
	op, err = owner.GetOwnerOpValue(context.Background(), tInfo.client, DDLOwnerKey, "log prefix")
	require.NoError(t, err)
	require.Equal(t, op, owner.OpNone)
	require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/owner/MockDelOwnerKey"))
}

// TestGetOwnerOpValueBeforeSet tests get owner opValue before set this value when the etcdClient is nil.
func TestGetOwnerOpValueBeforeSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration.NewClusterV3 will create file contains a colon which is not allowed on Windows")
	}
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/owner/MockNotSetOwnerOp", `return(true)`))

	_, dom := testkit.CreateMockStoreAndDomain(t)
	ddl := dom.DDL()
	require.NoError(t, ddl.OwnerManager().CampaignOwner())
	isOwner := checkOwner(ddl, true)
	require.True(t, isOwner)

	// test set/get owner info
	manager := ddl.OwnerManager()
	ownerID, err := manager.GetOwnerID(context.Background())
	require.NoError(t, err)
	require.Equal(t, ddl.GetID(), ownerID)
	op, err := owner.GetOwnerOpValue(context.Background(), nil, DDLOwnerKey, "log prefix")
	require.NoError(t, err)
	require.Equal(t, op, owner.OpNone)
	require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/owner/MockNotSetOwnerOp"))
	err = manager.SetOwnerOpValue(context.Background(), owner.OpSyncUpgradingState)
	require.NoError(t, err)
	op, err = owner.GetOwnerOpValue(context.Background(), nil, DDLOwnerKey, "log prefix")
	require.NoError(t, err)
	require.Equal(t, op, owner.OpSyncUpgradingState)
}

func TestCluster(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration.NewClusterV3 will create file contains a colon which is not allowed on Windows")
	}
	integration.BeforeTestExternal(t)

	originalTTL := owner.ManagerSessionTTL
	owner.ManagerSessionTTL = 3
	defer func() {
		owner.ManagerSessionTTL = originalTTL
	}()

	tInfo := newTestInfo(t)
	store, cluster, d := tInfo.store, tInfo.cluster, tInfo.ddl
	defer tInfo.Close(t)
	require.NoError(t, d.OwnerManager().CampaignOwner())

	isOwner := checkOwner(d, true)
	require.True(t, isOwner)

	cli1 := cluster.Client(1)
	ic2 := infoschema.NewCache(2)
	ic2.Insert(infoschema.MockInfoSchemaWithSchemaVer(nil, 0), 0)
	d1 := NewDDL(
		context.Background(),
		WithEtcdClient(cli1),
		WithStore(store),
		WithLease(testLease),
		WithInfoCache(ic2),
	)
	require.NoError(t, d1.OwnerManager().CampaignOwner())

	isOwner = checkOwner(d1, false)
	require.False(t, isOwner)

	// Delete the leader key, the d1 become the owner.
	cliRW := cluster.Client(2)
	err := deleteLeader(cliRW, DDLOwnerKey)
	require.NoError(t, err)

	isOwner = checkOwner(d, false)
	require.False(t, isOwner)

	d.OwnerManager().Cancel()
	// d3 (not owner) stop
	cli3 := cluster.Client(3)
	ic3 := infoschema.NewCache(2)
	ic3.Insert(infoschema.MockInfoSchemaWithSchemaVer(nil, 0), 0)
	d3 := NewDDL(
		context.Background(),
		WithEtcdClient(cli3),
		WithStore(store),
		WithLease(testLease),
		WithInfoCache(ic3),
	)
	require.NoError(t, d3.OwnerManager().CampaignOwner())

	isOwner = checkOwner(d3, false)
	require.False(t, isOwner)

	d3.OwnerManager().Cancel()
	// Cancel the owner context, there is no owner.
	d1.OwnerManager().Cancel()

	logPrefix := fmt.Sprintf("[ddl] %s ownerManager %s", DDLOwnerKey, "useless id")
	logCtx := logutil.WithKeyValue(context.Background(), "owner info", logPrefix)
	_, _, err = owner.GetOwnerKeyInfo(context.Background(), logCtx, cliRW, DDLOwnerKey, "useless id")
	require.Truef(t, terror.ErrorEqual(err, concurrency.ErrElectionNoLeader), "get owner info result don't match, err %v", err)
	op, err := owner.GetOwnerOpValue(context.Background(), cliRW, DDLOwnerKey, logPrefix)
	require.Truef(t, terror.ErrorEqual(err, concurrency.ErrElectionNoLeader), "get owner info result don't match, err %v", err)
	require.Equal(t, op, owner.OpNone)
}

func TestWatchOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration.NewClusterV3 will create file contains a colon which is not allowed on Windows")
	}
	integration.BeforeTestExternal(t)

	tInfo := newTestInfo(t)
	client, d := tInfo.client, tInfo.ddl
	defer tInfo.Close(t)
	ownerManager := d.OwnerManager()

	// failed to get owner id before CampaignOwner().
	ctx := context.Background()
	_, err := ownerManager.GetOwnerID(ctx)
	require.ErrorContains(t, err, "election: no leader")
	_, _, err = owner.GetOwnerKeyInfo(ctx, ctx, client, DDLOwnerKey, ownerManager.ID())
	require.ErrorContains(t, err, "election: no leader")

	// start CampaignOwner.
	require.NoError(t, ownerManager.CampaignOwner())
	isOwner := checkOwner(d, true)
	require.True(t, isOwner)

	// get the owner id with a canceled context.
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = owner.GetOwnerKeyInfo(ctx, ctx, client, DDLOwnerKey, ownerManager.ID())
	require.ErrorContains(t, err, "ownerInfoNotMatch")

	// get the owner id.
	ctx = context.Background()
	id, err := ownerManager.GetOwnerID(ctx)
	require.NoError(t, err)

	// create etcd session.
	session, err := concurrency.NewSession(client)
	require.NoError(t, err)

	// test the GetOwnerKeyInfo()
	ownerKey, currRevision, err := owner.GetOwnerKeyInfo(ctx, context.TODO(), client, DDLOwnerKey, id)
	require.NoError(t, err)

	// watch the ownerKey.
	ctx2, cancel2 := context.WithTimeout(ctx, time.Millisecond*300)
	defer cancel2()
	watchDone := make(chan bool)
	watched := false
	go func() {
		watchErr := owner.WatchOwnerForTest(ctx, ownerManager, session, ownerKey, currRevision)
		require.NoError(t, watchErr)
		watchDone <- true
	}()

	select {
	case watched = <-watchDone:
	case <-ctx2.Done():
	}
	require.False(t, watched)

	// delete the owner, and can watch the DELETE event.
	err = deleteLeader(client, DDLOwnerKey)
	require.NoError(t, err)
	watched = <-watchDone
	require.True(t, watched)

	// the ownerKey has been deleted, watch ownerKey again, it can be watched.
	go func() {
		watchErr := owner.WatchOwnerForTest(ctx, ownerManager, session, ownerKey, currRevision)
		require.NoError(t, watchErr)
		watchDone <- true
	}()

	watched = <-watchDone
	require.True(t, watched)
}

func TestWatchOwnerAfterDeleteOwnerKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration.NewClusterV3 will create file contains a colon which is not allowed on Windows")
	}
	integration.BeforeTestExternal(t)

	tInfo := newTestInfo(t)
	client, d := tInfo.client, tInfo.ddl
	defer tInfo.Close(t)
	ownerManager := d.OwnerManager()
	require.NoError(t, ownerManager.CampaignOwner())
	isOwner := checkOwner(d, true)
	require.True(t, isOwner)

	// get the owner id.
	ctx := context.Background()
	id, err := ownerManager.GetOwnerID(ctx)
	require.NoError(t, err)
	session, err := concurrency.NewSession(client)
	require.NoError(t, err)

	// get the ownkey informations.
	ownerKey, currRevision, err := owner.GetOwnerKeyInfo(ctx, context.TODO(), client, DDLOwnerKey, id)
	require.NoError(t, err)

	// delete the ownerkey
	err = deleteLeader(client, DDLOwnerKey)
	require.NoError(t, err)

	// watch the ownerKey with the current revisoin.
	watchDone := make(chan bool)
	go func() {
		watchErr := owner.WatchOwnerForTest(ctx, ownerManager, session, ownerKey, currRevision)
		require.NoError(t, watchErr)
		watchDone <- true
	}()
	<-watchDone
}

func checkOwner(d DDL, fbVal bool) (isOwner bool) {
	manager := d.OwnerManager()
	// The longest to wait for 30 seconds to
	// make sure that campaigning owners is completed.
	for i := 0; i < 6000; i++ {
		time.Sleep(5 * time.Millisecond)
		isOwner = manager.IsOwner()
		if isOwner == fbVal {
			break
		}
	}
	return
}

func deleteLeader(cli *clientv3.Client, prefixKey string) error {
	session, err := concurrency.NewSession(cli)
	if err != nil {
		return errors.Trace(err)
	}
	defer func() {
		_ = session.Close()
	}()
	election := concurrency.NewElection(session, prefixKey)
	resp, err := election.Leader(context.Background())
	if err != nil {
		return errors.Trace(err)
	}
	_, err = cli.Delete(context.Background(), string(resp.Kvs[0].Key))
	return errors.Trace(err)
}

func TestImmediatelyCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration.NewClusterV3 will create file contains a colon which is not allowed on Windows")
	}
	integration.BeforeTestExternal(t)

	tInfo := newTestInfo(t)
	d := tInfo.ddl
	defer tInfo.Close(t)
	ownerManager := d.OwnerManager()
	for i := 0; i < 10; i++ {
		err := ownerManager.CampaignOwner()
		require.NoError(t, err)
		ownerManager.CampaignCancel()
	}
}
