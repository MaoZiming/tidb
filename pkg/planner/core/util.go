// Copyright 2017 PingCAP, Inc.
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

package core

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/planner/core/internal/base"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/table/tables"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/mathutil"
	"github.com/pingcap/tidb/pkg/util/set"
	"github.com/pingcap/tidb/pkg/util/size"
)

// AggregateFuncExtractor visits Expr tree.
// It collects AggregateFuncExpr from AST Node.
type AggregateFuncExtractor struct {
	// skipAggMap stores correlated aggregate functions which have been built in outer query,
	// so extractor in sub-query will skip these aggregate functions.
	skipAggMap map[*ast.AggregateFuncExpr]*expression.CorrelatedColumn
	// AggFuncs is the collected AggregateFuncExprs.
	AggFuncs []*ast.AggregateFuncExpr
}

// Enter implements Visitor interface.
func (*AggregateFuncExtractor) Enter(n ast.Node) (ast.Node, bool) {
	switch n.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
		return n, true
	}
	return n, false
}

// Leave implements Visitor interface.
func (a *AggregateFuncExtractor) Leave(n ast.Node) (ast.Node, bool) {
	//nolint: revive
	switch v := n.(type) {
	case *ast.AggregateFuncExpr:
		if _, ok := a.skipAggMap[v]; !ok {
			a.AggFuncs = append(a.AggFuncs, v)
		}
	}
	return n, true
}

// WindowFuncExtractor visits Expr tree.
// It converts ColunmNameExpr to WindowFuncExpr and collects WindowFuncExpr.
type WindowFuncExtractor struct {
	// WindowFuncs is the collected WindowFuncExprs.
	windowFuncs []*ast.WindowFuncExpr
}

// Enter implements Visitor interface.
func (*WindowFuncExtractor) Enter(n ast.Node) (ast.Node, bool) {
	switch n.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
		return n, true
	}
	return n, false
}

// Leave implements Visitor interface.
func (a *WindowFuncExtractor) Leave(n ast.Node) (ast.Node, bool) {
	//nolint: revive
	switch v := n.(type) {
	case *ast.WindowFuncExpr:
		a.windowFuncs = append(a.windowFuncs, v)
	}
	return n, true
}

// logicalSchemaProducer stores the schema for the logical plans who can produce schema directly.
type logicalSchemaProducer struct {
	schema *expression.Schema
	names  types.NameSlice
	baseLogicalPlan
}

// Schema implements the Plan.Schema interface.
func (s *logicalSchemaProducer) Schema() *expression.Schema {
	if s.schema == nil {
		if len(s.Children()) == 1 {
			// default implementation for plans has only one child: proprgate child schema.
			// multi-children plans are likely to have particular implementation.
			s.schema = s.Children()[0].Schema().Clone()
		} else {
			s.schema = expression.NewSchema()
		}
	}
	return s.schema
}

func (s *logicalSchemaProducer) OutputNames() types.NameSlice {
	if s.names == nil && len(s.Children()) == 1 {
		// default implementation for plans has only one child: proprgate child `OutputNames`.
		// multi-children plans are likely to have particular implementation.
		s.names = s.Children()[0].OutputNames()
	}
	return s.names
}

func (s *logicalSchemaProducer) SetOutputNames(names types.NameSlice) {
	s.names = names
}

// SetSchema implements the Plan.SetSchema interface.
func (s *logicalSchemaProducer) SetSchema(schema *expression.Schema) {
	s.schema = schema
}

func (s *logicalSchemaProducer) setSchemaAndNames(schema *expression.Schema, names types.NameSlice) {
	s.schema = schema
	s.names = names
}

