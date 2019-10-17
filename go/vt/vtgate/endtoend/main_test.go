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

package endtoend

import (
	"flag"
	"fmt"
	"os"
	"path"
	"testing"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/vttest"

	vschemapb "vitess.io/vitess/go/vt/proto/vschema"
	vttestpb "vitess.io/vitess/go/vt/proto/vttest"
)

var (
	cluster        *vttest.LocalCluster
	vtParams       mysql.ConnParams
	mysqlParams    mysql.ConnParams
	grpcAddress    string
	tabletHostName = flag.String("tablet_hostname", "", "the tablet hostname")

	schema = `
create table t1(
	id1 bigint,
	id2 bigint,
	primary key(id1)
) Engine=InnoDB;

create table t1_id2_idx(
	id2 bigint,
	keyspace_id varbinary(10),
	primary key(id2)
) Engine=InnoDB;

create table vstream_test(
	id bigint,
	val bigint,
	primary key(id)
) Engine=InnoDB;

create table aggr_test(
	id bigint,
	val1 varchar(16),
	val2 bigint,
	primary key(id)
) Engine=InnoDB;

create table t2(
	id3 bigint,
	id4 bigint,
	primary key(id3)
) Engine=InnoDB;

create table t2_id4_idx(
	id bigint not null auto_increment,
	id4 bigint,
	id3 bigint,
	primary key(id),
	key idx_id4(id4)
) Engine=InnoDB;

create table user(
	id bigint,
	name varchar(64),
	primary key(id)
) Engine=InnoDB;

create table user_details(
	user_id bigint,
	email varchar(64),
	primary key(user_id)
) Engine=InnoDB;

create table user_info(
	name varchar(64),
	info varchar(128),
	primary key(name)
) Engine=InnoDB;

create table t3 (
	user_id bigint,
	lastname varchar(64),
	address varchar(64),
	primary key (user_id)
) Engine=InnoDB;

create table t3_lastname_map (
	lastname varchar(64),
	user_id bigint,
	UNIQUE (lastname, user_id)
) Engine=InnoDB;

create table t3_address_map (
	address varchar(64),
	user_id bigint,
	primary key (address)
) Engine=InnoDB;

create table t4_music (
	user_id bigint,
	id bigint,
	song varchar(64),
	primary key (user_id, id)
) Engine=InnoDB;

create table t4_music_art (
	music_id bigint,
	user_id bigint,
	artist varchar(64),
	primary key (music_id)
) Engine=InnoDB;

create table t4_music_lookup (
	music_id bigint,
	user_id bigint,
	primary key (music_id)
) Engine=InnoDB;

create table upsert_primary (
	pk bigint,
	ksnum_id bigint,
	primary key (pk)
	) Engine=InnoDB;

create table upsert_owned (
	owned bigint,
	ksnum_id bigint,
	primary key (owned)
	) Engine=InnoDB;

create table upsert (
	pk bigint,
	owned bigint,
	user_id bigint,
	col bigint,
	primary key (pk)
	) Engine=InnoDB;

create table vt_user (
	id bigint,
	name varchar(64),
	primary key (id)
	) Engine=InnoDB;

create table twopc_user (
	user_id bigint,
	name varchar(128),
	primary key (user_id)
) Engine=InnoDB;

create table twopc_lookup (
	name varchar(128),
	id bigint,
	primary key (id)
) Engine=InnoDB;
`

	vschema = &vschemapb.Keyspace{
		Sharded: true,
		Vindexes: map[string]*vschemapb.Vindex{
			"hash": {
				Type: "hash",
			},
			"unicode_hash": {
				Type: "unicode_loose_md5",
			},
			"t1_id2_vdx": {
				Type: "consistent_lookup_unique",
				Params: map[string]string{
					"table": "t1_id2_idx",
					"from":  "id2",
					"to":    "keyspace_id",
				},
				Owner: "t1",
			},
			"t2_id4_idx": {
				Type: "lookup_hash",
				Params: map[string]string{
					"table":      "t2_id4_idx",
					"from":       "id4",
					"to":         "id3",
					"autocommit": "true",
				},
				Owner: "t2",
			},
			"t3_lastname_map_vdx": {
				Type: "lookup_hash",
				Params: map[string]string{
					"table":      "t3_lastname_map",
					"from":       "lastname",
					"to":         "user_id",
					"autocommit": "true",
				},
				Owner: "t3",
			},
			"t3_address_map_vdx": {
				Type: "lookup_hash_unique",
				Params: map[string]string{
					"table":      "t3_address_map",
					"from":       "address",
					"to":         "user_id",
					"autocommit": "true",
				},
				Owner: "t3",
			},
			"t4_music_lookup_vdx": {
				Type: "lookup_hash_unique",
				Params: map[string]string{
					"table":      "t4_music_lookup",
					"from":       "music_id",
					"to":         "user_id",
					"autocommit": "true",
				},
				Owner: "t4_music",
			},
			"upsert_primary": {
				Type: "lookup_hash_unique",
				Params: map[string]string{
					"table":      "upsert_primary",
					"from":       "pk",
					"to":         "ksnum_id",
					"autocommit": "true",
				},
			},
			"upsert_owned": {
				Type: "lookup_hash_unique",
				Params: map[string]string{
					"table":      "upsert_owned",
					"from":       "owned",
					"to":         "ksnum_id",
					"autocommit": "false",
				},
				Owner: "upsert",
			"twopc_lookup_vdx": {
				Type: "lookup_hash_unique",
				Params: map[string]string{
					"table":      "twopc_lookup",
					"from":       "name",
					"to":         "id",
					"autocommit": "true",
				},
				Owner: "twopc_user",
			},
		},
		Tables: map[string]*vschemapb.Table{
			"t1": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "id1",
					Name:   "hash",
				}, {
					Column: "id2",
					Name:   "t1_id2_vdx",
				}},
			},
			"t1_id2_idx": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "id2",
					Name:   "hash",
				}},
			},
			"t2": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "id3",
					Name:   "hash",
				}, {
					Column: "id4",
					Name:   "t2_id4_idx",
				}},
			},
			"t2_id4_idx": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "id4",
					Name:   "hash",
				}},
			},
			"vstream_test": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "id",
					Name:   "hash",
				}},
			},
			"aggr_test": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "id",
					Name:   "hash",
				}},
				Columns: []*vschemapb.Column{{
					Name: "val1",
					Type: sqltypes.VarChar,
				}},
			},
			"user": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "id",
					Name:   "hash",
				}},
				Columns: []*vschemapb.Column{{
					Name: "name",
					Type: sqltypes.VarChar,
				}},
			},
			"user_details": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "user_id",
					Name:   "hash",
				}},
				Columns: []*vschemapb.Column{{
					Name: "email",
					Type: sqltypes.VarChar,
				}},
			},
			"user_info": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "name",
					Name:   "unicode_hash",
				}},
				Columns: []*vschemapb.Column{{
					Name: "info",
					Type: sqltypes.VarChar,
				}},
			},
			"t3": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "user_id",
					Name:   "hash",
				}, {
					Column: "lastname",
					Name:   "t3_lastname_map_vdx",
				}, {
					Column: "address",
					Name:   "t3_address_map_vdx",
				}},
			},
			"t3_lastname_map": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "user_id",
					Name:   "hash",
				}},
			},
			"t3_address_map": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "user_id",
					Name:   "hash",
				}},
			},
			"t4_music": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "user_id",
					Name:   "hash",
				}, {
					Column: "id",
					Name:   "t4_music_lookup_vdx",
				}},
			},
			"t4_music_art": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "music_id",
					Name:   "t4_music_lookup_vdx",
				}, {
					Column: "user_id",
					Name:   "hash",
				}},
			},
			"t4_music_lookup": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "user_id",
					Name:   "hash",
				}},
			},
			"upsert": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "pk",
					Name:   "upsert_primary",
				}, {
					Column: "owned",
					Name:   "upsert_owned",
				}, {
					Column: "user_id",
					Name:   "hash",
				}},
			},
			"upsert_primary": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "pk",
					Name:   "hash",
				}},
			},
			"upsert_owned": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "owned",
					Name:   "hash",
				}},
			},
			"vt_user": {
        ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "id",
					Name:   "hash",
				}},
			},
			"twopc_user": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "user_id",
					Name:   "hash",
				}, {
					Column: "name",
					Name:   "twopc_lookup_vdx",
				}},
			},
			"twopc_lookup": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "id",
					Name:   "hash",
				}},
			},
		},
	}
)

