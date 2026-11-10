package recordlayer

import (
	"embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"fdb.dev/gen/demopb"
)

// Keep source inputs explicit: this is a reviewable inventory of the store,
// supported maintainer, and backfill boundaries, not a runtime filesystem probe.
//
//go:embed store.go store_api.go store_batch.go store_typed.go store_builder.go store_delete_where.go store_partition.go store_version.go partition_protocol.go partition_format.go online_indexer.go indexing_mutual.go indexing_pending_write_queue.go index_maintainer.go standard_index_maintainer_api.go text_index_maintainer.go index_state.go record_count.go
//go:embed partition_writers_test.go partition_backfill_test.go partition_protocol_test.go partition_preflight_test.go partition_format_test.go partition_annotation_test.go partition_group_test.go index_maintainer_queue_test.go index_state_test.go record_count_test.go store_api_test.go store_state_cache_test.go
var partitionCensusSources embed.FS

type partitionWriterClassification struct {
	class string
	via   string
	proof string
}

// Administrative writes deliberately do not acquire per-record fences: index
// state/build bookkeeping and StoreInfo are not record routes. Safe DROP must
// separately exclude active builds, WRITE_ONLY state, and pending queues.
var partitionWriterInventory = map[string]partitionWriterClassification{
	"FDBRecordStore.SaveRecord":                                {"delegated", "SaveRecordWithOptions", "save"},
	"FDBRecordStore.SaveRecordWithOptions":                     {"delegated", "saveRecordInternal", "save"},
	"FDBRecordStore.SaveRecordWithVersion":                     {"delegated", "saveRecordInternal", "save"},
	"FDBRecordStore.InsertRecord":                              {"delegated", "SaveRecordWithOptions", "save"},
	"FDBRecordStore.UpdateRecord":                              {"delegated", "SaveRecordWithOptions", "save"},
	"FDBRecordStore.OverrideLockSaveRecord":                    {"delegated", "saveRecordInternal", "save"},
	"FDBRecordStore.OverrideLockSaveRecordWithVersion":         {"delegated", "saveRecordInternal", "save"},
	"FDBRecordStore.SaveRecordsPipelined":                      {"delegated", "savePartitionedGroup", "group"},
	"FDBRecordStore.savePartitionedGroup":                      {"guarded", "beginPartitionMutation", "group"},
	"FDBRecordStore.saveRecordInternal":                        {"guarded", "beginRecordPartitionMutation", "save"},
	"FDBRecordStore.SaveRecordBatch":                           {"delegated", "SaveRecordBatchWithOptions", "save"},
	"FDBRecordStore.SaveRecordBatchWithOptions":                {"guarded", "beginPartitionMutation", "batch"},
	"FDBRecordStore.DeleteRecord":                              {"delegated", "deleteRecord", "delete"},
	"FDBRecordStore.deleteRecord":                              {"guarded", "beginRecordPartitionMutation", "delete"},
	"FDBRecordStore.DeleteRecordsWhere":                        {"guarded", "beginPartitionMutation", "prefix"},
	"FDBRecordStore.ClearPartitionData":                        {"guarded", "beginPartitionMutation", "prefix"},
	"FDBRecordStore.DeleteAllRecords":                          {"guarded", "prepareReset", "reset"},
	"DeleteStore":                                              {"reset", "clearRange", "reset"},
	"FDBRecordStore.DryRunSaveRecord":                          {"preview", "previewPartitionRoute", "preview"},
	"FDBRecordStore.DryRunDeleteRecord":                        {"preview", "previewPartitionRoute", "preview"},
	"FDBRecordStore.GetIndexMaintainer":                        {"refused", "indexIsPartitioned", "raw"},
	"FDBRecordStore.DeleteIndexEntries":                        {"refused", "indexIsPartitioned", "raw"},
	"FDBRecordStore.DeleteIndexEntriesInRange":                 {"refused", "indexIsPartitioned", "raw"},
	"NewStandardIndexMaintainer":                               {"refused", "partitionFunctions", "raw"},
	"StandardIndexMaintainer.UpdateWhileWriteOnly":             {"delegated", "Update", "raw"},
	"StandardIndexMaintainer.Update":                           {"internal", "checkKeyValueSizes", "raw"},
	"StandardIndexMaintainer.DeleteWhere":                      {"refused", "deleteWhereRange", "raw"},
	"StandardIndexMaintainer.insertInt64Entry":                 {"internal", "Set", "save"},
	"StandardIndexMaintainer.insertScalarEntry":                {"internal", "Set", "save"},
	"StandardIndexMaintainer.insertSingleEntry":                {"internal", "Set", "save"},
	"deleteWhereRange":                                         {"internal", "ClearRange", "prefix"},
	"textIndexMaintainer.Update":                               {"internal", "update", "backfill"},
	"textIndexMaintainer.update":                               {"internal", "updateStandard", "backfill"},
	"textIndexMaintainer.UpdateWhileWriteOnly":                 {"delegated", "Update", "backfill"},
	"textIndexMaintainer.writeRecordTokenizerVersion":          {"internal", "SetBytes", "backfill"},
	"textIndexMaintainer.clearRecordTokenizerVersion":          {"internal", "ClearBytes", "backfill"},
	"textIndexMaintainer.DeleteWhere":                          {"internal", "ClearRange", "prefix"},
	"FDBRecordStore.updateOneIndex":                            {"internal", "UpdateWhileWriteOnly", "save"},
	"FDBRecordStore.saveRecordVersionAtKey":                    {"internal", "AddVersionMutation", "save"},
	"FDBRecordStore.saveVersionWithOldFormat":                  {"internal", "AddVersionMutation", "save"},
	"FDBRecordStore.removeVersionDataInRange":                  {"internal", "RemoveVersionMutationsInRange", "reset"},
	"partitionMutation.apply":                                  {"internal", "Set", "protocol"},
	"FDBRecordStore.addUniquenessViolation":                    {"internal", "Set", "annotation-lock"},
	"FDBRecordStore.removeUniquenessViolations":                {"internal", "Clear", "annotation"},
	"FDBRecordStore.AddUniquenessViolation":                    {"delegated", "AddUniquenessViolationWithExisting", "annotation"},
	"FDBRecordStore.AddUniquenessViolationWithExisting":        {"guarded", "partitionIndexMutation", "annotation"},
	"FDBRecordStore.ResolveUniquenessViolation":                {"guarded", "partitionIndexMutation", "annotation"},
	"FDBRecordStore.ResolveUniquenessViolationByDeletion":      {"guarded", "beginPartitionMutation", "resolution"},
	"FDBRecordStore.RebuildIndex":                              {"guarded", "prepareBackfillTable", "backfill"},
	"FDBRecordStore.RebuildAllIndexes":                         {"delegated", "RebuildIndex", "backfill"},
	"FDBRecordStore.updatePartitionedBackfill":                 {"guarded", "prepareRoute", "backfill"},
	"FDBRecordStore.checkPartitionBackfillRecordType":          {"preview", "beginRecordPartitionMutation", "backfill-type"},
	"OnlineIndexer.indexScannedRecord":                         {"delegated", "updatePartitionedBackfill", "backfill"},
	"OnlineIndexer.buildRangeInTransaction":                    {"delegated", "indexScannedRecord", "backfill"},
	"OnlineIndexer.buildRangeByIndex":                          {"delegated", "updatePartitionedBackfill", "backfill"},
	"mutualIndexBuilder.buildFragmentRange":                    {"delegated", "indexScannedRecord", "backfill"},
	"FDBRecordStore.replayPendingIndexWrite":                   {"refused", "indexIsPartitioned", "queue"},
	"FDBRecordStore.enqueuePendingIndexWrite":                  {"refused", "indexIsPartitioned", "queue"},
	"FDBRecordStore.validatePendingQueueIndex":                 {"refused", "indexIsPartitioned", "queue"},
	"FDBRecordStore.SetFormatVersion":                          {"refused", "hasPartitionedTypes", "format"},
	"FDBRecordStore.checkPossiblyRebuild":                      {"guarded", "beginPartitionMutation", "format"},
	"FDBRecordStore.convertRecordVersionsToInline":             {"delegated", "convertPartitionRecordVersionsToInline", "format"},
	"FDBRecordStore.convertPartitionRecordVersionsToInline":    {"guarded", "prepareRoute", "format"},
	"FDBRecordStore.MarkIndexDisabled":                         {"administrative", "clearIndexData", "state"},
	"FDBRecordStore.MarkIndexReadable":                         {"administrative", "setIndexStateLocked", "state"},
	"FDBRecordStore.MarkIndexReadableOrUniquePending":          {"administrative", "setIndexStateLocked", "state"},
	"FDBRecordStore.MarkIndexWriteOnly":                        {"administrative", "markIndexWriteOnly", "state"},
	"FDBRecordStore.MarkIndexWriteOnlyWithQueue":               {"refused", "markIndexWriteOnly", "queue"},
	"FDBRecordStore.ClearAndMarkIndexWriteOnly":                {"administrative", "clearAndMarkIndexWriteOnly", "state"},
	"FDBRecordStore.ClearAndMarkIndexWriteOnlyWithQueue":       {"refused", "clearAndMarkIndexWriteOnly", "queue"},
	"FDBRecordStore.setIndexStateLocked":                       {"administrative", "Set", "state"},
	"FDBRecordStore.clearIndexData":                            {"administrative", "ClearRange", "state"},
	"FDBRecordStore.removeFormerIndexData":                     {"administrative", "ClearRange", "state"},
	"FDBRecordStore.clearReadableIndexBuildData":               {"administrative", "Clear", "state"},
	"FDBRecordStore.eraseAllIndexingDataButTheLockAndRangeSet": {"administrative", "ClearRange", "state"},
	"FDBRecordStore.SaveIndexingTypeStamp":                     {"administrative", "Set", "state"},
	"FDBRecordStore.AddBuildProgress":                          {"administrative", "Add", "state"},
	"FDBRecordStore.VacuumReadableIndexesBuildData":            {"administrative", "ClearRange", "vacuum"},
	"FDBRecordStore.writeStoreHeader":                          {"administrative", "Set", "header"},
	"FDBRecordStore.SetUserVersion":                            {"administrative", "writeStoreHeader", "header"},
	"FDBRecordStore.UpdateIncarnation":                         {"administrative", "writeStoreHeader", "header"},
	"FDBRecordStore.SetHeaderUserField":                        {"administrative", "writeStoreHeader", "header"},
	"FDBRecordStore.ClearHeaderUserField":                      {"administrative", "writeStoreHeader", "header"},
	"FDBRecordStore.SetStoreLockState":                         {"administrative", "writeStoreHeader", "header"},
	"FDBRecordStore.ClearStoreLockState":                       {"administrative", "writeStoreHeader", "header"},
	"FDBRecordStore.SetStateCacheability":                      {"administrative", "writeStoreHeader", "header"},
	"FDBRecordStore.UpdateRecordCountState":                    {"administrative", "writeStoreHeader", "count"},
	"FDBRecordStore.addRecordCount":                            {"internal", "Add", "count"},
	"FDBRecordStore.checkPossiblyRebuildRecordCounts":          {"administrative", "ClearRange", "count"},
	"FDBRecordStore.rebuildRecordCounts":                       {"administrative", "Set", "count"},
	"FDBRecordStore.AddRecordReadConflict":                     {"conflict-only", "AddReadConflictRange", "save"},
	"FDBRecordStore.AddRecordWriteConflict":                    {"conflict-only", "AddWriteConflictRange", "save"},
	"FDBRecordContext.clearRangeWithIndexStateViews":           {"native-escape", "ClearRange", "reset"},
	"FDBRecordStore.Context":                                   {"native-escape", "", "raw"},
	"FDBRecordStore.GetContext":                                {"native-escape", "", "raw"},
	"TypedFDBRecordStore.SaveRecord":                           {"delegated", "SaveRecordWithOptions", "save"},
	"TypedFDBRecordStore.SaveRecordWithOptions":                {"delegated", "SaveRecordWithVersion", "save"},
	"TypedFDBRecordStore.SaveRecordWithVersion":                {"delegated", "saveRecordInternal", "save"},
	"TypedFDBRecordStore.InsertRecord":                         {"delegated", "SaveRecordWithOptions", "save"},
	"TypedFDBRecordStore.UpdateRecord":                         {"delegated", "SaveRecordWithOptions", "save"},
	"TypedFDBRecordStore.DeleteRecord":                         {"delegated", "deleteRecord", "delete"},
	"TypedFDBRecordStore.DeleteAllRecords":                     {"delegated", "DeleteAllRecords", "reset"},
	"TypedFDBRecordStore.AddRecordReadConflict":                {"conflict-only", "AddRecordReadConflict", "save"},
	"TypedFDBRecordStore.AddRecordWriteConflict":               {"conflict-only", "AddRecordWriteConflict", "save"},
}

