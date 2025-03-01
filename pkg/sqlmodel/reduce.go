// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package sqlmodel

import (
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/pingcap/tiflow/dm/pkg/log"
)

// HasNotNullUniqueIdx returns true when the target table structure has PK or UK
// whose columns are all NOT NULL.
func (r *RowChange) HasNotNullUniqueIdx() bool {
	r.lazyInitWhereHandle()

	return r.whereHandle.UniqueNotNullIdx != nil
}

// IdentityValues returns the two group of values that can be used to identify
// the row. That is to say, if two row changes has same IdentityValues, they are
// changes of the same row. We can use this property to only replicate latest
// changes of one row.
// We always use same index for same table structure to get IdentityValues.
// two groups returned are from preValues and postValues.
func (r *RowChange) IdentityValues() ([]interface{}, []interface{}) {
	r.lazyInitWhereHandle()

	indexInfo := r.whereHandle.UniqueNotNullIdx
	if indexInfo == nil {
		return r.preValues, r.postValues
	}

	pre := make([]interface{}, 0, len(indexInfo.Columns))
	post := make([]interface{}, 0, len(indexInfo.Columns))

	for _, column := range indexInfo.Columns {
		if r.preValues != nil {
			pre = append(pre, r.preValues[column.Offset])
		}
		if r.postValues != nil {
			post = append(post, r.postValues[column.Offset])
		}
	}
	return pre, post
}

// IsIdentityUpdated returns true when the row is updated by the same values.
func (r *RowChange) IsIdentityUpdated() bool {
	if r.tp != RowChangeUpdate {
		return false
	}

	r.lazyInitWhereHandle()
	pre, post := r.IdentityValues()
	if len(pre) != len(post) {
		// should not happen
		return true
	}
	for i := range pre {
		if pre[i] != post[i] {
			return true
		}
	}
	return false
}

// genKey gens key by values e.g. "a.1.b".
func genKey(values []interface{}) string {
	builder := new(strings.Builder)
	for i, v := range values {
		if i != 0 {
			builder.WriteString(".")
		}
		fmt.Fprintf(builder, "%v", v)
	}

	return builder.String()
}

// IdentityKey returns a string generated by IdentityValues.
// If RowChange.IsIdentityUpdated, the behaviour is undefined.
func (r *RowChange) IdentityKey() string {
	pre, post := r.IdentityValues()
	if len(pre) != 0 {
		return genKey(pre)
	}
	return genKey(post)
}

// Reduce will merge two row changes of same row into one row changes,
// e.g., INSERT{1} + UPDATE{1 -> 2} -> INSERT{2}. Receiver will be changed
// in-place.
func (r *RowChange) Reduce(preRowChange *RowChange) {
	if r.IdentityKey() != preRowChange.IdentityKey() {
		log.L().DPanic("reduce row change failed, identity key not match",
			zap.String("preID", preRowChange.IdentityKey()),
			zap.String("curID", r.IdentityKey()))
		return
	}

	// special handle INSERT + DELETE -> DELETE
	if r.tp == RowChangeDelete && preRowChange.tp == RowChangeInsert {
		return
	}

	r.preValues = preRowChange.preValues
	r.calculateType()
}

// SplitUpdate will split current RowChangeUpdate into two RowChangeDelete and
// RowChangeInsert one. The behaviour is undefined for other types of RowChange.
func (r *RowChange) SplitUpdate() (*RowChange, *RowChange) {
	if r.tp != RowChangeUpdate {
		log.L().DPanic("SplitUpdate should only be called on RowChangeUpdate",
			zap.Stringer("rowChange", r))
		return nil, nil
	}

	pre := &RowChange{
		sourceTable:     r.sourceTable,
		targetTable:     r.targetTable,
		preValues:       r.preValues,
		sourceTableInfo: r.sourceTableInfo,
		targetTableInfo: r.targetTableInfo,
		tiSessionCtx:    r.tiSessionCtx,
		tp:              RowChangeDelete,
		whereHandle:     r.whereHandle,
	}
	post := &RowChange{
		sourceTable:     r.sourceTable,
		targetTable:     r.targetTable,
		postValues:      r.postValues,
		sourceTableInfo: r.sourceTableInfo,
		targetTableInfo: r.targetTableInfo,
		tiSessionCtx:    r.tiSessionCtx,
		tp:              RowChangeInsert,
		whereHandle:     r.whereHandle,
	}

	return pre, post
}
