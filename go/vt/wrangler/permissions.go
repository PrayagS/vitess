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

package wrangler

import (
	"fmt"
	"sort"
	"sync"

	"context"

	"vitess.io/vitess/go/vt/concurrency"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/mysqlctl/tmutils"
	"vitess.io/vitess/go/vt/topo/topoproto"

	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

// GetPermissions returns the permissions set on a remote tablet
func (wr *Wrangler) GetPermissions(ctx context.Context, tabletAlias *topodatapb.TabletAlias) (*tabletmanagerdatapb.Permissions, error) {
	ti, err := wr.ts.GetTablet(ctx, tabletAlias)
	if err != nil {
		return nil, err
	}

	return wr.tmc.GetPermissions(ctx, ti.Tablet)
}

// diffPermissions is a helper method to asynchronously diff a permissions
func (wr *Wrangler) diffPermissions(ctx context.Context, masterPermissions *tabletmanagerdatapb.Permissions, masterAlias *topodatapb.TabletAlias, alias *topodatapb.TabletAlias, wg *sync.WaitGroup, er concurrency.ErrorRecorder) {
	defer wg.Done()
	log.Infof("Gathering permissions for %v", topoproto.TabletAliasString(alias))
	replicaPermissions, err := wr.GetPermissions(ctx, alias)
	if err != nil {
		er.RecordError(err)
		return
	}

	log.Infof("Diffing permissions for %v", topoproto.TabletAliasString(alias))
	tmutils.DiffPermissions(topoproto.TabletAliasString(masterAlias), masterPermissions, topoproto.TabletAliasString(alias), replicaPermissions, er)
}

// ValidatePermissionsShard validates all the permissions are the same
// in a shard
func (wr *Wrangler) ValidatePermissionsShard(ctx context.Context, keyspace, shard string) error {
	si, err := wr.ts.GetShard(ctx, keyspace, shard)
	if err != nil {
		return err
	}

	// get permissions from the primary, or error
	if !si.HasMaster() {
		return fmt.Errorf("no master in shard %v/%v", keyspace, shard)
	}
	log.Infof("Gathering permissions for master %v", topoproto.TabletAliasString(si.PrimaryAlias))
	masterPermissions, err := wr.GetPermissions(ctx, si.PrimaryAlias)
	if err != nil {
		return err
	}

	// read all the aliases in the shard, that is all tablets that are
	// replicating from the primary
	aliases, err := wr.ts.FindAllTabletAliasesInShard(ctx, keyspace, shard)
	if err != nil {
		return err
	}

	// then diff all of them, except primary
	er := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}
	for _, alias := range aliases {
		if topoproto.TabletAliasEqual(alias, si.PrimaryAlias) {
			continue
		}
		wg.Add(1)
		go wr.diffPermissions(ctx, masterPermissions, si.PrimaryAlias, alias, &wg, &er)
	}
	wg.Wait()
	if er.HasErrors() {
		return fmt.Errorf("permissions diffs: %v", er.Error().Error())
	}
	return nil
}

// ValidatePermissionsKeyspace validates all the permissions are the same
// in a keyspace
func (wr *Wrangler) ValidatePermissionsKeyspace(ctx context.Context, keyspace string) error {
	// find all the shards
	shards, err := wr.ts.GetShardNames(ctx, keyspace)
	if err != nil {
		return err
	}

	// corner cases
	if len(shards) == 0 {
		return fmt.Errorf("no shards in keyspace %v", keyspace)
	}
	sort.Strings(shards)
	if len(shards) == 1 {
		return wr.ValidatePermissionsShard(ctx, keyspace, shards[0])
	}

	// find the reference permissions using the first shard's primary
	si, err := wr.ts.GetShard(ctx, keyspace, shards[0])
	if err != nil {
		return err
	}
	if !si.HasMaster() {
		return fmt.Errorf("no master in shard %v/%v", keyspace, shards[0])
	}
	referenceAlias := si.PrimaryAlias
	log.Infof("Gathering permissions for reference primary %v", topoproto.TabletAliasString(referenceAlias))
	referencePermissions, err := wr.GetPermissions(ctx, si.PrimaryAlias)
	if err != nil {
		return err
	}

	// then diff with all tablets but primary 0
	er := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}
	for _, shard := range shards {
		aliases, err := wr.ts.FindAllTabletAliasesInShard(ctx, keyspace, shard)
		if err != nil {
			er.RecordError(err)
			continue
		}

		for _, alias := range aliases {
			if topoproto.TabletAliasEqual(alias, si.PrimaryAlias) {
				continue
			}

			wg.Add(1)
			go wr.diffPermissions(ctx, referencePermissions, referenceAlias, alias, &wg, &er)
		}
	}
	wg.Wait()
	if er.HasErrors() {
		return fmt.Errorf("permissions diffs: %v", er.Error().Error())
	}
	return nil
}
