/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tabletmanager

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stretchr/testify/assert"
	"vitess.io/vitess/go/json2"
	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/test/endtoend/cluster"
	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

// TabletReshuffle test if a vttablet can be pointed at an existing mysql
func TestTabletReshuffle(t *testing.T) {
	ctx := context.Background()

	masterConn, err := mysql.Connect(ctx, &masterTabletParams)
	require.NoError(t, err)
	defer masterConn.Close()

	replicaConn, err := mysql.Connect(ctx, &replicaTabletParams)
	require.NoError(t, err)
	defer replicaConn.Close()

	// Sanity Check
	exec(t, masterConn, "delete from t1")
	exec(t, masterConn, "insert into t1(id, value) values(1,'a'), (2,'b')")
	checkDataOnReplica(t, replicaConn, `[[VARCHAR("a")] [VARCHAR("b")]]`)

	//Create new tablet
	rTablet := clusterInstance.GetVttabletInstance("replica", 0, "")

	//Init Tablets
	err = clusterInstance.VtctlclientProcess.InitTablet(rTablet, cell, keyspaceName, hostname, shardName)
	require.NoError(t, err)

	// mycnf_server_id prevents vttablet from reading the mycnf
	// Pointing to masterTablet's socket file
	clusterInstance.VtTabletExtraArgs = []string{
		"-lock_tables_timeout", "5s",
		"-mycnf_server_id", fmt.Sprintf("%d", rTablet.TabletUID),
		"-db_socket", fmt.Sprintf("%s/mysql.sock", masterTablet.VttabletProcess.Directory),
	}
	// SupportsBackup=False prevents vttablet from trying to restore
	// Start vttablet process
	err = clusterInstance.StartVttablet(rTablet, "SERVING", false, cell, keyspaceName, hostname, shardName)
	assert.Nil(t, err)

	sql := "select value from t1"
	args := []string{
		"VtTabletExecute",
		"-options", "included_fields:TYPE_ONLY",
		"-json",
		rTablet.Alias,
		sql,
	}
	result, err := clusterInstance.VtctlclientProcess.ExecuteCommandWithOutput(args...)
	assertExcludeFields(t, result)

	err = clusterInstance.VtctlclientProcess.ExecuteCommand("Backup", rTablet.Alias)
	assert.Error(t, err, "cannot perform backup without my.cnf")

	// Reset the VtTabletExtraArgs
	clusterInstance.VtTabletExtraArgs = []string{}
	killTablets(t, rTablet)
}

func TestHealthCheck(t *testing.T) {
	// Add one replica that starts not initialized
	// (for the replica, we let vttablet do the InitTablet)
	ctx := context.Background()

	rTablet := clusterInstance.GetVttabletInstance("replica", 0, "")

	// Start Mysql Processes and return connection
	replicaConn, err := cluster.StartMySQLAndGetConnection(ctx, rTablet, username, clusterInstance.TmpDirectory)
	assert.Nil(t, err)

	defer replicaConn.Close()

	// Create database in mysql
	exec(t, replicaConn, fmt.Sprintf("create database vt_%s", keyspaceName))

	//Init Replica Tablet
	err = clusterInstance.VtctlclientProcess.InitTablet(rTablet, cell, keyspaceName, hostname, shardName)

	// start vttablet process, should be in SERVING state as we already have a master
	err = clusterInstance.StartVttablet(rTablet, "SERVING", false, cell, keyspaceName, hostname, shardName)
	assert.Nil(t, err, "error should be Nil")

	masterConn, err := mysql.Connect(ctx, &masterTabletParams)
	require.NoError(t, err)
	defer masterConn.Close()

	err = clusterInstance.VtctlclientProcess.ExecuteCommand("RunHealthCheck", rTablet.Alias)
	assert.Nil(t, err)
	checkHealth(t, replicaTablet.HTTPPort, false)

	// Make sure the master is still master
	checkTabletType(t, masterTablet.Alias, "MASTER")
	exec(t, masterConn, "stop slave")

	// stop replication, make sure we don't go unhealthy.
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("StopSlave", rTablet.Alias)
	assert.Nil(t, err)
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("RunHealthCheck", rTablet.Alias)
	assert.Nil(t, err)

	// make sure the health stream is updated
	result, err := clusterInstance.VtctlclientProcess.ExecuteCommandWithOutput("VtTabletStreamHealth", "-count", "1", rTablet.Alias)
	assert.Nil(t, err)
	verifyStreamHealth(t, result)

	// then restart replication, make sure we stay healthy
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("StopSlave", rTablet.Alias)
	assert.Nil(t, err)
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("RunHealthCheck", rTablet.Alias)
	assert.Nil(t, err)
	checkHealth(t, replicaTablet.HTTPPort, false)

	// now test VtTabletStreamHealth returns the right thing
	result, err = clusterInstance.VtctlclientProcess.ExecuteCommandWithOutput("VtTabletStreamHealth", "-count", "2", rTablet.Alias)
	scanner := bufio.NewScanner(strings.NewReader(result))
	for scanner.Scan() {
		// fmt.Println() // Println will add back the final '\n'
		verifyStreamHealth(t, scanner.Text())
	}

	// Manual cleanup of processes
	killTablets(t, rTablet)
}

func checkHealth(t *testing.T, port int, shouldError bool) {
	url := fmt.Sprintf("http://localhost:%d/healthz", port)
	resp, err := http.Get(url)
	assert.Nil(t, err)
	if shouldError {
		assert.True(t, resp.StatusCode > 400)
	} else {
		assert.Equal(t, 200, resp.StatusCode)
	}
}

