// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package persistence

// TODO(ericsnow) Eliminate the mongo-related imports here.

import (
	"fmt"
	"reflect"

	"github.com/juju/errors"
	"github.com/juju/names"
	jujutxn "github.com/juju/txn"
	"gopkg.in/juju/charm.v5"
	"gopkg.in/mgo.v2/txn"

	"github.com/juju/juju/process"
)

// TODO(ericsnow) Implement persistence using a TXN abstraction (used
// in the business logic) with ops factories available from the
// persistence layer.

// PersistenceBase exposes the core persistence functionality needed
// for workload processes.
type PersistenceBase interface {
	// One populates doc with the document corresponding to the given
	// ID. Missing documents result in errors.NotFound.
	One(collName, id string, doc interface{}) error
	// All populates docs with the list of the documents corresponding
	// to the provided query.
	All(collName string, query, docs interface{}) error
	// Run runs the transaction generated by the provided factory
	// function. It may be retried several times.
	Run(transactions jujutxn.TransactionSource) error
}

// Persistence exposes the high-level persistence functionality
// related to workload processes in Juju.
type Persistence struct {
	st    PersistenceBase
	charm names.CharmTag
	unit  names.UnitTag
}

// NewPersistence builds a new Persistence based on the provided info.
func NewPersistence(st PersistenceBase, charm *names.CharmTag, unit *names.UnitTag) *Persistence {
	pp := &Persistence{
		st: st,
	}
	if charm != nil {
		pp.charm = *charm
	}
	if unit != nil {
		pp.unit = *unit
	}
	return pp
}

// EnsureDefinitions checks persistence to see if records for the
// definitions are already there. If not then they are added. If so then
// they are checked to be sure they match those provided. The lists of
// names for those that already exist and that don't match are returned.
func (pp Persistence) EnsureDefinitions(definitions ...charm.Process) ([]string, []string, error) {
	var found []string
	var mismatched []string

	if len(definitions) == 0 {
		return found, mismatched, nil
	}

	var ids []string
	var ops []txn.Op
	for _, definition := range definitions {
		ids = append(ids, pp.definitionID(definition.Name))
		ops = append(ops, pp.newInsertDefinitionOp(definition))
	}
	buildTxn := func(attempt int) ([]txn.Op, error) {
		if attempt > 0 {
			// The last attempt aborted so clear out any ops that failed
			// the DocMissing assertion and try again.
			found = []string{}
			mismatched = []string{}
			indexed, err := pp.indexDefinitionDocs(ids)
			if err != nil {
				return nil, errors.Trace(err)
			}

			var okOps []txn.Op
			for _, op := range ops {
				if existing, ok := indexed[op.Id]; !ok {
					okOps = append(okOps, op)
				} else { // Otherwise the op is dropped.
					id := fmt.Sprintf("%s", op.Id)
					found = append(found, id)
					definition, ok := op.Insert.(*ProcessDefinitionDoc)
					if !ok {
						return nil, errors.Errorf("inserting invalid type %T", op.Insert)
					}
					if !reflect.DeepEqual(definition, &existing) {
						mismatched = append(mismatched, id)
					}
				}
			}
			if len(okOps) == 0 {
				return nil, jujutxn.ErrNoOperations
			}
			ops = okOps
		}
		return ops, nil
	}
	if err := pp.st.Run(buildTxn); err != nil {
		return nil, nil, errors.Trace(err)
	}

	return found, mismatched, nil
}

// Insert adds records for the process to persistence. If the process
// is already there then false gets returned (true if inserted).
// Existing records are not checked for consistency.
func (pp Persistence) Insert(info process.Info) (bool, error) {
	var okay bool
	var ops []txn.Op
	// TODO(ericsnow) Add unitPersistence.newEnsureAliveOp(pp.unit)?
	// TODO(ericsnow) Add pp.newEnsureDefinitionOp(info.Process)?
	ops = append(ops, pp.newInsertProcessOps(info)...)
	buildTxn := func(attempt int) ([]txn.Op, error) {
		if attempt > 0 {
			// One of the records already exists.
			okay = false
			return nil, jujutxn.ErrNoOperations
		}
		okay = true
		return ops, nil
	}
	if err := pp.st.Run(buildTxn); err != nil {
		return false, errors.Trace(err)
	}
	return okay, nil
}

