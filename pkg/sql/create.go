// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package sql

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/coltypes"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/transform"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/pkg/errors"
)

type createDatabaseNode struct {
	n *tree.CreateDatabase
}

// CreateDatabase creates a database.
// Privileges: security.RootUser user.
//   Notes: postgres requires superuser or "CREATEDB".
//          mysql uses the mysqladmin command.
func (p *planner) CreateDatabase(n *tree.CreateDatabase) (planNode, error) {
	if n.Name == "" {
		return nil, errEmptyDatabaseName
	}

	if tmpl := n.Template; tmpl != "" {
		// See https://www.postgresql.org/docs/current/static/manage-ag-templatedbs.html
		if !strings.EqualFold(tmpl, "template0") {
			return nil, fmt.Errorf("unsupported template: %s", tmpl)
		}
	}

	if enc := n.Encoding; enc != "" {
		// We only support UTF8 (and aliases for UTF8).
		if !(strings.EqualFold(enc, "UTF8") ||
			strings.EqualFold(enc, "UTF-8") ||
			strings.EqualFold(enc, "UNICODE")) {
			return nil, fmt.Errorf("unsupported encoding: %s", enc)
		}
	}

	if col := n.Collate; col != "" {
		// We only support C and C.UTF-8.
		if col != "C" && col != "C.UTF-8" {
			return nil, fmt.Errorf("unsupported collation: %s", col)
		}
	}

	if ctype := n.CType; ctype != "" {
		// We only support C and C.UTF-8.
		if ctype != "C" && ctype != "C.UTF-8" {
			return nil, fmt.Errorf("unsupported character classification: %s", ctype)
		}
	}

	if err := p.RequireSuperUser("CREATE DATABASE"); err != nil {
		return nil, err
	}

	return &createDatabaseNode{n: n}, nil
}

func (n *createDatabaseNode) Start(params runParams) error {
	desc := makeDatabaseDesc(n.n)

	created, err := params.p.createDatabase(params.ctx, &desc, n.n.IfNotExists)
	if err != nil {
		return err
	}
	if created {
		// Log Create Database event. This is an auditable log event and is
		// recorded in the same transaction as the table descriptor update.
		if err := MakeEventLogger(params.p.LeaseMgr()).InsertEventRecord(
			params.ctx,
			params.p.txn,
			EventLogCreateDatabase,
			int32(desc.ID),
			int32(params.p.evalCtx.NodeID),
			struct {
				DatabaseName string
				Statement    string
				User         string
			}{n.n.Name.String(), n.n.String(), params.p.session.User},
		); err != nil {
			return err
		}
		params.p.session.tables.addUncommittedDatabase(
			desc.Name, desc.ID, false /* dropped */)
	}
	return nil
}

func (*createDatabaseNode) Next(runParams) (bool, error) { return false, nil }
func (*createDatabaseNode) Close(context.Context)        {}
func (*createDatabaseNode) Values() tree.Datums          { return tree.Datums{} }

type createIndexNode struct {
	n         *tree.CreateIndex
	tableDesc *sqlbase.TableDescriptor
}