// inlineProjection prunes unneeded columns inline a executor.
func (s *logicalSchemaProducer) inlineProjection(parentUsedCols []*expression.Column, opt *logicalOptimizeOp) {
	prunedColumns := make([]*expression.Column, 0)
	used := expression.GetUsedList(parentUsedCols, s.Schema())
	if len(parentUsedCols) == 0 {
		// When this operator output no columns, we return its smallest column for safety.
		minColLen := math.MaxInt
		chosenPos := 0
		for i, col := range s.schema.Columns {
			flen := col.GetType().GetFlen()
			if flen < minColLen {
				chosenPos = i
				minColLen = flen
			}
		}
		// It should be always true.
		if len(used) > 0 {
			used[chosenPos] = true
		}
	}
	for i := len(used) - 1; i >= 0; i-- {
		if !used[i] {
			prunedColumns = append(prunedColumns, s.Schema().Columns[i])
			s.schema.Columns = append(s.Schema().Columns[:i], s.Schema().Columns[i+1:]...)
		}
	}
	appendColumnPruneTraceStep(s.self, prunedColumns, opt)
}

// physicalSchemaProducer stores the schema for the physical plans who can produce schema directly.
type physicalSchemaProducer struct {
	schema *expression.Schema
	basePhysicalPlan
}

func (s *physicalSchemaProducer) cloneWithSelf(newSelf PhysicalPlan) (*physicalSchemaProducer, error) {
	base, err := s.basePhysicalPlan.cloneWithSelf(newSelf)
	if err != nil {
		return nil, err
	}
	return &physicalSchemaProducer{
		basePhysicalPlan: *base,
		schema:           s.Schema().Clone(),
	}, nil
}

// Schema implements the Plan.Schema interface.
func (s *physicalSchemaProducer) Schema() *expression.Schema {
	if s.schema == nil {
		if len(s.Children()) == 1 {
			// default implementation for plans has only one child: proprgate child schema.
			// multi-children plans are likely to have particular implementation.
			s.schema = s.Children()[0].Schema().Clone()
		} else {
			s.schema = expression.NewSchema()
		}
	}
	return s.schema
}

// SetSchema implements the Plan.SetSchema interface.
func (s *physicalSchemaProducer) SetSchema(schema *expression.Schema) {
	s.schema = schema
}

// MemoryUsage return the memory usage of physicalSchemaProducer
func (s *physicalSchemaProducer) MemoryUsage() (sum int64) {
	if s == nil {
		return
	}

	sum = s.basePhysicalPlan.MemoryUsage() + size.SizeOfPointer
	return
}

// baseSchemaProducer stores the schema for the base plans who can produce schema directly.
type baseSchemaProducer struct {
	schema *expression.Schema
	names  types.NameSlice
	base.Plan
}

// OutputNames returns the outputting names of each column.
func (s *baseSchemaProducer) OutputNames() types.NameSlice {
	return s.names
}

func (s *baseSchemaProducer) SetOutputNames(names types.NameSlice) {
	s.names = names
}

// Schema implements the Plan.Schema interface.
func (s *baseSchemaProducer) Schema() *expression.Schema {
	if s.schema == nil {
		s.schema = expression.NewSchema()
	}
	return s.schema
}

// SetSchema implements the Plan.SetSchema interface.
func (s *baseSchemaProducer) SetSchema(schema *expression.Schema) {
	s.schema = schema
}

func (s *baseSchemaProducer) setSchemaAndNames(schema *expression.Schema, names types.NameSlice) {
	s.schema = schema
	s.names = names
}

// MemoryUsage return the memory usage of baseSchemaProducer
func (s *baseSchemaProducer) MemoryUsage() (sum int64) {
	if s == nil {
		return
	}

	sum = size.SizeOfPointer + size.SizeOfSlice + int64(cap(s.names))*size.SizeOfPointer + s.Plan.MemoryUsage()
	if s.schema != nil {
		sum += s.schema.MemoryUsage()
	}
	for _, name := range s.names {
		sum += name.MemoryUsage()
	}
	return
}

// Schema implements the Plan.Schema interface.
func (p *LogicalMaxOneRow) Schema() *expression.Schema {
	s := p.Children()[0].Schema().Clone()
	resetNotNullFlag(s, 0, s.Len())
	return s
}