// SetStatus updates the raw status for the identified process in
// persistence. The return value corresponds to whether or not the
// record was found in persistence. Any other problem results in
// an error. The process is not checked for inconsistent records.
func (pp Persistence) SetStatus(id string, status process.Status) (bool, error) {
	var found bool
	var ops []txn.Op
	// TODO(ericsnow) Add unitPersistence.newEnsureAliveOp(pp.unit)?
	ops = append(ops, pp.newSetRawStatusOps(id, status)...)
	buildTxn := func(attempt int) ([]txn.Op, error) {
		if attempt > 0 {
			_, err := pp.proc(id)
			if errors.IsNotFound(err) {
				found = false
				return nil, jujutxn.ErrNoOperations
			} else if err != nil {
				return nil, errors.Trace(err)
			}
			// We ignore the request since the proc is dying.
			// TODO(ericsnow) Ensure that procDoc.Status != state.Alive?
			return nil, jujutxn.ErrNoOperations
		}
		found = true
		return ops, nil
	}
	if err := pp.st.Run(buildTxn); err != nil {
		return false, errors.Trace(err)
	}
	return found, nil
}

// List builds the list of processes found in persistence which match
// the provided IDs. The lists of IDs with missing records is also
// returned. Inconsistent records result in errors.NotValid.
func (pp Persistence) List(ids ...string) ([]process.Info, []string, error) {
	var missing []string

	// TODO(ericsnow) Ensure that the unit is Alive?
	// TODO(ericsnow) fix race that exists between the 3 calls
	definitionDocs, err := pp.definitions(ids)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	launchDocs, err := pp.launches(ids)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	procDocs, err := pp.procs(ids)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}

	var results []process.Info
	for _, id := range ids {
		proc, missingCount := pp.extractProc(id, definitionDocs, launchDocs, procDocs)
		if missingCount > 0 {
			if missingCount < 7 {
				return nil, nil, errors.Errorf("found inconsistent records for process %q", id)
			}
			missing = append(missing, id)
			continue
		}
		results = append(results, *proc)
	}
	return results, missing, nil
}

// ListAll builds the list of all processes found in persistence.
// Inconsistent records result in errors.NotValid.
func (pp Persistence) ListAll() ([]process.Info, error) {
	// TODO(ericsnow) Ensure that the unit is Alive?
	// TODO(ericsnow) fix race that exists between the 3 calls
	definitionDocs, err := pp.allDefinitions()
	if err != nil {
		return nil, errors.Trace(err)
	}
	launchDocs, err := pp.allLaunches()
	if err != nil {
		return nil, errors.Trace(err)
	}
	procDocs, err := pp.allProcs()
	if err != nil {
		return nil, errors.Trace(err)
	}

	if len(launchDocs) > len(procDocs) {
		return nil, errors.Errorf("found inconsistent records (extra launch docs)")
	}

	var results []process.Info
	for id := range procDocs {
		proc, missingCount := pp.extractProc(id, definitionDocs, launchDocs, procDocs)
		if missingCount > 0 {
			return nil, errors.Errorf("found inconsistent records for process %q", id)
		}
		results = append(results, *proc)
	}
	for name, doc := range definitionDocs {
		matched := false
		for _, proc := range results {
			if name == proc.Name {
				matched = true
				break
			}
		}
		if !matched {
			results = append(results, process.Info{
				Process: doc.definition(),
			})
		}
	}
	return results, nil
}

// TODO(ericsnow) Add procs to state/cleanup.go.

// TODO(ericsnow) How to ensure they are completely removed from state?

// Remove removes all records associated with the identified process
// from persistence. Also returned is whether or not the process was
// found. If the records for the process are not consistent then
// errors.NotValid is returned.
func (pp Persistence) Remove(id string) (bool, error) {
	var found bool
	var ops []txn.Op
	// TODO(ericsnow) Add unitPersistence.newEnsureAliveOp(pp.unit)?
	ops = append(ops, pp.newRemoveProcessOps(id)...)
	buildTxn := func(attempt int) ([]txn.Op, error) {
		if attempt > 0 {
			okay, err := pp.checkRecords(id)
			if err != nil {
				return nil, errors.Trace(err)
			}
			// If okay is true, it must be dying.
			found = okay
			return nil, jujutxn.ErrNoOperations
		}
		found = true
		return ops, nil
	}
	if err := pp.st.Run(buildTxn); err != nil {
		return false, errors.Trace(err)
	}
	return found, nil
}