func TestMain(m *testing.M) {
	flag.Parse()

	exitCode := func() int {
		var cfg vttest.Config
		cfg.Topology = &vttestpb.VTTestTopology{
			Keyspaces: []*vttestpb.Keyspace{{
				Name: "ks",
				Shards: []*vttestpb.Shard{{
					Name: "-80",
				}, {
					Name: "80-",
				}},
			}},
		}
		cfg.ExtraMyCnf = []string{path.Join(os.Getenv("VTTOP"), "config/mycnf/rbr.cnf")}
		if err := cfg.InitSchemas("ks", schema, vschema); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.RemoveAll(cfg.SchemaDir)
			return 1
		}
		defer os.RemoveAll(cfg.SchemaDir)

		cfg.TabletHostName = *tabletHostName

		cluster = &vttest.LocalCluster{
			Config: cfg,
		}
		if err := cluster.Setup(); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			cluster.TearDown()
			return 1
		}
		defer cluster.TearDown()

		vtParams = mysql.ConnParams{
			Host: "localhost",
			Port: cluster.Env.PortForProtocol("vtcombo_mysql_port", ""),
		}
		mysqlParams = cluster.MySQLConnParams()
		grpcAddress = fmt.Sprintf("localhost:%d", cluster.Env.PortForProtocol("vtcombo", "grpc"))

		return m.Run()
	}()
	os.Exit(exitCode)
}