func buildLogicalJoinSchema(joinType JoinType, join LogicalPlan) *expression.Schema {
	leftSchema := join.Children()[0].Schema()
	switch joinType {
	case SemiJoin, AntiSemiJoin:
		return leftSchema.Clone()
	case LeftOuterSemiJoin, AntiLeftOuterSemiJoin:
		newSchema := leftSchema.Clone()
		newSchema.Append(join.Schema().Columns[join.Schema().Len()-1])
		return newSchema
	}
	newSchema := expression.MergeSchema(leftSchema, join.Children()[1].Schema())
	if joinType == LeftOuterJoin {
		resetNotNullFlag(newSchema, leftSchema.Len(), newSchema.Len())
	} else if joinType == RightOuterJoin {
		resetNotNullFlag(newSchema, 0, leftSchema.Len())
	}
	return newSchema
}

// BuildPhysicalJoinSchema builds the schema of PhysicalJoin from it's children's schema.
func BuildPhysicalJoinSchema(joinType JoinType, join PhysicalPlan) *expression.Schema {
	leftSchema := join.Children()[0].Schema()
	switch joinType {
	case SemiJoin, AntiSemiJoin:
		return leftSchema.Clone()
	case LeftOuterSemiJoin, AntiLeftOuterSemiJoin:
		newSchema := leftSchema.Clone()
		newSchema.Append(join.Schema().Columns[join.Schema().Len()-1])
		return newSchema
	}
	newSchema := expression.MergeSchema(leftSchema, join.Children()[1].Schema())
	if joinType == LeftOuterJoin {
		resetNotNullFlag(newSchema, leftSchema.Len(), newSchema.Len())
	} else if joinType == RightOuterJoin {
		resetNotNullFlag(newSchema, 0, leftSchema.Len())
	}
	return newSchema
}

// GetStatsInfoFromFlatPlan gets the statistics info from a FlatPhysicalPlan.
func GetStatsInfoFromFlatPlan(flat *FlatPhysicalPlan) map[string]uint64 {
	res := make(map[string]uint64)
	for _, op := range flat.Main {
		switch p := op.Origin.(type) {
		case *PhysicalIndexScan:
			if _, ok := res[p.Table.Name.O]; p.StatsInfo() != nil && !ok {
				res[p.Table.Name.O] = p.StatsInfo().StatsVersion
			}
		case *PhysicalTableScan:
			if _, ok := res[p.Table.Name.O]; p.StatsInfo() != nil && !ok {
				res[p.Table.Name.O] = p.StatsInfo().StatsVersion
			}
		}
	}
	return res
}

// GetStatsInfo gets the statistics info from a physical plan tree.
// Deprecated: FlattenPhysicalPlan() + GetStatsInfoFromFlatPlan() is preferred.
func GetStatsInfo(i interface{}) map[string]uint64 {
	if i == nil {
		// it's a workaround for https://github.com/pingcap/tidb/issues/17419
		// To entirely fix this, uncomment the assertion in TestPreparedIssue17419
		return nil
	}
	p := i.(Plan)
	var physicalPlan PhysicalPlan
	switch x := p.(type) {
	case *Insert:
		physicalPlan = x.SelectPlan
	case *Update:
		physicalPlan = x.SelectPlan
	case *Delete:
		physicalPlan = x.SelectPlan
	case PhysicalPlan:
		physicalPlan = x
	}

	if physicalPlan == nil {
		return nil
	}

	statsInfos := make(map[string]uint64)
	statsInfos = CollectPlanStatsVersion(physicalPlan, statsInfos)
	return statsInfos
}

// extractStringFromStringSet helps extract string info from set.StringSet.
func extractStringFromStringSet(set set.StringSet) string {
	if len(set) < 1 {
		return ""
	}
	l := make([]string, 0, len(set))
	for k := range set {
		l = append(l, fmt.Sprintf(`"%s"`, k))
	}
	slices.Sort(l)
	return strings.Join(l, ",")
}

// extractStringFromStringSlice helps extract string info from []string.
func extractStringFromStringSlice(ss []string) string {
	if len(ss) < 1 {
		return ""
	}
	slices.Sort(ss)
	return strings.Join(ss, ",")
}

