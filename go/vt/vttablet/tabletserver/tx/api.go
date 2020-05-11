/*
Copyright 2020 The Vitess Authors.

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

package tx

import (
	"context"

	"vitess.io/vitess/go/sqltypes"
	querypb "vitess.io/vitess/go/vt/proto/query"
)

type (
	ConnID            = int64
	DTID              = string
	TransactionalConn interface {
		// Executes a query inside the scope of the transaction
		Exec(ctx context.Context, query string, maxrows int, wantfields bool) (*sqltypes.Result, error)

		// Should not be needed
		BeginAgain(ctx context.Context) error

		// String returns a printable version of the connection info.
		String() string
	}
	FuncWithConnection func(TransactionalConn) error
	TransactionEngine  interface {
		// Local transactions
		Begin(ctx context.Context, options *querypb.ExecuteOptions, exec FuncWithConnection) (ConnID, string, error)
		ReserveBegin(ctx context.Context, options *querypb.ExecuteOptions, exec FuncWithConnection, connection ConnID) (ConnID, string, error)
		Reserve(ctx context.Context, options *querypb.ExecuteOptions, setStatements []string, exec FuncWithConnection, connection ConnID) (ConnID, error)

		Exec(ctx context.Context, connection ConnID, exec FuncWithConnection) error
		Commit(ctx context.Context, transactionID ConnID) (string, ConnID, error)
		Rollback(ctx context.Context, transactionID ConnID) (ConnID, error)

		// 2PC Transactions
		Prepare(transactionID ConnID, dtid DTID) error
		CommitPrepared(dtid DTID) error
		RollbackPrepared(dtid DTID, originalID ConnID) error
		CreateTransaction(dtid DTID, participants []*querypb.Target) error
		StartCommit(transactionID ConnID, dtid DTID) error
		SetRollback(dtid DTID, transactionID ConnID) error
		ConcludeTransaction(dtid DTID) error
		ReadTransaction(dtid DTID) (*querypb.TransactionMetadata, error)
		ReadTwopcInflight() (distributed []*DistributedTx, prepared, failed []*PreparedTx, err error)
	}

	//TxEngineStateMachine is used to control the state the transactional engine -
	//whether new connections and/or transactions are allowed or not.
	TxEngineStateMachine interface {
		Init() error
		AcceptReadWrite() error
		AcceptReadOnly() error
		StopGently()
	}

	TrustedConnection interface {
		TransactionalConn
		Release()
		Commit(ctx context.Context)
		Rollback(ctx context.Context)
	}
	TrustedTxEngine interface {
		TrustedBegin(ctx context.Context, options *querypb.ExecuteOptions, connection ConnID) (TrustedConnection, string, error)
		TrustedReserve(ctx context.Context, options *querypb.ExecuteOptions, setStatements []string, connection ConnID) (TrustedConnection, error)
		Get(ctx context.Context, connection ConnID) (TrustedConnection, error)
		Put(ctx context.Context, connection ConnID, conn TrustedConnection) error
	}
)

func DoNothing(TransactionalConn) error { return nil }