// CreateIndex creates an index.
// Privileges: CREATE on table.
//   notes: postgres requires CREATE on the table.
//          mysql requires INDEX on the table.
func (p *planner) CreateIndex(ctx context.Context, n *tree.CreateIndex) (planNode, error) {
	tn, err := n.Table.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	tableDesc, err := MustGetTableDesc(ctx, p.txn, p.getVirtualTabler(), tn, true /*allowAdding*/)
	if err != nil {
		return nil, err
	}

	if err := p.CheckPrivilege(tableDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	return &createIndexNode{tableDesc: tableDesc, n: n}, nil
}

func (n *createIndexNode) Start(params runParams) error {
	_, dropped, err := n.tableDesc.FindIndexByName(string(n.n.Name))
	if err == nil {
		if dropped {
			return fmt.Errorf("index %q being dropped, try again later", string(n.n.Name))
		}
		if n.n.IfNotExists {
			return nil
		}
	}

	indexDesc := sqlbase.IndexDescriptor{
		Name:             string(n.n.Name),
		Unique:           n.n.Unique,
		StoreColumnNames: n.n.Storing.ToStrings(),
	}
	if err := indexDesc.FillColumns(n.n.Columns); err != nil {
		return err
	}
	if n.n.PartitionBy != nil {
		if err := addPartitionedBy(
			params.ctx, &params.p.evalCtx, n.tableDesc, &indexDesc, &indexDesc.Partitioning,
			n.n.PartitionBy, 0, /* colOffset */
		); err != nil {
			return err
		}
	}

	mutationIdx := len(n.tableDesc.Mutations)
	if err := n.tableDesc.AddIndexMutation(indexDesc, sqlbase.DescriptorMutation_ADD); err != nil {
		return err
	}
	if err := n.tableDesc.AllocateIDs(); err != nil {
		return err
	}

	if n.n.Interleave != nil {
		index := n.tableDesc.Mutations[mutationIdx].GetIndex()
		if err := params.p.addInterleave(params.ctx, n.tableDesc, index, n.n.Interleave); err != nil {
			return err
		}
		if err := params.p.finalizeInterleave(params.ctx, n.tableDesc, *index); err != nil {
			return err
		}
	}

	mutationID, err := params.p.createSchemaChangeJob(params.ctx, n.tableDesc,
		tree.AsStringWithFlags(n.n, tree.FmtSimpleQualified))
	if err != nil {
		return err
	}
	if err := params.p.writeTableDesc(params.ctx, n.tableDesc); err != nil {
		return err
	}

	// Record index creation in the event log. This is an auditable log
	// event and is recorded in the same transaction as the table descriptor
	// update.
	if err := MakeEventLogger(params.p.LeaseMgr()).InsertEventRecord(
		params.ctx,
		params.p.txn,
		EventLogCreateIndex,
		int32(n.tableDesc.ID),
		int32(params.p.evalCtx.NodeID),
		struct {
			TableName  string
			IndexName  string
			Statement  string
			User       string
			MutationID uint32
		}{n.tableDesc.Name, n.n.Name.String(), n.n.String(), params.p.session.User, uint32(mutationID)},
	); err != nil {
		return err
	}
	params.p.notifySchemaChange(n.tableDesc, mutationID)

	return nil
}

func (*createIndexNode) Next(runParams) (bool, error) { return false, nil }
func (*createIndexNode) Close(context.Context)        {}
func (*createIndexNode) Values() tree.Datums          { return tree.Datums{} }

type userAuthInfo struct {
	name     func() (string, error)
	password func() (string, error)
}

type createUserNode struct {
	ifNotExists  bool
	rowsAffected int
	userAuthInfo
}

func (p *planner) getUserAuthInfo(nameE, passwordE tree.Expr, ctx string) (userAuthInfo, error) {
	name, err := p.TypeAsString(nameE, ctx)
	if err != nil {
		return userAuthInfo{}, err
	}
	var password func() (string, error)
	if passwordE != nil {
		password, err = p.TypeAsString(passwordE, ctx)
		if err != nil {
			return userAuthInfo{}, err
		}
	}
	return userAuthInfo{name: name, password: password}, nil
}

// resolve returns the actual user name and (hashed) password.
func (ua *userAuthInfo) resolve() (string, []byte, error) {
	name, err := ua.name()
	if err != nil {
		return "", nil, err
	}
	if name == "" {
		return "", nil, errNoUserNameSpecified
	}
	normalizedUsername, err := NormalizeAndValidateUsername(name)
	if err != nil {
		return "", nil, err
	}

	var hashedPassword []byte
	if ua.password != nil {
		resolvedPassword, err := ua.password()
		if err != nil {
			return "", nil, err
		}
		if resolvedPassword == "" {
			return "", nil, security.ErrEmptyPassword
		}

		hashedPassword, err = security.HashPassword(resolvedPassword)
		if err != nil {
			return "", nil, err
		}
	}

	return normalizedUsername, hashedPassword, nil
}

// CreateUser creates a user.
// Privileges: INSERT on system.users.
//   notes: postgres allows the creation of users with an empty password. We do
//          as well, but disallow password authentication for these users.
func (p *planner) CreateUser(ctx context.Context, n *tree.CreateUser) (planNode, error) {
	tDesc, err := getTableDesc(ctx, p.txn, p.getVirtualTabler(), &tree.TableName{DatabaseName: "system", TableName: "users"})
	if err != nil {
		return nil, err
	}

	if err := p.CheckPrivilege(tDesc, privilege.INSERT); err != nil {
		return nil, err
	}

	ua, err := p.getUserAuthInfo(n.Name, n.Password, "CREATE USER")
	if err != nil {
		return nil, err
	}

	return &createUserNode{
		userAuthInfo: ua,
		ifNotExists:  n.IfNotExists,
	}, nil
}

const usernameHelp = "usernames are case insensitive, must start with a letter " +
	"or underscore, may contain letters, digits or underscores, and must not exceed 63 characters"

var usernameRE = regexp.MustCompile(`^[\p{Ll}_][\p{Ll}0-9_]{0,62}$`)

var blacklistedUsernames = map[string]struct{}{
	security.NodeUser: {},
}

// NormalizeAndValidateUsername case folds the specified username and verifies
// it validates according to the usernameRE regular expression.
func NormalizeAndValidateUsername(username string) (string, error) {
	username = tree.Name(username).Normalize()
	if !usernameRE.MatchString(username) {
		return "", errors.Errorf("username %q invalid; %s", username, usernameHelp)
	}
	if _, ok := blacklistedUsernames[username]; ok {
		return "", errors.Errorf("username %q reserved", username)
	}
	return username, nil
}

var errNoUserNameSpecified = errors.New("no username specified")

func (n *createUserNode) Start(params runParams) error {
	normalizedUsername, hashedPassword, err := n.userAuthInfo.resolve()
	if err != nil {
		return err
	}

	internalExecutor := InternalExecutor{LeaseManager: params.p.LeaseMgr()}
	n.rowsAffected, err = internalExecutor.ExecuteStatementInTransaction(
		params.ctx,
		"create-user",
		params.p.txn,
		"INSERT INTO system.users VALUES ($1, $2);",
		normalizedUsername,
		hashedPassword,
	)
	if err != nil {
		if sqlbase.IsUniquenessConstraintViolationError(err) {
			if n.ifNotExists {
				// INSERT only detects error at the end of each batch.  This
				// means perhaps the count by ExecuteStatementInTransactions
				// will have reported updated rows even though an error was
				// encountered.  If the error was due to a duplicate entry, we
				// are not actually inserting anything but are canceling the
				// error, so clear the row count so that the client can learn
				// what's going on.
				n.rowsAffected = 0
				return nil
			}
			err = errors.Errorf("user %s already exists", normalizedUsername)
		}
		return err
	} else if n.rowsAffected != 1 {
		return errors.Errorf(
			"%d rows affected by user creation; expected exactly one row affected", n.rowsAffected,
		)
	}

	return nil
}

func (n *createUserNode) FastPathResults() (int, bool) { return n.rowsAffected, true }
func (*createUserNode) Next(runParams) (bool, error)   { return false, nil }
func (*createUserNode) Close(context.Context)          {}
func (*createUserNode) Values() tree.Datums            { return tree.Datums{} }

// alterUserSetPasswordNode represents an ALTER USER ... WITH PASSWORD statement.
type alterUserSetPasswordNode struct {
	userAuthInfo
	ifExists     bool
	rowsAffected int
}

func (p *planner) AlterUserSetPassword(
	ctx context.Context, n *tree.AlterUserSetPassword,
) (planNode, error) {
	tDesc, err := getTableDesc(ctx, p.txn, p.getVirtualTabler(), &tree.TableName{DatabaseName: "system", TableName: "users"})
	if err != nil {
		return nil, err
	}

	if err := p.CheckPrivilege(tDesc, privilege.UPDATE); err != nil {
		return nil, err
	}

	ua, err := p.getUserAuthInfo(n.Name, n.Password, "ALTER USER")
	if err != nil {
		return nil, err
	}

	return &alterUserSetPasswordNode{
		userAuthInfo: ua,
		ifExists:     n.IfExists,
	}, nil
}

func (n *alterUserSetPasswordNode) FastPathResults() (int, bool) {
	return n.rowsAffected, true
}

func (n *alterUserSetPasswordNode) Start(params runParams) error {
	normalizedUsername, hashedPassword, err := n.userAuthInfo.resolve()
	if err != nil {
		return err
	}

	internalExecutor := InternalExecutor{LeaseManager: params.p.LeaseMgr()}
	n.rowsAffected, err = internalExecutor.ExecuteStatementInTransaction(
		params.ctx,
		"create-user",
		params.p.txn,
		`UPDATE system.users SET "hashedPassword" = $2 WHERE username = $1`,
		normalizedUsername,
		hashedPassword,
	)
	if err != nil {
		return err
	}
	if n.rowsAffected == 0 && !n.ifExists {
		return errors.Errorf("user %s does not exist", normalizedUsername)
	}
	return err
}

func (*alterUserSetPasswordNode) Next(runParams) (bool, error) { return false, nil }
func (*alterUserSetPasswordNode) Close(context.Context)        {}
func (*alterUserSetPasswordNode) Values() tree.Datums          { return tree.Datums{} }

// createViewNode represents a CREATE VIEW statement.
type createViewNode struct {
	p             *planner
	n             *tree.CreateView
	dbDesc        *sqlbase.DatabaseDescriptor
	sourceColumns sqlbase.ResultColumns
	// planDeps tracks which tables and views the view being created
	// depends on. This is collected during the construction of
	// the view query's logical plan.
	planDeps planDependencies
}

// CreateView creates a view.
// Privileges: CREATE on database plus SELECT on all the selected columns.
//   notes: postgres requires CREATE on database plus SELECT on all the
//						selected columns.
//          mysql requires CREATE VIEW plus SELECT on all the selected columns.
func (p *planner) CreateView(ctx context.Context, n *tree.CreateView) (planNode, error) {
	name, err := n.Name.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	dbDesc, err := MustGetDatabaseDesc(ctx, p.txn, p.getVirtualTabler(), name.Database())
	if err != nil {
		return nil, err
	}

	if err := p.CheckPrivilege(dbDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	// Ensure that all the table names are properly qualified.  The
	// traversal will update the NormalizableTableNames in-place, so the
	// changes are persisted in n.AsSource. We use tree.FormatNode
	// merely as a traversal method; its output buffer is discarded
	// immediately after the traversal because it is not needed further.
	var queryBuf bytes.Buffer
	var fmtErr error
	tree.FormatNode(
		&queryBuf,
		tree.FmtReformatTableNames(
			tree.FmtParsable,
			func(t *tree.NormalizableTableName, buf *bytes.Buffer, f tree.FmtFlags) {
				tn, err := p.QualifyWithDatabase(ctx, t)
				if err != nil {
					log.Warningf(ctx, "failed to qualify table name %q with database name: %v",
						tree.ErrString(t), err)
					fmtErr = err
					return
				}
				// Persist the database prefix expansion.
				tn.DBNameOriginallyOmitted = false
			},
		),
		n.AsSource,
	)
	if fmtErr != nil {
		return nil, fmtErr
	}

	planDeps, sourceColumns, err := p.analyzeViewQuery(ctx, n.AsSource)
	if err != nil {
		return nil, err
	}

	numColNames := len(n.ColumnNames)
	numColumns := len(sourceColumns)
	if numColNames != 0 && numColNames != numColumns {
		return nil, sqlbase.NewSyntaxError(fmt.Sprintf(
			"CREATE VIEW specifies %d column name%s, but data source has %d column%s",
			numColNames, util.Pluralize(int64(numColNames)),
			numColumns, util.Pluralize(int64(numColumns))))
	}

	log.VEventf(ctx, 2, "collected view dependencies:\n%s", planDeps.String())

	return &createViewNode{
		p:             p,
		n:             n,
		dbDesc:        dbDesc,
		sourceColumns: sourceColumns,
		planDeps:      planDeps,
	}, nil
}

func (n *createViewNode) Start(params runParams) error {
	viewName := n.n.Name.TableName().Table()
	tKey := tableKey{parentID: n.dbDesc.ID, name: viewName}
	key := tKey.Key()
	if exists, err := descExists(params.ctx, n.p.txn, key); err == nil && exists {
		// TODO(a-robinson): Support CREATE OR REPLACE commands.
		return sqlbase.NewRelationAlreadyExistsError(tKey.Name())
	} else if err != nil {
		return err
	}

	id, err := GenerateUniqueDescID(params.ctx, n.p.session.execCfg.DB)
	if err != nil {
		return nil
	}

	// Inherit permissions from the database descriptor.
	privs := n.dbDesc.GetPrivileges()

	desc, err := n.makeViewTableDesc(
		params.ctx,
		viewName,
		n.n.ColumnNames,
		n.dbDesc.ID,
		id,
		n.sourceColumns,
		privs,
		&n.p.evalCtx,
	)
	if err != nil {
		return err
	}

	if err = desc.ValidateTable(); err != nil {
		return err
	}

	// Collect all the tables/views this view depends on.
	for backrefID := range n.planDeps {
		desc.DependsOn = append(desc.DependsOn, backrefID)
	}

	if err = n.p.createDescriptorWithID(params.ctx, key, id, &desc); err != nil {
		return err
	}

	// Persist the back-references in all referenced table descriptors.
	for _, updated := range n.planDeps {
		backrefDesc := *updated.desc
		for _, dep := range updated.deps {
			// The logical plan constructor merely registered the dependencies.
			// It did not populate the "ID" field of TableDescriptor_Reference,
			// because the ID of the newly created view descriptor was not
			// yet known.
			// We need to do it here.
			dep.ID = desc.ID
			backrefDesc.DependedOnBy = append(backrefDesc.DependedOnBy, dep)
		}
		if err := n.p.saveNonmutationAndNotify(params.ctx, &backrefDesc); err != nil {
			return err
		}
	}

	if desc.Adding() {
		n.p.notifySchemaChange(&desc, sqlbase.InvalidMutationID)
	}
	if err := desc.Validate(params.ctx, n.p.txn); err != nil {
		return err
	}

	// Log Create View event. This is an auditable log event and is
	// recorded in the same transaction as the table descriptor update.
	return MakeEventLogger(n.p.LeaseMgr()).InsertEventRecord(
		params.ctx,
		n.p.txn,
		EventLogCreateView,
		int32(desc.ID),
		int32(n.p.evalCtx.NodeID),
		struct {
			ViewName  string
			Statement string
			User      string
		}{n.n.Name.String(), n.n.String(), n.p.session.User},
	)
}

func (n *createViewNode) Close(ctx context.Context) {}

func (*createViewNode) Next(runParams) (bool, error) { return false, nil }
func (*createViewNode) Values() tree.Datums          { return tree.Datums{} }

type createTableNode struct {
	n          *tree.CreateTable
	dbDesc     *sqlbase.DatabaseDescriptor
	sourcePlan planNode
	count      int
}

// CreateTable creates a table.
// Privileges: CREATE on database.
//   Notes: postgres/mysql require CREATE on database.
func (p *planner) CreateTable(ctx context.Context, n *tree.CreateTable) (planNode, error) {
	tn, err := n.Table.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	dbDesc, err := MustGetDatabaseDesc(ctx, p.txn, p.getVirtualTabler(), tn.Database())
	if err != nil {
		return nil, err
	}

	if err := p.CheckPrivilege(dbDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	HoistConstraints(n)
	for _, def := range n.Defs {
		switch t := def.(type) {
		case *tree.ForeignKeyConstraintTableDef:
			if _, err := t.Table.NormalizeWithDatabaseName(p.session.Database); err != nil {
				return nil, err
			}
		}
	}

	var sourcePlan planNode
	if n.As() {
		// The sourcePlan is needed to determine the set of columns to use
		// to populate the new table descriptor in Start() below. We
		// instantiate the sourcePlan as early as here so that EXPLAIN has
		// something useful to show about CREATE TABLE .. AS ...
		sourcePlan, err = p.Select(ctx, n.AsSource, []types.T{})
		if err != nil {
			return nil, err
		}
		numColNames := len(n.AsColumnNames)
		numColumns := len(planColumns(sourcePlan))
		if numColNames != 0 && numColNames != numColumns {
			sourcePlan.Close(ctx)
			return nil, sqlbase.NewSyntaxError(fmt.Sprintf(
				"CREATE TABLE specifies %d column name%s, but data source has %d column%s",
				numColNames, util.Pluralize(int64(numColNames)),
				numColumns, util.Pluralize(int64(numColumns))))
		}
	}

	return &createTableNode{n: n, dbDesc: dbDesc, sourcePlan: sourcePlan}, nil
}

// HoistConstraints finds column constraints defined inline with the columns
// and moves them into n.Defs as constraints. For example, the foreign key
// constraint in `CREATE TABLE foo (a INT REFERENCES bar(a))` gets pulled into
// a top level fk constraint like
// `CREATE TABLE foo (a int CONSTRAINT .. FOREIGN KEY(a) REFERENCES bar(a)`.
func HoistConstraints(n *tree.CreateTable) {
	for _, d := range n.Defs {
		if col, ok := d.(*tree.ColumnTableDef); ok {
			for _, checkExpr := range col.CheckExprs {
				n.Defs = append(n.Defs,
					&tree.CheckConstraintTableDef{
						Expr: checkExpr.Expr,
						Name: checkExpr.ConstraintName,
					},
				)
			}
			col.CheckExprs = nil
			if col.HasFKConstraint() {
				var targetCol tree.NameList
				if col.References.Col != "" {
					targetCol = append(targetCol, col.References.Col)
				}
				n.Defs = append(n.Defs, &tree.ForeignKeyConstraintTableDef{
					Table:    col.References.Table,
					FromCols: tree.NameList{col.Name},
					ToCols:   targetCol,
					Name:     col.References.ConstraintName,
					Actions:  col.References.Actions,
				})
				col.References.Table = tree.NormalizableTableName{}
			}
		}
	}
}

func (n *createTableNode) Start(params runParams) error {
	tKey := tableKey{parentID: n.dbDesc.ID, name: n.n.Table.TableName().Table()}
	key := tKey.Key()
	if exists, err := descExists(params.ctx, params.p.txn, key); err == nil && exists {
		if n.n.IfNotExists {
			return nil
		}
		return sqlbase.NewRelationAlreadyExistsError(tKey.Name())
	} else if err != nil {
		return err
	}

	id, err := GenerateUniqueDescID(params.ctx, params.p.session.execCfg.DB)
	if err != nil {
		return err
	}

	// If a new system table is being created (which should only be doable by
	// an internal user account), make sure it gets the correct privileges.
	privs := n.dbDesc.GetPrivileges()
	if n.dbDesc.ID == keys.SystemDatabaseID {
		privs = sqlbase.NewDefaultPrivilegeDescriptor()
	}

	var desc sqlbase.TableDescriptor
	var affected map[sqlbase.ID]*sqlbase.TableDescriptor
	creationTime := params.p.txn.OrigTimestamp()
	if n.n.As() {
		desc, err = makeTableDescIfAs(n.n, n.dbDesc.ID, id, creationTime, planColumns(n.sourcePlan), privs, &params.p.semaCtx, &params.p.evalCtx)
	} else {
		affected = make(map[sqlbase.ID]*sqlbase.TableDescriptor)
		desc, err = params.p.makeTableDesc(params.ctx, n.n, n.dbDesc.ID, id, creationTime, privs, affected)
	}
	if err != nil {
		return err
	}

	// We need to validate again after adding the FKs.
	// Only validate the table because backreferences aren't created yet.
	// Everything is validated below.
	err = desc.ValidateTable()
	if err != nil {
		return err
	}

	if err := params.p.createDescriptorWithID(params.ctx, key, id, &desc); err != nil {
		return err
	}

	for _, updated := range affected {
		if err := params.p.saveNonmutationAndNotify(params.ctx, updated); err != nil {
			return err
		}
	}
	if desc.Adding() {
		params.p.notifySchemaChange(&desc, sqlbase.InvalidMutationID)
	}

	for _, index := range desc.AllNonDropIndexes() {
		if len(index.Interleave.Ancestors) > 0 {
			if err := params.p.finalizeInterleave(params.ctx, &desc, index); err != nil {
				return err
			}
		}
	}

	if err := desc.Validate(params.ctx, params.p.txn); err != nil {
		return err
	}

	// Log Create Table event. This is an auditable log event and is
	// recorded in the same transaction as the table descriptor update.
	if err := MakeEventLogger(params.p.LeaseMgr()).InsertEventRecord(
		params.ctx,
		params.p.txn,
		EventLogCreateTable,
		int32(desc.ID),
		int32(params.p.evalCtx.NodeID),
		struct {
			TableName string
			Statement string
			User      string
		}{n.n.Table.String(), n.n.String(), params.p.session.User},
	); err != nil {
		return err
	}

	if n.n.As() {
		// TODO(knz): Ideally we would want to plug the sourcePlan which
		// was already computed as a data source into the insertNode. Now
		// unfortunately this is not so easy: when this point is reached,
		// expandPlan() has already been called on sourcePlan (for
		// EXPLAIN), and expandPlan() on insertPlan (via optimizePlan)
		// below would cause a 2nd invocation and cause a panic. So
		// instead we close this sourcePlan and let the insertNode create
		// it anew from the AsSource syntax node.
		n.sourcePlan.Close(params.ctx)
		n.sourcePlan = nil

		insert := &tree.Insert{
			Table:     &n.n.Table,
			Rows:      n.n.AsSource,
			Returning: tree.AbsentReturningClause,
		}
		insertPlan, err := params.p.Insert(params.ctx, insert, nil /* desiredTypes */)
		if err != nil {
			return err
		}
		defer insertPlan.Close(params.ctx)
		insertPlan, err = params.p.optimizePlan(params.ctx, insertPlan, allColumns(insertPlan))
		if err != nil {
			return err
		}
		if err = params.p.startPlan(params.ctx, insertPlan); err != nil {
			return err
		}
		// This driver function call is done here instead of in the Next
		// method since CREATE TABLE is a DDL statement and Executor only
		// runs Next() for statements with type "Rows".
		count, err := countRowsAffected(params, insertPlan)
		if err != nil {
			return err
		}
		// Passing the affected rows num back
		n.count = count
	}
	return nil
}

func (n *createTableNode) Close(ctx context.Context) {
	if n.sourcePlan != nil {
		n.sourcePlan.Close(ctx)
		n.sourcePlan = nil
	}
}

func (*createTableNode) Next(runParams) (bool, error) { return false, nil }
func (*createTableNode) Values() tree.Datums          { return tree.Datums{} }

type indexMatch bool

const (
	matchExact  indexMatch = true
	matchPrefix indexMatch = false
)

// Referenced cols must be unique, thus referenced indexes must match exactly.
// Referencing cols have no uniqueness requirement and thus may match a strict
// prefix of an index.
func matchesIndex(
	cols []sqlbase.ColumnDescriptor, idx sqlbase.IndexDescriptor, exact indexMatch,
) bool {
	if len(cols) > len(idx.ColumnIDs) || (exact && len(cols) != len(idx.ColumnIDs)) {
		return false
	}

	for i := range cols {
		if cols[i].ID != idx.ColumnIDs[i] {
			return false
		}
	}
	return true
}

func (p *planner) resolveFK(
	ctx context.Context,
	tbl *sqlbase.TableDescriptor,
	d *tree.ForeignKeyConstraintTableDef,
	backrefs map[sqlbase.ID]*sqlbase.TableDescriptor,
	mode sqlbase.ConstraintValidity,
) error {
	return resolveFK(ctx, p.txn, &p.session.virtualSchemas, tbl, d, backrefs, mode)
}

// resolveFK looks up the tables and columns mentioned in a `REFERENCES`
// constraint and adds metadata representing that constraint to the descriptor.
// It may, in doing so, add to or alter descriptors in the passed in `backrefs`
// map of other tables that need to be updated when this table is created.
// Constraints that are not known to hold for existing data are created
// "unvalidated", but when table is empty (e.g. during creation), no existing
// data imples no existing violations, and thus the constraint can be created
// without the unvalidated flag.
func resolveFK(
	ctx context.Context,
	txn *client.Txn,
	vt VirtualTabler,
	tbl *sqlbase.TableDescriptor,
	d *tree.ForeignKeyConstraintTableDef,
	backrefs map[sqlbase.ID]*sqlbase.TableDescriptor,
	mode sqlbase.ConstraintValidity,
) error {
	targetTable := d.Table.TableName()
	target, err := getTableDesc(ctx, txn, vt, targetTable)
	if err != nil {
		return err
	}
	// Special-case: self-referencing FKs (i.e. referencing another col in the
	// same table) will reference a table name that doesn't exist yet (since we
	// are creating it).
	if target == nil {
		if targetTable.Table() == tbl.Name {
			target = tbl
		} else {
			return fmt.Errorf("referenced table %q not found", targetTable.String())
		}
	} else {
		// Since this FK is referencing another table, this table must be created in
		// a non-public "ADD" state and made public only after all leases on the
		// other table are updated to include the backref.
		if mode == sqlbase.ConstraintValidity_Validated {
			tbl.State = sqlbase.TableDescriptor_ADD
			if err := tbl.SetUpVersion(); err != nil {
				return err
			}
		}

		// When adding a self-ref FK to an _existing_ table, we want to make sure
		// we edit the same copy.
		if target.ID == tbl.ID {
			target = tbl
		} else {
			// If we resolve the same table more than once, we only want to edit a
			// single instance of it, so replace target with previously resolved table.
			if prev, ok := backrefs[target.ID]; ok {
				target = prev
			} else {
				backrefs[target.ID] = target
			}
		}
	}

	srcCols, err := tbl.FindActiveColumnsByNames(d.FromCols)
	if err != nil {
		return err
	}

	targetColNames := d.ToCols
	// If no columns are specified, attempt to default to PK.
	if len(targetColNames) == 0 {
		targetColNames = make(tree.NameList, len(target.PrimaryIndex.ColumnNames))
		for i, n := range target.PrimaryIndex.ColumnNames {
			targetColNames[i] = tree.Name(n)
		}
	}

	targetCols, err := target.FindActiveColumnsByNames(targetColNames)
	if err != nil {
		return err
	}

	if len(targetCols) != len(srcCols) {
		return fmt.Errorf("%d columns must reference exactly %d columns in referenced table (found %d)",
			len(srcCols), len(srcCols), len(targetCols))
	}

	for i := range srcCols {
		if s, t := srcCols[i], targetCols[i]; s.Type.SemanticType != t.Type.SemanticType {
			return fmt.Errorf("type of %q (%s) does not match foreign key %q.%q (%s)",
				s.Name, s.Type.SemanticType, target.Name, t.Name, t.Type.SemanticType)
		}
	}

	constraintName := string(d.Name)
	if constraintName == "" {
		constraintName = fmt.Sprintf("fk_%s_ref_%s", string(d.FromCols[0]), target.Name)
	}

	var targetIdx *sqlbase.IndexDescriptor
	if matchesIndex(targetCols, target.PrimaryIndex, matchExact) {
		targetIdx = &target.PrimaryIndex
	} else {
		found := false
		// Find the index corresponding to the referenced column.
		for i, idx := range target.Indexes {
			if idx.Unique && matchesIndex(targetCols, idx, matchExact) {
				targetIdx = &target.Indexes[i]
				found = true
				break
			}
		}
		if !found {
			return pgerror.NewErrorf(
				pgerror.CodeInvalidForeignKeyError,
				"there is no unique constraint matching given keys for referenced table %s",
				targetTable.String(),
			)
		}
	}

	if d.Actions.Delete != tree.NoAction &&
		d.Actions.Delete != tree.Restrict {
		feature := fmt.Sprintf("unsupported: ON DELETE %s", d.Actions.Delete)
		return pgerror.Unimplemented(feature, feature)
	}
	if d.Actions.Update != tree.NoAction &&
		d.Actions.Update != tree.Restrict {
		feature := fmt.Sprintf("unsupported: ON UPDATE %s", d.Actions.Update)
		return pgerror.Unimplemented(feature, feature)
	}
	ref := sqlbase.ForeignKeyReference{
		Table:           target.ID,
		Index:           targetIdx.ID,
		Name:            constraintName,
		SharedPrefixLen: int32(len(srcCols)),
		OnDelete:        sqlbase.ForeignKeyReferenceActionValue[d.Actions.Delete],
		OnUpdate:        sqlbase.ForeignKeyReferenceActionValue[d.Actions.Update],
	}

	if mode == sqlbase.ConstraintValidity_Unvalidated {
		ref.Validity = sqlbase.ConstraintValidity_Unvalidated
	}
	backref := sqlbase.ForeignKeyReference{Table: tbl.ID}

	if matchesIndex(srcCols, tbl.PrimaryIndex, matchPrefix) {
		if tbl.PrimaryIndex.ForeignKey.IsSet() {
			return pgerror.NewErrorf(pgerror.CodeInvalidForeignKeyError,
				"columns cannot be used by multiple foreign key constraints")
		}
		tbl.PrimaryIndex.ForeignKey = ref
		backref.Index = tbl.PrimaryIndex.ID
	} else {
		found := false
		for i := range tbl.Indexes {
			if matchesIndex(srcCols, tbl.Indexes[i], matchPrefix) {
				if tbl.Indexes[i].ForeignKey.IsSet() {
					return pgerror.NewErrorf(pgerror.CodeInvalidForeignKeyError,
						"columns cannot be used by multiple foreign key constraints")
				}
				tbl.Indexes[i].ForeignKey = ref
				backref.Index = tbl.Indexes[i].ID
				found = true
				break
			}
		}
		if !found {
			// Avoid unexpected index builds from ALTER TABLE ADD CONSTRAINT.
			if mode == sqlbase.ConstraintValidity_Unvalidated {
				return pgerror.NewErrorf(pgerror.CodeInvalidForeignKeyError,
					"foreign key requires an existing index on columns %s", colNames(srcCols))
			}
			added, err := addIndexForFK(tbl, srcCols, constraintName, ref)
			if err != nil {
				return err
			}
			backref.Index = added
		}
	}
	targetIdx.ReferencedBy = append(targetIdx.ReferencedBy, backref)
	return nil
}

// Adds an index to a table descriptor (that is in the process of being created)
// that will support using `srcCols` as the referencing (src) side of an FK.
func addIndexForFK(
	tbl *sqlbase.TableDescriptor,
	srcCols []sqlbase.ColumnDescriptor,
	constraintName string,
	ref sqlbase.ForeignKeyReference,
) (sqlbase.IndexID, error) {
	// No existing index for the referencing columns found, so we add one.
	idx := sqlbase.IndexDescriptor{
		Name:             fmt.Sprintf("%s_auto_index_%s", tbl.Name, constraintName),
		ColumnNames:      make([]string, len(srcCols)),
		ColumnDirections: make([]sqlbase.IndexDescriptor_Direction, len(srcCols)),
		ForeignKey:       ref,
	}
	for i, c := range srcCols {
		idx.ColumnDirections[i] = sqlbase.IndexDescriptor_ASC
		idx.ColumnNames[i] = c.Name
	}
	if err := tbl.AddIndex(idx, false); err != nil {
		return 0, err
	}
	if err := tbl.AllocateIDs(); err != nil {
		return 0, err
	}

	added := tbl.Indexes[len(tbl.Indexes)-1]

	// Since we just added the index, we can assume it is the last one rather than
	// searching all the indexes again. That said, we sanity check that it matches
	// in case a refactor ever violates that assumption.
	if !matchesIndex(srcCols, added, matchPrefix) {
		panic("no matching index and auto-generated index failed to match")
	}

	return added.ID, nil
}

// colNames converts a []colDesc to a human-readable string for use in error messages.
func colNames(cols []sqlbase.ColumnDescriptor) string {
	var s bytes.Buffer
	s.WriteString(`("`)
	for i, c := range cols {
		if i != 0 {
			s.WriteString(`", "`)
		}
		s.WriteString(c.Name)
	}
	s.WriteString(`")`)
	return s.String()
}

func (p *planner) saveNonmutationAndNotify(ctx context.Context, td *sqlbase.TableDescriptor) error {
	if err := td.SetUpVersion(); err != nil {
		return err
	}
	if err := td.ValidateTable(); err != nil {
		return err
	}
	if err := p.writeTableDesc(ctx, td); err != nil {
		return err
	}
	p.notifySchemaChange(td, sqlbase.InvalidMutationID)
	return nil
}

func (p *planner) addInterleave(
	ctx context.Context,
	desc *sqlbase.TableDescriptor,
	index *sqlbase.IndexDescriptor,
	interleave *tree.InterleaveDef,
) error {
	return addInterleave(ctx, p.txn, &p.session.virtualSchemas, desc, index, interleave, p.session.Database)
}

// addInterleave marks an index as one that is interleaved in some parent data
// according to the given definition.
func addInterleave(
	ctx context.Context,
	txn *client.Txn,
	vt VirtualTabler,
	desc *sqlbase.TableDescriptor,
	index *sqlbase.IndexDescriptor,
	interleave *tree.InterleaveDef,
	sessionDB string,
) error {
	if interleave.DropBehavior != tree.DropDefault {
		return pgerror.UnimplementedWithIssueErrorf(
			7854, "unsupported shorthand %s", interleave.DropBehavior)
	}

	tn, err := interleave.Parent.NormalizeWithDatabaseName(sessionDB)
	if err != nil {
		return err
	}

	parentTable, err := MustGetTableDesc(ctx, txn, vt, tn, true /*allowAdding*/)
	if err != nil {
		return err
	}
	parentIndex := parentTable.PrimaryIndex

	if len(interleave.Fields) != len(parentIndex.ColumnIDs) {
		return fmt.Errorf("interleaved columns must match parent")
	}
	if len(interleave.Fields) > len(index.ColumnIDs) {
		return fmt.Errorf("declared columns must match index being interleaved")
	}
	for i, targetColID := range parentIndex.ColumnIDs {
		targetCol, err := parentTable.FindColumnByID(targetColID)
		if err != nil {
			return err
		}
		col, err := desc.FindColumnByID(index.ColumnIDs[i])
		if err != nil {
			return err
		}
		if string(interleave.Fields[i]) != col.Name {
			return fmt.Errorf("declared columns must match index being interleaved")
		}
		if !col.Type.Equal(targetCol.Type) || index.ColumnDirections[i] != parentIndex.ColumnDirections[i] {
			return fmt.Errorf("interleaved columns must match parent")
		}
	}

	ancestorPrefix := append(
		[]sqlbase.InterleaveDescriptor_Ancestor(nil), parentIndex.Interleave.Ancestors...)
	intl := sqlbase.InterleaveDescriptor_Ancestor{
		TableID:         parentTable.ID,
		IndexID:         parentIndex.ID,
		SharedPrefixLen: uint32(len(parentIndex.ColumnIDs)),
	}
	for _, ancestor := range ancestorPrefix {
		intl.SharedPrefixLen -= ancestor.SharedPrefixLen
	}
	index.Interleave = sqlbase.InterleaveDescriptor{Ancestors: append(ancestorPrefix, intl)}

	desc.State = sqlbase.TableDescriptor_ADD
	return nil
}

// finalizeInterleave creates backreferences from an interleaving parent to the
// child data being interleaved.
func (p *planner) finalizeInterleave(
	ctx context.Context, desc *sqlbase.TableDescriptor, index sqlbase.IndexDescriptor,
) error {
	// TODO(dan): This is similar to finalizeFKs. Consolidate them
	if len(index.Interleave.Ancestors) == 0 {
		return nil
	}
	// Only the last ancestor needs the backreference.
	ancestor := index.Interleave.Ancestors[len(index.Interleave.Ancestors)-1]
	var ancestorTable *sqlbase.TableDescriptor
	if ancestor.TableID == desc.ID {
		ancestorTable = desc
	} else {
		var err error
		ancestorTable, err = sqlbase.GetTableDescFromID(ctx, p.txn, ancestor.TableID)
		if err != nil {
			return err
		}
	}
	ancestorIndex, err := ancestorTable.FindIndexByID(ancestor.IndexID)
	if err != nil {
		return err
	}
	ancestorIndex.InterleavedBy = append(ancestorIndex.InterleavedBy,
		sqlbase.ForeignKeyReference{Table: desc.ID, Index: index.ID})

	if err := p.saveNonmutationAndNotify(ctx, ancestorTable); err != nil {
		return err
	}

	if desc.State == sqlbase.TableDescriptor_ADD {
		desc.State = sqlbase.TableDescriptor_PUBLIC

		if err := p.saveNonmutationAndNotify(ctx, desc); err != nil {
			return err
		}
	}

	return nil
}

// valueEncodePartitionTuple typechecks the datums in maybeTuple, returns the
// concatenation of these datums each encoded using the table "value" encoding.
// The special values of DEFAULT (for list) and MAXVALUE (for range) are encoded
// as NOT NULL.
//
// TODO(dan): The typechecking here should be run during plan construction, so
// we can support placeholders.
func valueEncodePartitionTuple(
	typ tree.PartitionByType,
	evalCtx *tree.EvalContext,
	maybeTuple tree.Expr,
	cols []sqlbase.ColumnDescriptor,
) ([]byte, error) {
	maybeTuple = tree.StripParens(maybeTuple)
	tuple, ok := maybeTuple.(*tree.Tuple)
	if !ok {
		// If we don't already have a tuple, promote whatever we have to a 1-tuple.
		tuple = &tree.Tuple{Exprs: []tree.Expr{maybeTuple}}
	}

	if len(tuple.Exprs) != len(cols) {
		return nil, errors.Errorf("partition has %d columns but %d values were supplied",
			len(cols), len(tuple.Exprs))
	}

	var value, scratch []byte
	for i, expr := range tuple.Exprs {
		switch expr.(type) {
		case tree.DefaultVal:
			if typ != tree.PartitionByList {
				return nil, errors.Errorf("%s cannot be used with PARTITION BY %s", expr, typ)
			}
			// NOT NULL is used to signal DEFAULT.
			value = encoding.EncodeNotNullValue(value, encoding.NoColumnID)
			continue
		case tree.MaxVal:
			if typ != tree.PartitionByRange {
				return nil, errors.Errorf("%s cannot be used with PARTITION BY %s", expr, typ)
			}
			// NOT NULL is used to signal MAXVALUE.
			value = encoding.EncodeNotNullValue(value, encoding.NoColumnID)
			continue
		case *tree.Placeholder:
			return nil, pgerror.UnimplementedWithIssueErrorf(
				19464, "placeholders are not supported in PARTITION BY")
		default:
			// Fall-through.
		}

		typedExpr, err := tree.TypeCheck(expr, nil, cols[i].Type.ToDatumType())
		if err != nil {
			return nil, errors.Wrap(err, expr.String())
		}
		datum, err := typedExpr.Eval(evalCtx)
		if err != nil {
			return nil, errors.Wrap(err, typedExpr.String())
		}
		if err := sqlbase.CheckColumnType(cols[i], datum.ResolvedType(), nil); err != nil {
			return nil, err
		}
		value, err = sqlbase.EncodeTableValue(
			value, sqlbase.ColumnID(encoding.NoColumnID), datum, scratch,
		)
		if err != nil {
			return nil, err
		}
	}
	return value, nil
}

// addPartitionedBy marks an index as one that is partitioned into ranges, each
// addressable by zone configs.
func addPartitionedBy(
	ctx context.Context,
	evalCtx *tree.EvalContext,
	tableDesc *sqlbase.TableDescriptor,
	indexDesc *sqlbase.IndexDescriptor,
	partDesc *sqlbase.PartitioningDescriptor,
	partBy *tree.PartitionBy,
	colOffset int,
) error {
	partDesc.NumColumns = uint32(len(partBy.Fields))

	var cols []sqlbase.ColumnDescriptor
	for i := 0; i < len(partBy.Fields); i++ {
		if colOffset+i >= len(indexDesc.ColumnNames) {
			return errors.New("declared partition columns must match index being partitioned")
		}
		// Search by name because some callsites of this method have not
		// allocated ids yet (so they are still all the 0 value).
		col, err := tableDesc.FindActiveColumnByName(indexDesc.ColumnNames[colOffset+i])
		if err != nil {
			return err
		}
		cols = append(cols, col)
		if string(partBy.Fields[i]) != col.Name {
			return errors.New("declared partition columns must match index being partitioned")
		}
	}

	for _, l := range partBy.List {
		p := sqlbase.PartitioningDescriptor_List{
			Name: l.Name.Normalize(),
		}
		for _, expr := range l.Exprs {
			encodedTuple, err := valueEncodePartitionTuple(
				tree.PartitionByList, evalCtx, expr, cols)
			if err != nil {
				return errors.Wrapf(err, "PARTITION %s", p.Name)
			}
			p.Values = append(p.Values, encodedTuple)
		}
		if l.Subpartition != nil {
			newColOffset := colOffset + int(partDesc.NumColumns)
			if err := addPartitionedBy(
				ctx, evalCtx, tableDesc, indexDesc, &p.Subpartitioning, l.Subpartition, newColOffset,
			); err != nil {
				return err
			}
		}
		partDesc.List = append(partDesc.List, p)
	}
	for _, r := range partBy.Range {
		p := sqlbase.PartitioningDescriptor_Range{
			Name: r.Name.Normalize(),
		}
		encodedTuple, err := valueEncodePartitionTuple(
			tree.PartitionByRange, evalCtx, r.Expr, cols)
		if err != nil {
			return errors.Wrapf(err, "PARTITION %s", p.Name)
		}
		if r.Subpartition != nil {
			return errors.Errorf("PARTITION %s: cannot subpartition a range partition", p.Name)
		}
		p.UpperBound = encodedTuple
		partDesc.Range = append(partDesc.Range, p)
	}

	return nil
}

func initTableDescriptor(
	id, parentID sqlbase.ID,
	name string,
	creationTime hlc.Timestamp,
	privileges *sqlbase.PrivilegeDescriptor,
) sqlbase.TableDescriptor {
	return sqlbase.TableDescriptor{
		ID:               id,
		Name:             name,
		ParentID:         parentID,
		FormatVersion:    sqlbase.InterleavedFormatVersion,
		Version:          1,
		ModificationTime: creationTime,
		Privileges:       privileges,
	}
}

// makeViewTableDesc returns the table descriptor for a new view.
//
// It creates the descriptor directly in the PUBLIC state rather than
// the ADDING state because back-references are added to the view's
// dependencies in the same transaction that the view is created and it
// doesn't matter if reads/writes use a cached descriptor that doesn't
// include the back-references.
func (n *createViewNode) makeViewTableDesc(
	ctx context.Context,
	viewName string,
	columnNames tree.NameList,
	parentID sqlbase.ID,
	id sqlbase.ID,
	resultColumns []sqlbase.ResultColumn,
	privileges *sqlbase.PrivilegeDescriptor,
	evalCtx *tree.EvalContext,
) (sqlbase.TableDescriptor, error) {
	desc := initTableDescriptor(id, parentID, viewName, n.p.txn.OrigTimestamp(), privileges)
	desc.ViewQuery = tree.AsStringWithFlags(n.n.AsSource, tree.FmtParsable)
	for i, colRes := range resultColumns {
		colType, err := coltypes.DatumTypeToColumnType(colRes.Typ)
		if err != nil {
			return desc, err
		}
		columnTableDef := tree.ColumnTableDef{Name: tree.Name(colRes.Name), Type: colType}
		if len(columnNames) > i {
			columnTableDef.Name = columnNames[i]
		}
		col, _, err := sqlbase.MakeColumnDefDescs(&columnTableDef, &n.p.semaCtx, evalCtx)
		if err != nil {
			return desc, err
		}
		desc.AddColumn(*col)
	}

	return desc, desc.AllocateIDs()
}

func (n *createSequenceNode) makeSequenceTableDesc(
	ctx context.Context,
	sequenceName string,
	parentID sqlbase.ID,
	id sqlbase.ID,
	privileges *sqlbase.PrivilegeDescriptor,
) (sqlbase.TableDescriptor, error) {
	desc := initTableDescriptor(id, parentID, sequenceName, n.p.txn.OrigTimestamp(), privileges)

	// Fill in options, starting with defaults then overriding.

	opts := &sqlbase.TableDescriptor_SequenceOpts{
		Cycle:     false,
		Increment: 1,
	}
	err := assignSequenceOptions(opts, n.n.Options, true /* setDefaults */)
	if err != nil {
		return desc, err
	}
	desc.SequenceOpts = opts

	return desc, desc.AllocateIDs()
}

// makeTableDescIfAs is the MakeTableDesc method for when we have a table
// that is created with the CREATE AS format.
func makeTableDescIfAs(
	p *tree.CreateTable,
	parentID, id sqlbase.ID,
	creationTime hlc.Timestamp,
	resultColumns []sqlbase.ResultColumn,
	privileges *sqlbase.PrivilegeDescriptor,
	semaCtx *tree.SemaContext,
	evalCtx *tree.EvalContext,
) (desc sqlbase.TableDescriptor, err error) {
	tableName, err := p.Table.Normalize()
	if err != nil {
		return desc, err
	}
	desc = initTableDescriptor(id, parentID, tableName.Table(), creationTime, privileges)
	for i, colRes := range resultColumns {
		colType, err := coltypes.DatumTypeToColumnType(colRes.Typ)
		if err != nil {
			return desc, err
		}
		columnTableDef := tree.ColumnTableDef{Name: tree.Name(colRes.Name), Type: colType}
		columnTableDef.Nullable.Nullability = tree.SilentNull
		if len(p.AsColumnNames) > i {
			columnTableDef.Name = p.AsColumnNames[i]
		}
		col, _, err := sqlbase.MakeColumnDefDescs(&columnTableDef, semaCtx, evalCtx)
		if err != nil {
			return desc, err
		}
		desc.AddColumn(*col)
	}

	return desc, desc.AllocateIDs()
}

// MakeTableDesc creates a table descriptor from a CreateTable statement.
func MakeTableDesc(
	ctx context.Context,
	txn *client.Txn,
	vt VirtualTabler,
	n *tree.CreateTable,
	parentID, id sqlbase.ID,
	creationTime hlc.Timestamp,
	privileges *sqlbase.PrivilegeDescriptor,
	affected map[sqlbase.ID]*sqlbase.TableDescriptor,
	sessionDB string,
	semaCtx *tree.SemaContext,
	evalCtx *tree.EvalContext,
) (sqlbase.TableDescriptor, error) {
	tableName, err := n.Table.Normalize()
	if err != nil {
		return sqlbase.TableDescriptor{}, err
	}
	desc := initTableDescriptor(id, parentID, tableName.Table(), creationTime, privileges)

	for _, def := range n.Defs {
		if d, ok := def.(*tree.ColumnTableDef); ok {
			if !desc.IsVirtualTable() {
				if _, ok := d.Type.(*coltypes.TVector); ok {
					return desc, pgerror.NewErrorf(
						pgerror.CodeFeatureNotSupportedError,
						"VECTOR column types are unsupported",
					)
				}
			}
			col, idx, err := sqlbase.MakeColumnDefDescs(d, semaCtx, evalCtx)
			if err != nil {
				return desc, err
			}

			desc.AddColumn(*col)
			if idx != nil {
				if err := desc.AddIndex(*idx, d.PrimaryKey); err != nil {
					return desc, err
				}
			}
			if d.HasColumnFamily() {
				// Pass true for `create` and `ifNotExists` because when we're creating
				// a table, we always want to create the specified family if it doesn't
				// exist.
				err := desc.AddColumnToFamilyMaybeCreate(col.Name, string(d.Family.Name), true, true)
				if err != nil {
					return desc, err
				}
			}
		}
	}

	var primaryIndexColumnSet map[string]struct{}
	for _, def := range n.Defs {
		switch d := def.(type) {
		case *tree.ColumnTableDef:
			// pass, handled above.

		case *tree.IndexTableDef:
			idx := sqlbase.IndexDescriptor{
				Name:             string(d.Name),
				StoreColumnNames: d.Storing.ToStrings(),
			}
			if err := idx.FillColumns(d.Columns); err != nil {
				return desc, err
			}
			if d.PartitionBy != nil {
				if err := addPartitionedBy(
					ctx, evalCtx, &desc, &idx, &idx.Partitioning, d.PartitionBy, 0, /* colOffset */
				); err != nil {
					return desc, err
				}
			}
			if err := desc.AddIndex(idx, false); err != nil {
				return desc, err
			}
			if d.Interleave != nil {
				return desc, pgerror.UnimplementedWithIssueError(9148, "use CREATE INDEX to make interleaved indexes")
			}
		case *tree.UniqueConstraintTableDef:
			idx := sqlbase.IndexDescriptor{
				Name:             string(d.Name),
				Unique:           true,
				StoreColumnNames: d.Storing.ToStrings(),
			}
			if err := idx.FillColumns(d.Columns); err != nil {
				return desc, err
			}
			if d.PartitionBy != nil {
				if err := addPartitionedBy(
					ctx, evalCtx, &desc, &idx, &idx.Partitioning, d.PartitionBy, 0, /* colOffset */
				); err != nil {
					return desc, err
				}
			}
			if err := desc.AddIndex(idx, d.PrimaryKey); err != nil {
				return desc, err
			}
			if d.PrimaryKey {
				primaryIndexColumnSet = make(map[string]struct{})
				for _, c := range d.Columns {
					primaryIndexColumnSet[string(c.Column)] = struct{}{}
				}
			}
			if d.Interleave != nil {
				return desc, pgerror.UnimplementedWithIssueError(9148, "use CREATE INDEX to make interleaved indexes")
			}

		case *tree.CheckConstraintTableDef, *tree.ForeignKeyConstraintTableDef, *tree.FamilyTableDef:
			// pass, handled below.

		default:
			return desc, errors.Errorf("unsupported table def: %T", def)
		}
	}

	if primaryIndexColumnSet != nil {
		// Primary index columns are not nullable.
		for i := range desc.Columns {
			if _, ok := primaryIndexColumnSet[desc.Columns[i].Name]; ok {
				desc.Columns[i].Nullable = false
			}
		}
	}

	// Now that all columns are in place, add any explicit families (this is done
	// here, rather than in the constraint pass below since we want to pick up
	// explicit allocations before AllocateIDs adds implicit ones).
	for _, def := range n.Defs {
		if d, ok := def.(*tree.FamilyTableDef); ok {
			fam := sqlbase.ColumnFamilyDescriptor{
				Name:        string(d.Name),
				ColumnNames: d.Columns.ToStrings(),
			}
			desc.AddFamily(fam)
		}
	}

	if err := desc.AllocateIDs(); err != nil {
		return desc, err
	}

	if n.Interleave != nil {
		if err := addInterleave(ctx, txn, vt, &desc, &desc.PrimaryIndex, n.Interleave, sessionDB); err != nil {
			return desc, err
		}
	}

	if n.PartitionBy != nil {
		if err := addPartitionedBy(
			ctx, evalCtx, &desc, &desc.PrimaryIndex, &desc.PrimaryIndex.Partitioning, n.PartitionBy,
			0, /* colOffset */
		); err != nil {
			return desc, err
		}
	}

	// With all structural elements in place and IDs allocated, we can resolve the
	// constraints and qualifications.
	// FKs are resolved after the descriptor is otherwise complete and IDs have
	// been allocated since the FKs will reference those IDs. Resolution also
	// accumulates updates to other tables (adding backreferences) in the passed
	// map -- anything in that map should be saved when the table is created.
	generatedNames := map[string]struct{}{}
	for _, def := range n.Defs {
		switch d := def.(type) {
		case *tree.ColumnTableDef, *tree.IndexTableDef, *tree.UniqueConstraintTableDef, *tree.FamilyTableDef:
			// pass, handled above.

		case *tree.CheckConstraintTableDef:
			ck, err := makeCheckConstraint(desc, d, generatedNames, semaCtx, evalCtx)
			if err != nil {
				return desc, err
			}
			desc.Checks = append(desc.Checks, ck)

		case *tree.ForeignKeyConstraintTableDef:
			if err := resolveFK(ctx, txn, vt, &desc, d, affected, sqlbase.ConstraintValidity_Validated); err != nil {
				return desc, err
			}
		default:
			return desc, errors.Errorf("unsupported table def: %T", def)
		}
	}

	// Multiple FKs from the same column would potentially result in ambiguous or
	// unexpected behavior with conflicting CASCADE/RESTRICT/etc behaviors.
	colsInFKs := make(map[sqlbase.ColumnID]struct{})
	for _, idx := range desc.Indexes {
		if idx.ForeignKey.IsSet() {
			numCols := len(idx.ColumnIDs)
			if idx.ForeignKey.SharedPrefixLen > 0 {
				numCols = int(idx.ForeignKey.SharedPrefixLen)
			}
			for i := 0; i < numCols; i++ {
				if _, ok := colsInFKs[idx.ColumnIDs[i]]; ok {
					return desc, fmt.Errorf(
						"column %q cannot be used by multiple foreign key constraints", idx.ColumnNames[i])
				}
				colsInFKs[idx.ColumnIDs[i]] = struct{}{}
			}
		}
	}

	return desc, desc.AllocateIDs()
}

// makeTableDesc creates a table descriptor from a CreateTable statement.
func (p *planner) makeTableDesc(
	ctx context.Context,
	n *tree.CreateTable,
	parentID, id sqlbase.ID,
	creationTime hlc.Timestamp,
	privileges *sqlbase.PrivilegeDescriptor,
	affected map[sqlbase.ID]*sqlbase.TableDescriptor,
) (sqlbase.TableDescriptor, error) {
	return MakeTableDesc(
		ctx,
		p.txn,
		&p.session.virtualSchemas,
		n,
		parentID,
		id,
		creationTime,
		privileges,
		affected,
		p.session.Database,
		&p.semaCtx,
		&p.evalCtx,
	)
}

// dummyColumnItem is used in makeCheckConstraint to construct an expression
// that can be both type-checked and examined for variable expressions.
type dummyColumnItem struct {
	typ types.T
}

// String implements the Stringer interface.
func (d dummyColumnItem) String() string {
	return fmt.Sprintf("<%s>", d.typ)
}

// Format implements the NodeFormatter interface.
func (d dummyColumnItem) Format(buf *bytes.Buffer, _ tree.FmtFlags) {
	buf.WriteString(d.String())
}

// Walk implements the Expr interface.
func (d dummyColumnItem) Walk(_ tree.Visitor) tree.Expr {
	return d
}

// TypeCheck implements the Expr interface.
func (d dummyColumnItem) TypeCheck(_ *tree.SemaContext, desired types.T) (tree.TypedExpr, error) {
	return d, nil
}

// Eval implements the TypedExpr interface.
func (dummyColumnItem) Eval(_ *tree.EvalContext) (tree.Datum, error) {
	panic("dummyColumnItem.Eval() is undefined")
}

// ResolvedType implements the TypedExpr interface.
func (d dummyColumnItem) ResolvedType() types.T {
	return d.typ
}

func makeCheckConstraint(
	desc sqlbase.TableDescriptor,
	d *tree.CheckConstraintTableDef,
	inuseNames map[string]struct{},
	semaCtx *tree.SemaContext,
	evalCtx *tree.EvalContext,
) (*sqlbase.TableDescriptor_CheckConstraint, error) {
	// CHECK expressions seem to vary across databases. Wikipedia's entry on
	// Check_constraint (https://en.wikipedia.org/wiki/Check_constraint) says
	// that if the constraint refers to a single column only, it is possible to
	// specify the constraint as part of the column definition. Postgres allows
	// specifying them anywhere about any columns, but it moves all constraints to
	// the table level (i.e., columns never have a check constraint themselves). We
	// will adhere to the stricter definition.

	var nameBuf bytes.Buffer
	name := string(d.Name)

	generateName := name == ""
	if generateName {
		nameBuf.WriteString("check")
	}

	preFn := func(expr tree.Expr) (err error, recurse bool, newExpr tree.Expr) {
		vBase, ok := expr.(tree.VarName)
		if !ok {
			// Not a VarName, don't do anything to this node.
			return nil, true, expr
		}

		v, err := vBase.NormalizeVarName()
		if err != nil {
			return err, false, nil
		}

		c, ok := v.(*tree.ColumnItem)
		if !ok {
			return nil, true, expr
		}

		col, err := desc.FindActiveColumnByName(string(c.ColumnName))
		if err != nil {
			return fmt.Errorf("column %q not found for constraint %q",
				c.ColumnName, d.Expr.String()), false, nil
		}
		if generateName {
			nameBuf.WriteByte('_')
			nameBuf.WriteString(col.Name)
		}
		// Convert to a dummy node of the correct type.
		return nil, false, dummyColumnItem{col.Type.ToDatumType()}
	}

	expr, err := tree.SimpleVisit(d.Expr, preFn)
	if err != nil {
		return nil, err
	}

	var t transform.ExprTransformContext
	if err := t.AssertNoAggregationOrWindowing(expr, "CHECK expressions", semaCtx.SearchPath); err != nil {
		return nil, err
	}

	if _, err := sqlbase.SanitizeVarFreeExpr(
		expr, types.Bool, "CHECK", semaCtx, evalCtx,
	); err != nil {
		return nil, err
	}
	if generateName {
		name = nameBuf.String()

		// If generated name isn't unique, attempt to add a number to the end to
		// get a unique name.
		if _, ok := inuseNames[name]; ok {
			i := 1
			for {
				appended := fmt.Sprintf("%s%d", name, i)
				if _, ok := inuseNames[appended]; !ok {
					name = appended
					break
				}
				i++
			}
		}
		if inuseNames != nil {
			inuseNames[name] = struct{}{}
		}
	}
	return &sqlbase.TableDescriptor_CheckConstraint{Expr: tree.Serialize(d.Expr), Name: name}, nil
}

type createSequenceNode struct {
	p      *planner
	n      *tree.CreateSequence
	dbDesc *sqlbase.DatabaseDescriptor
}

func (p *planner) CreateSequence(ctx context.Context, n *tree.CreateSequence) (planNode, error) {
	name, err := n.Name.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	dbDesc, err := MustGetDatabaseDesc(ctx, p.txn, p.getVirtualTabler(), name.Database())
	if err != nil {
		return nil, err
	}

	if err := p.CheckPrivilege(dbDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	return &createSequenceNode{
		p:      p,
		n:      n,
		dbDesc: dbDesc,
	}, nil
}

func (n *createSequenceNode) Start(params runParams) error {
	seqName := n.n.Name.TableName().Table()
	tKey := tableKey{parentID: n.dbDesc.ID, name: seqName}
	key := tKey.Key()
	if exists, err := descExists(params.ctx, n.p.txn, key); err == nil && exists {
		if n.n.IfNotExists {
			// If the sequence exists but the user specified IF NOT EXISTS, return without doing anything.
			return nil
		}
		return sqlbase.NewRelationAlreadyExistsError(tKey.Name())
	} else if err != nil {
		return err
	}

	id, err := GenerateUniqueDescID(params.ctx, n.p.session.execCfg.DB)
	if err != nil {
		return nil
	}

	// Inherit permissions from the database descriptor.
	privs := n.dbDesc.GetPrivileges()

	desc, err := n.makeSequenceTableDesc(params.ctx, seqName, n.dbDesc.ID, id, privs)
	if err != nil {
		return err
	}

	if err = desc.ValidateTable(); err != nil {
		return err
	}

	if err = n.p.createDescriptorWithID(params.ctx, key, id, &desc); err != nil {
		return err
	}

	// Initialize the sequence value.
	seqValueKey := keys.MakeSequenceKey(uint32(id))
	b := &client.Batch{}
	b.Inc(seqValueKey, desc.SequenceOpts.Start-desc.SequenceOpts.Increment)
	if err := n.p.txn.Run(params.ctx, b); err != nil {
		return err
	}

	if desc.Adding() {
		n.p.notifySchemaChange(&desc, sqlbase.InvalidMutationID)
	}
	if err := desc.Validate(params.ctx, n.p.txn); err != nil {
		return err
	}

	// Log Create Sequence event. This is an auditable log event and is
	// recorded in the same transaction as the table descriptor update.
	return MakeEventLogger(n.p.LeaseMgr()).InsertEventRecord(
		params.ctx,
		n.p.txn,
		EventLogCreateSequence,
		int32(desc.ID),
		int32(n.p.evalCtx.NodeID),
		struct {
			SequenceName string
			Statement    string
			User         string
		}{n.n.Name.String(), n.n.String(), n.p.session.User},
	)
}

func (*createSequenceNode) Next(runParams) (bool, error) { return false, nil }
func (*createSequenceNode) Close(context.Context)        {}
func (*createSequenceNode) Values() tree.Datums          { return tree.Datums{} }
