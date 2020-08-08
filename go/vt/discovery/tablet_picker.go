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

package discovery

import (
	"math/rand"
	"time"

	"vitess.io/vitess/go/vt/topo/topoproto"

	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"

	"vitess.io/vitess/go/vt/vttablet/tabletconn"

	"vitess.io/vitess/go/vt/log"

	"golang.org/x/net/context"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vterrors"
)

var (
	tabletPickerRetryDelay = 30 * time.Second
)

// TabletPicker gives a simplified API for picking tablets.
type TabletPicker struct {
	ts          *topo.Server
	cells       []string
	keyspace    string
	shard       string
	tabletTypes []topodatapb.TabletType
}

// NewTabletPicker returns a TabletPicker.
func NewTabletPicker(ts *topo.Server, cells []string, keyspace, shard, tabletTypesStr string) (*TabletPicker, error) {
	tabletTypes, err := topoproto.ParseTabletTypes(tabletTypesStr)
	if err != nil {
		return nil, vterrors.Errorf(vtrpcpb.Code_FAILED_PRECONDITION, "failed to parse list of tablet types: %v", tabletTypesStr)
	}
	if keyspace == "" || shard == "" || len(cells) == 0 {
		return nil, vterrors.Errorf(vtrpcpb.Code_FAILED_PRECONDITION, "Keyspace, Shard and cells must be provided")
	}
	return &TabletPicker{
		ts:          ts,
		cells:       cells,
		keyspace:    keyspace,
		shard:       shard,
		tabletTypes: tabletTypes,
	}, nil
}

// PickForStreaming picks an available tablet
// All tablets that belong to tp.cells are evaluated and one is
// chosen at random
func (tp *TabletPicker) PickForStreaming(ctx context.Context) (*topodatapb.Tablet, error) {
	// keep trying at intervals (tabletPickerRetryDelay) until a tablet is found
	// or the context is canceled
	for {
		select {
		case <-ctx.Done():
			return nil, vterrors.Errorf(vtrpcpb.Code_CANCELED, "context has expired")
		default:
		}
		candidates := tp.getMatchingTablets(ctx)
		if len(candidates) == 0 {
			// if no candidates were found, sleep and try again
			time.Sleep(tabletPickerRetryDelay)
			continue
		}
		// try at most len(candidate) times to find a healthy tablet
		for i := 0; i < len(candidates); i++ {
			idx := rand.Intn(len(candidates))
			ti := candidates[idx]
			// get tablet
			// try to connect to tablet
			conn, err := tabletconn.GetDialer()(ti.Tablet, true)
			if err != nil {
				log.Warningf("unable to connect to tablet for alias %v", ti.Alias)
				candidates = append(candidates[:idx], candidates[idx+1:]...)
				if len(candidates) == 0 {
					break
				}
				continue
			}
			// OK to use ctx here because it is not actually used by the underlying Close implementation
			_ = conn.Close(ctx)
			return ti.Tablet, nil
		}
	}
}

// getMatchingTablets returns a list of TabletInfo for tablets
// that match the cells, keyspace, shard and tabletTypes for this TabletPicker
func (tp *TabletPicker) getMatchingTablets(ctx context.Context) []*topo.TabletInfo {
	// Special handling for MASTER tablet type
	// Since there is only one master, we ignore cell and find the master
	aliases := make([]*topodatapb.TabletAlias, 0)
	if len(tp.tabletTypes) == 1 && tp.tabletTypes[0] == topodatapb.TabletType_MASTER {
		shortCtx, cancel := context.WithTimeout(ctx, *topo.RemoteOperationTimeout)
		defer cancel()
		si, err := tp.ts.GetShard(shortCtx, tp.keyspace, tp.shard)
		if err != nil {
			return nil
		}
		aliases = append(aliases, si.MasterAlias)
	} else {
		actualCells := make([]string, 0)
		for _, cell := range tp.cells {
			// check if cell is actually an alias
			// non-blocking read so that this is fast
			shortCtx, cancel := context.WithTimeout(ctx, *topo.RemoteOperationTimeout)
			defer cancel()
			alias, err := tp.ts.GetCellsAlias(shortCtx, cell, false)
			if err != nil {
				// either cellAlias doesn't exist or it isn't a cell alias at all. In that case assume it is a cell
				actualCells = append(actualCells, cell)
			} else {
				actualCells = append(actualCells, alias.Cells...)
			}
		}
		for _, cell := range actualCells {
			shortCtx, cancel := context.WithTimeout(ctx, *topo.RemoteOperationTimeout)
			defer cancel()
			// match cell, keyspace and shard
			sri, err := tp.ts.GetShardReplication(shortCtx, cell, tp.keyspace, tp.shard)
			if err != nil {
				log.Warningf("error %v from GetShardReplication for %v %v %v", err, cell, tp.keyspace, tp.shard)
				continue
			}

			for _, node := range sri.Nodes {
				aliases = append(aliases, node.TabletAlias)
			}
		}
	}

	if len(aliases) == 0 {
		return nil
	}
	shortCtx, cancel := context.WithTimeout(ctx, *topo.RemoteOperationTimeout)
	defer cancel()
	tabletMap, err := tp.ts.GetTabletMap(shortCtx, aliases)
	if err != nil {
		log.Warningf("error fetching tablets from topo: %v", err)
		return nil
	}
	tablets := make([]*topo.TabletInfo, 0, len(aliases))
	for _, tabletAlias := range aliases {
		tabletInfo, ok := tabletMap[topoproto.TabletAliasString(tabletAlias)]
		if !ok {
			// tablet disappeared on us (GetTabletMap ignores
			// topo.ErrNoNode), just echo a warning
			log.Warningf("failed to load tablet %v", tabletAlias)
		} else if topoproto.IsTypeInList(tabletInfo.Type, tp.tabletTypes) {
			tablets = append(tablets, tabletInfo)
		}
	}
	return tablets
}

func init() {
	// TODO(sougou): consolidate this call to be once per process.
	rand.Seed(time.Now().UnixNano())
}