var partitionCensusProofs = map[string]struct{ file, anchor string }{
	"save":            {"partition_writers_test.go", "routes every save surface and cleans pending/committed versions"},
	"group":           {"partition_group_test.go", "rejects a reset between completed group rows through"},
	"batch":           {"partition_annotation_test.go", "keeps plain batch writes free of partition sessions and guards"},
	"delete":          {"partition_writers_test.go", "fences loaded-record deletion and refuses missing routing state without clearing a body"},
	"prefix":          {"partition_writers_test.go", "retains active state through caller-excluded cleanup and conflicts a prefix clear with an admitted writer"},
	"reset":           {"partition_writers_test.go", "resets both tables with fresh incarnations and removes pending/legacy version state before reinsertion"},
	"preview":         {"partition_preflight_test.go", "saves and deletes without persisted or pending-version effects"},
	"raw":             {"partition_writers_test.go", "refuses unfenced public raw index mutation and leaves plain/nil controls unchanged"},
	"backfill":        {"partition_backfill_test.go", "validates routes without changing directory bytes during"},
	"protocol":        {"partition_protocol_test.go", "does not read epoch or rewrite shared state for an active route"},
	"backfill-type":   {"partition_backfill_test.go", "real body would otherwise be skipped by the target filter"},
	"annotation-lock": {"partition_annotation_test.go", "does not reacquire partition preparation under WRITE_ONLY maintenance with a queued state writer"},
	"annotation":      {"partition_annotation_test.go", "fences public annotation APIs and preserves exact violation bytes with committable refusals"},
	"resolution":      {"partition_writers_test.go", "routes uniqueness-resolution deletes while retaining the selected survivor and active generation"},
	"queue":           {"partition_backfill_test.go", "refuses queued admission and valid unexpected replay without effects for partitioned"},
	"format":          {"partition_format_test.go", "keeps reconciliation rollback-only when header publication is not reached after conversion"},
	"state":           {"index_state_test.go", `Describe("IndexState"`},
	"vacuum":          {"store_api_test.go", "clears build artifacts for READABLE indexes"},
	"header":          {"store_state_cache_test.go", "cache miss after header mutation (SetHeaderUserField)"},
	"count":           {"record_count_test.go", "StateTransitionValidation"},
}