// extractStringFromUint64Slice helps extract string info from uint64 slice.
func extractStringFromUint64Slice(slice []uint64) string {
	if len(slice) < 1 {
		return ""
	}
	l := make([]string, 0, len(slice))
	for _, k := range slice {
		l = append(l, fmt.Sprintf(`%d`, k))
	}
	slices.Sort(l)
	return strings.Join(l, ",")
}

// extractStringFromBoolSlice helps extract string info from bool slice.
func extractStringFromBoolSlice(slice []bool) string {
	if len(slice) < 1 {
		return ""
	}
	l := make([]string, 0, len(slice))
	for _, k := range slice {
		l = append(l, fmt.Sprintf(`%t`, k))
	}
	slices.Sort(l)
	return strings.Join(l, ",")
}

func tableHasDirtyContent(ctx sessionctx.Context, tableInfo *model.TableInfo) bool {
	pi := tableInfo.GetPartitionInfo()
	if pi == nil {
		return ctx.HasDirtyContent(tableInfo.ID)
	}
	// Currently, we add UnionScan on every partition even though only one partition's data is changed.
	// This is limited by current implementation of Partition Prune. It'll be updated once we modify that part.
	for _, partition := range pi.Definitions {
		if ctx.HasDirtyContent(partition.ID) {
			return true
		}
	}
	return false
}

func clonePhysicalPlan(plans []PhysicalPlan) ([]PhysicalPlan, error) {
	cloned := make([]PhysicalPlan, 0, len(plans))
	for _, p := range plans {
		c, err := p.Clone()
		if err != nil {
			return nil, err
		}
		cloned = append(cloned, c)
	}
	return cloned, nil
}

// GetPhysID returns the physical table ID.
func GetPhysID(tblInfo *model.TableInfo, partitionExpr *tables.PartitionExpr, d types.Datum) (int64, error) {
	pi := tblInfo.GetPartitionInfo()
	if pi == nil {
		return tblInfo.ID, nil
	}

	if partitionExpr == nil {
		return tblInfo.ID, nil
	}

	switch pi.Type {
	case model.PartitionTypeHash:
		intVal := d.GetInt64()
		partIdx := mathutil.Abs(intVal % int64(pi.Num))
		return pi.Definitions[partIdx].ID, nil
	case model.PartitionTypeKey:
		if partitionExpr.ForKeyPruning == nil ||
			len(pi.Columns) > 1 {
			return 0, errors.Errorf("unsupported partition type in BatchGet")
		}
		// We need to change the partition column index!
		col := &expression.Column{}
		*col = *partitionExpr.KeyPartCols[0]
		col.Index = 0
		newKeyPartExpr := tables.ForKeyPruning{KeyPartCols: []*expression.Column{col}}
		partIdx, err := newKeyPartExpr.LocateKeyPartition(pi.Num, []types.Datum{d})
		if err != nil {
			return 0, errors.Errorf("unsupported partition type in BatchGet")
		}
		return pi.Definitions[partIdx].ID, nil
	case model.PartitionTypeRange:
		// we've check the type assertions in func TryFastPlan
		col, ok := partitionExpr.Expr.(*expression.Column)
		if !ok {
			return 0, errors.Errorf("unsupported partition type in BatchGet")
		}
		unsigned := mysql.HasUnsignedFlag(col.GetType().GetFlag())
		ranges := partitionExpr.ForRangePruning
		length := len(ranges.LessThan)
		intVal := d.GetInt64()
		partIdx := sort.Search(length, func(i int) bool {
			return ranges.Compare(i, intVal, unsigned) > 0
		})
		if partIdx >= 0 && partIdx < length {
			return pi.Definitions[partIdx].ID, nil
		}
	case model.PartitionTypeList:
		isNull := false // we've guaranteed this in the build process of either TryFastPlan or buildBatchPointGet
		intVal := d.GetInt64()
		partIdx := partitionExpr.ForListPruning.LocatePartition(intVal, isNull)
		if partIdx >= 0 {
			return pi.Definitions[partIdx].ID, nil
		}
	}

	return 0, errors.Errorf("dual partition")
}