func checkTabletType(t *testing.T, tabletAlias string, typeWant string) {
	result, err := clusterInstance.VtctlclientProcess.ExecuteCommandWithOutput("GetTablet", tabletAlias)
	assert.Nil(t, err)

	var tablet topodatapb.Tablet
	err = json2.Unmarshal([]byte(result), &tablet)
	assert.Nil(t, err)

	actualType := tablet.GetType()
	got := fmt.Sprintf("%d", actualType)

	tabletType := topodatapb.TabletType_value[typeWant]
	want := fmt.Sprintf("%d", tabletType)

	assert.Equal(t, want, got)
}

func verifyStreamHealth(t *testing.T, result string) {
	var streamHealthResponse querypb.StreamHealthResponse
	err := json2.Unmarshal([]byte(result), &streamHealthResponse)
	require.NoError(t, err)
	serving := streamHealthResponse.GetServing()
	UID := streamHealthResponse.GetTabletAlias().GetUid()
	realTimeStats := streamHealthResponse.GetRealtimeStats()
	secondsBehindMaster := realTimeStats.GetSecondsBehindMaster()
	assert.True(t, serving, "Tablet should be in serving state")
	assert.True(t, UID > 0, "Tablet should contain uid")
	// secondsBehindMaster varies till 7200 so setting safe limit
	assert.True(t, secondsBehindMaster < 10000, "Slave should not be behind master")
}

func TestHealthCheckDrainedStateDoesNotShutdownQueryService(t *testing.T) {
	// This test is similar to test_health_check, but has the following differences:
	// - the second tablet is an 'rdonly' and not a 'replica'
	// - the second tablet will be set to 'drained' and we expect that
	// - the query service won't be shutdown

	//Wait if tablet is not in service state
	waitForTabletStatus(rdonlyTablet, "SERVING")

	// Check tablet health
	checkHealth(t, rdonlyTablet.HTTPPort, false)
	assert.Equal(t, "SERVING", rdonlyTablet.VttabletProcess.GetTabletStatus())

	// Change from rdonly to drained and stop replication. (These
	// actions are similar to the SplitClone vtworker command
	// implementation.)  The tablet will stay healthy, and the
	// query service is still running.
	err := clusterInstance.VtctlclientProcess.ExecuteCommand("ChangeSlaveType", rdonlyTablet.Alias, "drained")
	assert.Nil(t, err)
	// Trying to drain the same tablet again, should error
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("ChangeSlaveType", rdonlyTablet.Alias, "drained")
	assert.Error(t, err, "already drained")

	err = clusterInstance.VtctlclientProcess.ExecuteCommand("StopSlave", rdonlyTablet.Alias)
	assert.Nil(t, err)
	// Trigger healthcheck explicitly to avoid waiting for the next interval.
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("RunHealthCheck", rdonlyTablet.Alias)
	assert.Nil(t, err)

	checkTabletType(t, rdonlyTablet.Alias, "DRAINED")

	// Query service is still running.
	waitForTabletStatus(rdonlyTablet, "SERVING")

	// Restart replication. Tablet will become healthy again.
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("ChangeSlaveType", rdonlyTablet.Alias, "rdonly")
	assert.Nil(t, err)
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("StartSlave", rdonlyTablet.Alias)
	assert.Nil(t, err)
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("RunHealthCheck", rdonlyTablet.Alias)
	assert.Nil(t, err)
	checkHealth(t, rdonlyTablet.HTTPPort, false)
}

func waitForTabletStatus(tablet cluster.Vttablet, status string) error {
	timeout := time.Now().Add(10 * time.Second)
	for time.Now().Before(timeout) {
		if tablet.VttabletProcess.WaitForStatus(status) {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("Tablet status is not %s ", status)
}

func TestIgnoreHealthError(t *testing.T) {
	ctx := context.Background()
	mTablet := clusterInstance.GetVttabletInstance("replica", masterUID, "")

	//Init Tablets
	err := clusterInstance.VtctlclientProcess.InitTablet(mTablet, cell, keyspaceName, hostname, shardName)
	assert.Nil(t, err)

	// Start Mysql Processes
	masterConn, err := cluster.StartMySQLAndGetConnection(ctx, mTablet, username, clusterInstance.TmpDirectory)
	defer masterConn.Close()
	assert.Nil(t, err)

	mTablet.MysqlctlProcess.Stop()
	// Clean dir for mysql files
	mTablet.MysqlctlProcess.CleanupFiles(mTablet.TabletUID)

	// Start Vttablet, it should be NOT_SERVING state as mysql is stopped
	err = clusterInstance.StartVttablet(mTablet, "NOT_SERVING", false, cell, keyspaceName, hostname, shardName)
	assert.Nil(t, err)

	// Force it healthy.
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("IgnoreHealthError", mTablet.Alias, ".*no slave status.*")
	assert.Nil(t, err)
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("RunHealthCheck", mTablet.Alias)
	assert.Nil(t, err)
	waitForTabletStatus(*mTablet, "SERVING")

	// Turn off the force-healthy.
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("IgnoreHealthError", mTablet.Alias, "")
	assert.Nil(t, err)
	err = clusterInstance.VtctlclientProcess.ExecuteCommand("RunHealthCheck", mTablet.Alias)
	assert.Nil(t, err)
	waitForTabletStatus(*mTablet, "NOT_SERVING")
	checkHealth(t, mTablet.HTTPPort, true)

	// Tear down custom processes
	killTablets(t, mTablet)
}

func killTablets(t *testing.T, tablets ...*cluster.Vttablet) {
	for _, tablet := range tablets {
		//Stop Mysqld
		tablet.MysqlctlProcess.Stop()

		//Tear down Tablet
		tablet.VttabletProcess.TearDown()

	}
}