func partitionCensusReceiver(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.StarExpr:
		return partitionCensusReceiver(node.X)
	case *ast.IndexExpr:
		return partitionCensusReceiver(node.X)
	case *ast.IndexListExpr:
		return partitionCensusReceiver(node.X)
	case *ast.Ident:
		return node.Name
	default:
		return ""
	}
}

func partitionCensusFunctions(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	files, err := partitionCensusSources.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	functions := make(map[string]*ast.FuncDecl)
	productionFiles := 0
	for _, file := range files {
		if strings.HasSuffix(file.Name(), "_test.go") {
			continue
		}
		productionFiles++
		source, err := partitionCensusSources.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file.Name(), source, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil {
				name = partitionCensusReceiver(fn.Recv.List[0].Type) + "." + name
			}
			if functions[name] != nil {
				t.Fatalf("duplicate census function %s", name)
			}
			functions[name] = fn
		}
	}
	if productionFiles < 18 || len(functions) < 200 {
		t.Fatalf("vacuous census: %d files, %d functions", productionFiles, len(functions))
	}
	return functions
}

func partitionCensusCalls(fn *ast.FuncDecl) map[string]bool {
	calls := make(map[string]bool)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.SelectorExpr:
			calls[callee.Sel.Name] = true
		case *ast.Ident:
			calls[callee.Name] = true
		}
		return true
	})
	return calls
}

func partitionCensusNativeWriter(fn *ast.FuncDecl) bool {
	writer := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "Set", "SetBytes", "Clear", "ClearBytes", "ClearRange", "clearKey", "AddBytes", "AddVersionMutation", "RemoveVersionMutation", "RemoveVersionMutationsInRange", "RemoveLocalVersionsInRange", "SetVersionstampedKey", "SetVersionstampedValue":
			writer = true
		case "Add", "And", "BitAnd", "Or", "BitOr", "Xor", "BitXor", "Max", "Min", "ByteMax", "ByteMin", "AppendIfFits", "CompareAndClear", "MaxBytes", "MinBytes", "AppendIfFitsBytes", "CompareAndClearBytes":
			// Atomic mutations take key and operand; time.Time.Add and sync/atomic.Add do not.
			writer = writer || len(call.Args) == 2
		}
		return true
	})
	return writer
}

func partitionCensusMutatingAPI(name string) bool {
	for _, prefix := range []string{"Save", "Delete", "Insert", "Update", "OverrideLockSave", "Set", "Add", "Clear", "Mark", "Rebuild", "Resolve", "Vacuum", "DryRun"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return name == "GetIndexMaintainer"
}

func partitionCensusCoverage(population map[string]bool, inventory map[string]partitionWriterClassification) error {
	if len(population) == 0 || len(inventory) == 0 {
		return fmt.Errorf("empty writer census population or inventory")
	}
	var missing []string
	for name := range population {
		if _, ok := inventory[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		slices.Sort(missing)
		return fmt.Errorf("unclassified writers: %s", strings.Join(missing, ", "))
	}
	return nil
}

func TestPartitionWriterCensus(t *testing.T) {
	functions := partitionCensusFunctions(t)
	population := make(map[string]bool)
	nativeCount, publicCount := 0, 0
	for name, fn := range functions {
		if partitionCensusNativeWriter(fn) {
			nativeCount++
			population[name] = true
		}
	}
	// Reflection makes a newly exposed store mutator fail even if its new source
	// file has not yet been added to the explicit embedded source inputs.
	for _, store := range []struct {
		name string
		typ  reflect.Type
	}{
		{"FDBRecordStore", reflect.TypeFor[*FDBRecordStore]()},
		{"TypedFDBRecordStore", reflect.TypeFor[*TypedFDBRecordStore[*demopb.Order]]()},
	} {
		for i := 0; i < store.typ.NumMethod(); i++ {
			name := store.typ.Method(i).Name
			if partitionCensusMutatingAPI(name) {
				publicCount++
				population[store.name+"."+name] = true
			}
		}
	}
	if nativeCount < 30 || publicCount < 50 {
		t.Fatalf("vacuous writers: %d native, %d public", nativeCount, publicCount)
	}
	if err := partitionCensusCoverage(population, partitionWriterInventory); err != nil {
		t.Fatal(err)
	}
	classes := map[string]bool{"guarded": true, "delegated": true, "refused": true, "internal": true, "administrative": true, "reset": true, "preview": true, "conflict-only": true, "native-escape": true}
	for _, name := range slices.Sorted(maps.Keys(partitionWriterInventory)) {
		entry := partitionWriterInventory[name]
		fn := functions[name]
		if fn == nil {
			t.Fatalf("stale inventory entry %s", name)
		}
		if !classes[entry.class] {
			t.Fatalf("unclassified mechanism for %s: %q", name, entry.class)
		}
		if entry.via != "" && !partitionCensusCalls(fn)[entry.via] {
			t.Fatalf("%s lost its %s mechanism %s", name, entry.class, entry.via)
		}
		proof, ok := partitionCensusProofs[entry.proof]
		if !ok {
			t.Fatalf("%s has no runtime proof", name)
		}
		source, err := partitionCensusSources.ReadFile(proof.file)
		if err != nil {
			t.Fatal(err)
		}
		if proof.anchor == "" || !strings.Contains(string(source), proof.anchor) {
			t.Fatalf("%s lost runtime proof %s:%q", name, proof.file, proof.anchor)
		}
	}
	plainQueue, err := partitionCensusSources.ReadFile("index_maintainer_queue_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plainQueue), "applies ordered updates and computed DELETE_WHERE prefixes before clearing committed entries") {
		t.Fatal("plain queue positive control disappeared")
	}
	t.Logf("partition writer census: %d functions, %d native mutation callers, %d public mutator methods, %d classified boundaries", len(functions), nativeCount, publicCount, len(partitionWriterInventory))
}

func TestPartitionWriterCensusCoverageControls(t *testing.T) {
	inventory := map[string]partitionWriterClassification{"known": {class: "guarded"}}
	for _, tc := range []struct {
		name       string
		population map[string]bool
		inventory  map[string]partitionWriterClassification
		want       string
	}{
		{"empty population", nil, inventory, "empty"},
		{"empty inventory", map[string]bool{"known": true}, nil, "empty"},
		{"missing known writer", map[string]bool{"known": true}, map[string]partitionWriterClassification{"other": {}}, "known"},
		{"new blind writer", map[string]bool{"known": true, "blind": true}, inventory, "blind"},
		{"covered positive", map[string]bool{"known": true}, inventory, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := partitionCensusCoverage(tc.population, tc.inventory)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), "injected.go", `package recordlayer
func (store *FDBRecordStore) blindWriter() { store.context.Transaction().Set(nil, nil) }
func (store *FDBRecordStore) nonWriter() { store.context.Transaction().Get(nil) }
func globalWriter() { tx.ClearRange(nil) }
func (store *Generic[A, B]) genericWriter() { tx.Add(nil, nil) }
func timeOnly() { now.Add(1) }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false, true, true, false} {
		fn := parsed.Decls[i].(*ast.FuncDecl)
		if got := partitionCensusNativeWriter(fn); got != want {
			t.Fatalf("injected %s writer=%v want %v", fn.Name, got, want)
		}
		if fn.Recv != nil && partitionCensusReceiver(fn.Recv.List[0].Type) == "" {
			t.Fatal("receiver was not discovered")
		}
	}
	if partitionCensusReceiver(&ast.BasicLit{}) != "" {
		t.Fatal("unknown receiver must not invent a name")
	}
	for _, name := range []string{"SaveNewAPI", "DeleteNewAPI", "GetIndexMaintainer", "DryRunSaveRecord"} {
		if !partitionCensusMutatingAPI(name) {
			t.Fatalf("new API %s was not discovered", name)
		}
	}
	if partitionCensusMutatingAPI("LoadRecord") {
		t.Fatal("read API misclassified as writer")
	}
}

func TestPartitionBackfillChecksTypeBeforeFiltering(t *testing.T) {
	functions := partitionCensusFunctions(t)
	for _, name := range []string{"FDBRecordStore.RebuildIndex", "OnlineIndexer.indexScannedRecord", "OnlineIndexer.buildRangeByIndex"} {
		var check, filter token.Pos
		ast.Inspect(functions[name].Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if selector.Sel.Name == "checkPartitionBackfillRecordType" && check == token.NoPos {
				check = call.Pos()
			}
			if selector.Sel.Name == "shouldIndexRecordForIndex" && filter == token.NoPos {
				filter = call.Pos()
			}
			return true
		})
		if check == token.NoPos || filter == token.NoPos || check >= filter {
			t.Fatalf("%s can filter a poisoned physical prefix before checking its type: check=%d filter=%d", name, check, filter)
		}
	}
}

func TestPartitionBatchDeferGateControls(t *testing.T) {
	for _, tc := range []struct {
		source  string
		bad     bool
		hasLoop bool
	}{
		{`func sample() { defer outside(); for { defer bad() } }`, true, true},
		{`func sample() { for { func() { defer local() }() } }`, false, true},
		{`func sample() { for { for { defer bad() } } }`, true, true},
		{`func sample() {}`, false, false},
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), "injected.go", "package recordlayer\n"+tc.source, 0)
		if err != nil {
			t.Fatal(err)
		}
		loops, bad := partitionCensusLoopDefers(parsed.Decls[0].(*ast.FuncDecl))
		if bad != tc.bad || (loops != 0) != tc.hasLoop {
			t.Fatalf("%s: loops=%d bad=%v", tc.source, loops, bad)
		}
	}
}

func TestPartitionInternalAnnotationDoesNotReenterPreparation(t *testing.T) {
	fn := partitionCensusFunctions(t)["FDBRecordStore.addUniquenessViolation"]
	calls := partitionCensusCalls(fn)
	if !calls["Set"] {
		t.Fatal("raw annotation population is empty")
	}
	for _, forbidden := range []string{"beginPartitionMutation", "beginRecordPartitionMutation", "partitionIndexMutation", "AddUniquenessViolation", "AddUniquenessViolationWithExisting"} {
		if calls[forbidden] {
			t.Fatalf("internal annotation reacquires preparation through %s under maintenance locks", forbidden)
		}
	}
	public := partitionCensusFunctions(t)["FDBRecordStore.AddUniquenessViolationWithExisting"]
	publicCalls := partitionCensusCalls(public)
	if !publicCalls["partitionIndexMutation"] || !publicCalls["addUniquenessViolation"] {
		t.Fatal("public annotation must guard and share the private raw writer")
	}
}

func partitionCensusLoopDefers(fn *ast.FuncDecl) (loops int, deferredInsideLoop bool) {
	var walk func(ast.Node, bool)
	walk = func(node ast.Node, inLoop bool) {
		ast.Inspect(node, func(child ast.Node) bool {
			switch child.(type) {
			case *ast.FuncLit:
				// An immediately invoked physical-write closure unwinds per row;
				// its local defer does not accumulate until the outer batch returns.
				return false
			case *ast.ForStmt, *ast.RangeStmt:
				loops++
				if child != node {
					walk(child, true)
					return false
				}
			case *ast.DeferStmt:
				deferredInsideLoop = deferredInsideLoop || inLoop
			}
			return true
		})
	}
	walk(fn.Body, false)
	return loops, deferredInsideLoop
}

func TestPartitionBatchHasNoLoopScopedDefers(t *testing.T) {
	fn := partitionCensusFunctions(t)["FDBRecordStore.SaveRecordBatchWithOptions"]
	loops, deferredInsideLoop := partitionCensusLoopDefers(fn)
	if loops == 0 || deferredInsideLoop {
		t.Fatalf("batch loops=%d, loop-scoped deferred calls=%v", loops, deferredInsideLoop)
	}
}
