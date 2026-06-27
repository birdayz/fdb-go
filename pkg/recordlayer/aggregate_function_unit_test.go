package recordlayer

import (
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Aggregate Function Unit Tests", func() {
	Describe("constructor functions", func() {
		It("NewCountAggregateFunction", func() {
			operand := Field("price")
			fn := NewCountAggregateFunction(operand)
			Expect(fn.Name).To(Equal(FunctionNameCount))
			Expect(fn.Operand).To(Equal(operand))
			Expect(fn.Index).To(BeEmpty())
		})

		It("NewSumAggregateFunction", func() {
			operand := Ungrouped(Field("price"))
			fn := NewSumAggregateFunction(operand)
			Expect(fn.Name).To(Equal(FunctionNameSum))
			Expect(fn.Operand).To(Equal(operand))
			Expect(fn.Index).To(BeEmpty())
		})

		It("NewMinAggregateFunction", func() {
			operand := Field("price")
			fn := NewMinAggregateFunction(operand)
			Expect(fn.Name).To(Equal(FunctionNameMin))
			Expect(fn.Operand).To(Equal(operand))
			Expect(fn.Index).To(BeEmpty())
		})

		It("NewMaxAggregateFunction", func() {
			operand := Field("price")
			fn := NewMaxAggregateFunction(operand)
			Expect(fn.Name).To(Equal(FunctionNameMax))
			Expect(fn.Operand).To(Equal(operand))
			Expect(fn.Index).To(BeEmpty())
		})

		It("NewMinEverAggregateFunction", func() {
			operand := Ungrouped(Field("price"))
			fn := NewMinEverAggregateFunction(operand)
			Expect(fn.Name).To(Equal(FunctionNameMinEver))
			Expect(fn.Operand).To(Equal(operand))
			Expect(fn.Index).To(BeEmpty())
		})

		It("NewMaxEverAggregateFunction", func() {
			operand := Ungrouped(Field("price"))
			fn := NewMaxEverAggregateFunction(operand)
			Expect(fn.Name).To(Equal(FunctionNameMaxEver))
			Expect(fn.Operand).To(Equal(operand))
			Expect(fn.Index).To(BeEmpty())
		})
	})

	Describe("canEvaluateAggregate", func() {
		Describe("COUNT index", func() {
			It("accepts matching count function", func() {
				idx := NewCountIndex("c", Ungrouped(EmptyKey()))
				fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(EmptyKey())}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects wrong function name", func() {
				idx := NewCountIndex("c", Ungrouped(EmptyKey()))
				fn := &IndexAggregateFunction{Name: FunctionNameSum, Operand: Ungrouped(EmptyKey())}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})

			It("rejects mismatched operand", func() {
				idx := NewCountIndex("c", Ungrouped(EmptyKey()))
				fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("COUNT_NOT_NULL index", func() {
			It("accepts matching count_not_null function", func() {
				idx := &Index{Name: "cnn", Type: IndexTypeCountNotNull, RootExpression: Ungrouped(Field("price")), subspaceKey: "cnn"}
				fn := &IndexAggregateFunction{Name: FunctionNameCountNotNull, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects count function on count_not_null index", func() {
				idx := &Index{Name: "cnn", Type: IndexTypeCountNotNull, RootExpression: Ungrouped(Field("price")), subspaceKey: "cnn"}
				fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("COUNT_UPDATES index", func() {
			It("accepts matching count_updates function", func() {
				idx := &Index{Name: "cu", Type: IndexTypeCountUpdates, RootExpression: Ungrouped(EmptyKey()), subspaceKey: "cu"}
				fn := &IndexAggregateFunction{Name: FunctionNameCountUpdates, Operand: Ungrouped(EmptyKey())}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects count function on count_updates index", func() {
				idx := &Index{Name: "cu", Type: IndexTypeCountUpdates, RootExpression: Ungrouped(EmptyKey()), subspaceKey: "cu"}
				fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(EmptyKey())}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("SUM index", func() {
			It("accepts matching sum function", func() {
				idx := NewSumIndex("s", Ungrouped(Field("price")))
				fn := &IndexAggregateFunction{Name: FunctionNameSum, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects count function on sum index", func() {
				idx := NewSumIndex("s", Ungrouped(Field("price")))
				fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("MAX_EVER indexes", func() {
			It("accepts max_ever on MAX_EVER_LONG index", func() {
				idx := NewMaxEverLongIndex("me", Ungrouped(Field("price")))
				fn := &IndexAggregateFunction{Name: FunctionNameMaxEver, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts max_ever on MAX_EVER_TUPLE index", func() {
				idx := &Index{Name: "met", Type: IndexTypeMaxEverTuple, RootExpression: Ungrouped(Field("price")), subspaceKey: "met"}
				fn := &IndexAggregateFunction{Name: FunctionNameMaxEver, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts max_ever on MAX_EVER_VERSION index", func() {
				idx := &Index{Name: "mev", Type: IndexTypeMaxEverVersion, RootExpression: Ungrouped(VersionKey()), subspaceKey: "mev"}
				fn := &IndexAggregateFunction{Name: FunctionNameMaxEver, Operand: Ungrouped(VersionKey())}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects min_ever on MAX_EVER_LONG index", func() {
				idx := NewMaxEverLongIndex("me", Ungrouped(Field("price")))
				fn := &IndexAggregateFunction{Name: FunctionNameMinEver, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("MIN_EVER indexes", func() {
			It("accepts min_ever on MIN_EVER_LONG index", func() {
				idx := NewMinEverLongIndex("me", Ungrouped(Field("price")))
				fn := &IndexAggregateFunction{Name: FunctionNameMinEver, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts min_ever on MIN_EVER_TUPLE index", func() {
				idx := &Index{Name: "met", Type: IndexTypeMinEverTuple, RootExpression: Ungrouped(Field("price")), subspaceKey: "met"}
				fn := &IndexAggregateFunction{Name: FunctionNameMinEver, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects max_ever on MIN_EVER_LONG index", func() {
				idx := NewMinEverLongIndex("me", Ungrouped(Field("price")))
				fn := &IndexAggregateFunction{Name: FunctionNameMaxEver, Operand: Ungrouped(Field("price"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("PERMUTED_MIN/MAX indexes", func() {
			It("accepts min on PERMUTED_MIN index", func() {
				idx := NewPermutedMinIndex("pm", GroupBy(Field("score"), Field("game")), 1)
				fn := &IndexAggregateFunction{Name: FunctionNameMin, Operand: GroupBy(Field("score"), Field("game"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts max on PERMUTED_MAX index", func() {
				idx := NewPermutedMaxIndex("pm", GroupBy(Field("score"), Field("game")), 1)
				fn := &IndexAggregateFunction{Name: FunctionNameMax, Operand: GroupBy(Field("score"), Field("game"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects max on PERMUTED_MIN index", func() {
				idx := NewPermutedMinIndex("pm", GroupBy(Field("score"), Field("game")), 1)
				fn := &IndexAggregateFunction{Name: FunctionNameMax, Operand: GroupBy(Field("score"), Field("game"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})

			It("rejects min on PERMUTED_MAX index", func() {
				idx := NewPermutedMaxIndex("pm", GroupBy(Field("score"), Field("game")), 1)
				fn := &IndexAggregateFunction{Name: FunctionNameMin, Operand: GroupBy(Field("score"), Field("game"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("VALUE index", func() {
			It("accepts min on VALUE index", func() {
				idx := NewIndex("v", Field("price"))
				fn := &IndexAggregateFunction{Name: FunctionNameMin, Operand: Field("price")}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts max on VALUE index", func() {
				idx := NewIndex("v", Field("price"))
				fn := &IndexAggregateFunction{Name: FunctionNameMax, Operand: Field("price")}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects count on VALUE index", func() {
				idx := NewIndex("v", Field("price"))
				fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Field("price")}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})

			It("rejects sum on VALUE index", func() {
				idx := NewIndex("v", Field("price"))
				fn := &IndexAggregateFunction{Name: FunctionNameSum, Operand: Field("price")}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})

			It("rejects min when operand is not prefix of index", func() {
				idx := NewIndex("v", Field("price"))
				fn := &IndexAggregateFunction{Name: FunctionNameMin, Operand: Field("quantity")}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("BITMAP_VALUE index", func() {
			It("accepts bitmap_value function", func() {
				idx := NewBitmapValueIndex("bv", Ungrouped(Field("bits")))
				fn := &IndexAggregateFunction{Name: FunctionNameBitmapValue, Operand: Ungrouped(Field("bits"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects count on BITMAP_VALUE index", func() {
				idx := NewBitmapValueIndex("bv", Ungrouped(Field("bits")))
				fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(Field("bits"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("TIME_WINDOW_LEADERBOARD index", func() {
			It("accepts time_window_count function", func() {
				root := Ungrouped(Field("score"))
				idx := NewTimeWindowLeaderboardIndex("twl", root)
				fn := &IndexAggregateFunction{Name: FunctionNameTimeWindowCount, Operand: root}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts score_for_time_window_rank function", func() {
				root := Ungrouped(Field("score"))
				idx := NewTimeWindowLeaderboardIndex("twl", root)
				fn := &IndexAggregateFunction{Name: FunctionNameScoreForTimeWindowRank, Operand: root}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts score_for_time_window_rank_else_skip function", func() {
				root := Ungrouped(Field("score"))
				idx := NewTimeWindowLeaderboardIndex("twl", root)
				fn := &IndexAggregateFunction{Name: FunctionNameScoreForTimeWindowRankElseSkip, Operand: root}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts time_window_rank_for_score function", func() {
				root := Ungrouped(Field("score"))
				idx := NewTimeWindowLeaderboardIndex("twl", root)
				fn := &IndexAggregateFunction{Name: FunctionNameTimeWindowRankForScore, Operand: root}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects regular count on TIME_WINDOW_LEADERBOARD", func() {
				root := Ungrouped(Field("score"))
				idx := NewTimeWindowLeaderboardIndex("twl", root)
				fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: root}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})

			It("rejects time_window function with mismatched operand", func() {
				root := Ungrouped(Field("score"))
				idx := NewTimeWindowLeaderboardIndex("twl", root)
				fn := &IndexAggregateFunction{Name: FunctionNameTimeWindowCount, Operand: Ungrouped(Field("other"))}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("RANK index via canEvaluateAggregate", func() {
			It("accepts count_distinct on RANK index", func() {
				root := Ungrouped(Field("score"))
				idx := NewRankIndex("r", root)
				fn := &IndexAggregateFunction{Name: FunctionNameCountDistinct, Operand: root}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts rank_for_score on RANK index", func() {
				root := Ungrouped(Field("score"))
				idx := NewRankIndex("r", root)
				fn := &IndexAggregateFunction{Name: FunctionNameRankForScore, Operand: root}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts score_for_rank on RANK index", func() {
				root := Ungrouped(Field("score"))
				idx := NewRankIndex("r", root)
				fn := &IndexAggregateFunction{Name: FunctionNameScoreForRank, Operand: root}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("accepts score_for_rank_else_skip on RANK index", func() {
				root := Ungrouped(Field("score"))
				idx := NewRankIndex("r", root)
				fn := &IndexAggregateFunction{Name: FunctionNameScoreForRankElseSkip, Operand: root}
				Expect(canEvaluateAggregate(fn, idx)).To(BeTrue())
			})

			It("rejects sum on RANK index", func() {
				root := Ungrouped(Field("score"))
				idx := NewRankIndex("r", root)
				fn := &IndexAggregateFunction{Name: FunctionNameSum, Operand: root}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})

		Describe("unknown index type", func() {
			It("rejects any function on unknown index type", func() {
				idx := &Index{Name: "x", Type: "nonexistent_type", RootExpression: Field("price"), subspaceKey: "x"}
				fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Field("price")}
				Expect(canEvaluateAggregate(fn, idx)).To(BeFalse())
			})
		})
	})

	Describe("canEvaluateRankAggregate", func() {
		It("count_distinct requires exact expression match", func() {
			root := Ungrouped(Field("score"))
			idx := NewRankIndex("r", root)

			fn := &IndexAggregateFunction{Name: FunctionNameCountDistinct, Operand: root}
			Expect(canEvaluateRankAggregate(fn, idx)).To(BeTrue())

			fn2 := &IndexAggregateFunction{Name: FunctionNameCountDistinct, Operand: Ungrouped(Field("other"))}
			Expect(canEvaluateRankAggregate(fn2, idx)).To(BeFalse())
		})

		It("count requires unique RANK index", func() {
			root := Ungrouped(Field("score"))
			idx := NewRankIndex("r", root)
			// Not unique by default
			fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(EmptyKey())}
			Expect(canEvaluateRankAggregate(fn, idx)).To(BeFalse())

			// Make it unique — still false because isGroupPrefix fails for
			// Ungrouped(EmptyKey()) vs Ungrouped(Field("score"))
			idx.Options = map[string]string{IndexOptionUnique: "true"}
			fn2 := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(EmptyKey())}
			Expect(canEvaluateRankAggregate(fn2, idx)).To(BeFalse())
		})

		It("count on unique RANK rejects operand with wrong column size", func() {
			root := Ungrouped(Field("score"))
			idx := NewRankIndex("r", root)
			idx.Options = map[string]string{IndexOptionUnique: "true"}

			// Operand has columnSize=1, groupingCount of root is 0 -> mismatch
			fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Field("score")}
			Expect(canEvaluateRankAggregate(fn, idx)).To(BeFalse())
		})

		It("count on unique RANK with grouped root rejects non-prefix operand", func() {
			// GroupBy(Field("score"), Field("game")) -> groupingCount = 1 (game), grouped = 1 (score)
			root := GroupBy(Field("score"), Field("game"))
			idx := NewRankIndex("r", root)
			idx.Options = map[string]string{IndexOptionUnique: "true"}

			// Operand columnSize matches groupingCount (1), but isGroupPrefix
			// compares structurally and Field("game") != GroupBy's grouping subexpression.
			fn := &IndexAggregateFunction{Name: FunctionNameCount, Operand: Field("game")}
			Expect(canEvaluateRankAggregate(fn, idx)).To(BeFalse())
		})

		It("rank_for_score requires exact expression match", func() {
			root := Ungrouped(Field("score"))
			idx := NewRankIndex("r", root)

			fn := &IndexAggregateFunction{Name: FunctionNameRankForScore, Operand: root}
			Expect(canEvaluateRankAggregate(fn, idx)).To(BeTrue())

			fn2 := &IndexAggregateFunction{Name: FunctionNameRankForScore, Operand: Ungrouped(Field("other"))}
			Expect(canEvaluateRankAggregate(fn2, idx)).To(BeFalse())
		})

		It("score_for_rank requires exact expression match", func() {
			root := Ungrouped(Field("score"))
			idx := NewRankIndex("r", root)

			fn := &IndexAggregateFunction{Name: FunctionNameScoreForRank, Operand: root}
			Expect(canEvaluateRankAggregate(fn, idx)).To(BeTrue())
		})

		It("score_for_rank_else_skip requires exact expression match", func() {
			root := Ungrouped(Field("score"))
			idx := NewRankIndex("r", root)

			fn := &IndexAggregateFunction{Name: FunctionNameScoreForRankElseSkip, Operand: root}
			Expect(canEvaluateRankAggregate(fn, idx)).To(BeTrue())
		})

		It("rejects unsupported function name", func() {
			root := Ungrouped(Field("score"))
			idx := NewRankIndex("r", root)

			fn := &IndexAggregateFunction{Name: FunctionNameBitmapValue, Operand: root}
			Expect(canEvaluateRankAggregate(fn, idx)).To(BeFalse())
		})
	})

	Describe("isGroupPrefix", func() {
		It("returns true for structurally equal expressions", func() {
			expr1 := Ungrouped(Field("price"))
			expr2 := Ungrouped(Field("price"))
			Expect(isGroupPrefix(expr1, expr2)).To(BeTrue())
		})

		It("returns true when operand grouping is a prefix of index grouping", func() {
			// Index: GroupBy(Field("score"), Field("game"), Field("region"))
			// -> grouping = [game, region], grouped = [score]
			indexRoot := GroupBy(Field("score"), Field("game"), Field("region"))

			// Operand: GroupBy(Field("score"), Field("game"))
			// -> grouping = [game], grouped = [score]
			operand := GroupBy(Field("score"), Field("game"))

			Expect(isGroupPrefix(operand, indexRoot)).To(BeTrue())
		})

		It("returns false when operand grouping is longer than index grouping", func() {
			indexRoot := GroupBy(Field("score"), Field("game"))
			operand := GroupBy(Field("score"), Field("game"), Field("region"))
			Expect(isGroupPrefix(operand, indexRoot)).To(BeFalse())
		})

		It("returns false when grouped portions differ in length", func() {
			// operand: 1 grouped column (price)
			operand := Ungrouped(Field("price"))
			// index: 0 grouped columns
			indexRoot := GroupAll(Field("price"))
			// operandGrouped = [Field("price")], indexGrouped = [] -> length mismatch
			Expect(isGroupPrefix(operand, indexRoot)).To(BeFalse())
		})

		It("returns false when grouped columns differ structurally", func() {
			operand := Ungrouped(Field("price"))
			indexRoot := Ungrouped(Field("quantity"))
			Expect(isGroupPrefix(operand, indexRoot)).To(BeFalse())
		})

		It("returns false when grouping columns differ structurally", func() {
			operand := GroupBy(Field("score"), Field("game"))
			indexRoot := GroupBy(Field("score"), Field("region"))
			Expect(isGroupPrefix(operand, indexRoot)).To(BeFalse())
		})

		It("handles EmptyKey operand with EmptyKey index", func() {
			operand := Ungrouped(EmptyKey())
			indexRoot := Ungrouped(EmptyKey())
			Expect(isGroupPrefix(operand, indexRoot)).To(BeTrue())
		})

		It("handles Concat expressions", func() {
			operand := GroupAll(Concat(Field("a"), Field("b")))
			indexRoot := GroupAll(Concat(Field("a"), Field("b"), Field("c")))
			// operand has 2 grouping columns (a, b), index has 3 (a, b, c)
			// operand grouping is a prefix of index grouping -> true
			Expect(isGroupPrefix(operand, indexRoot)).To(BeTrue())
		})

		It("rejects Concat when non-prefix", func() {
			operand := GroupAll(Concat(Field("x"), Field("b")))
			indexRoot := GroupAll(Concat(Field("a"), Field("b"), Field("c")))
			Expect(isGroupPrefix(operand, indexRoot)).To(BeFalse())
		})

		It("handles NestingKeyExpression via normalizeKeyForPositions", func() {
			// Nest("parent", Field("child")) normalizes to [Nest("parent", Field("child"))]
			operand := GroupAll(Nest("parent", Field("child")))
			indexRoot := GroupAll(Concat(Nest("parent", Field("child")), Field("extra")))
			// operand grouping = [Nest(parent, child)], index grouping = [Nest(parent, child), Field(extra)]
			// prefix match
			Expect(isGroupPrefix(operand, indexRoot)).To(BeTrue())
		})

		It("rejects NestingKeyExpression with different parent", func() {
			operand := GroupAll(Nest("parent1", Field("child")))
			indexRoot := GroupAll(Nest("parent2", Field("child")))
			Expect(isGroupPrefix(operand, indexRoot)).To(BeFalse())
		})

		It("rejects NestingKeyExpression with different child", func() {
			operand := GroupAll(Nest("parent", Field("child1")))
			indexRoot := GroupAll(Nest("parent", Field("child2")))
			Expect(isGroupPrefix(operand, indexRoot)).To(BeFalse())
		})
	})

	Describe("isUngroupedPrefixOf", func() {
		It("returns true for identical expressions", func() {
			Expect(isUngroupedPrefixOf(Field("price"), Field("price"))).To(BeTrue())
		})

		It("returns true when operand is prefix of index", func() {
			operand := Field("price")
			indexRoot := Concat(Field("price"), Field("quantity"))
			Expect(isUngroupedPrefixOf(operand, indexRoot)).To(BeTrue())
		})

		It("returns false when operand is longer than index", func() {
			operand := Concat(Field("price"), Field("quantity"))
			indexRoot := Field("price")
			Expect(isUngroupedPrefixOf(operand, indexRoot)).To(BeFalse())
		})

		It("returns false when fields differ", func() {
			operand := Field("x")
			indexRoot := Field("y")
			Expect(isUngroupedPrefixOf(operand, indexRoot)).To(BeFalse())
		})

		It("handles GroupingKeyExpression by delegating to wholeKey", func() {
			operand := GroupBy(Field("score"), Field("game"))
			indexRoot := Concat(Field("game"), Field("score"), Field("extra"))
			// normalizeKeyForPositions on GroupBy(Field("score"), Field("game")) =>
			// normalizeKeyForPositions(Concat(Field("game"), Field("score"))) = [Field("game"), Field("score")]
			// indexRoot normalizes to [Field("game"), Field("score"), Field("extra")]
			Expect(isUngroupedPrefixOf(operand, indexRoot)).To(BeTrue())
		})

		It("handles NestingKeyExpression", func() {
			operand := Nest("parent", Field("child"))
			indexRoot := Concat(Nest("parent", Field("child")), Field("extra"))
			Expect(isUngroupedPrefixOf(operand, indexRoot)).To(BeTrue())
		})

		It("rejects NestingKeyExpression mismatch", func() {
			operand := Nest("parent", Field("child1"))
			indexRoot := Nest("parent", Field("child2"))
			Expect(isUngroupedPrefixOf(operand, indexRoot)).To(BeFalse())
		})
	})

	Describe("getGroupingExprs", func() {
		It("returns all columns for non-GroupingKeyExpression", func() {
			expr := Concat(Field("a"), Field("b"))
			result := getGroupingExprs(expr)
			Expect(result).To(HaveLen(2))
		})

		It("returns grouping columns for GroupingKeyExpression", func() {
			// GroupBy(Field("score"), Field("game"), Field("region")) ->
			//   wholeKey = Concat(game, region, score), groupedCount = 1
			//   groupingCount = 3 - 1 = 2
			//   all = [game, region, score], grouping = [:2] = [game, region]
			expr := GroupBy(Field("score"), Field("game"), Field("region"))
			result := getGroupingExprs(expr)
			Expect(result).To(HaveLen(2))
		})

		It("returns all columns for GroupAll", func() {
			// GroupAll(Concat(Field("a"), Field("b"))) -> groupedCount=0, groupingCount=2
			expr := GroupAll(Concat(Field("a"), Field("b")))
			result := getGroupingExprs(expr)
			Expect(result).To(HaveLen(2))
		})

		It("returns empty for Ungrouped (all columns are grouped)", func() {
			// Ungrouped(Field("price")) -> groupedCount=1, groupingCount=0
			expr := Ungrouped(Field("price"))
			result := getGroupingExprs(expr)
			Expect(result).To(BeEmpty())
		})
	})

	Describe("getGroupedExprs", func() {
		It("returns nil for non-GroupingKeyExpression", func() {
			expr := Field("price")
			result := getGroupedExprs(expr)
			Expect(result).To(BeNil())
		})

		It("returns grouped columns for GroupingKeyExpression", func() {
			// GroupBy(Field("score"), Field("game")) ->
			//   wholeKey = Concat(game, score), groupedCount = 1
			//   all = [game, score], grouping = [game]
			//   grouped = all[groupingCount:] = [score]
			expr := GroupBy(Field("score"), Field("game"))
			result := getGroupedExprs(expr)
			Expect(result).To(HaveLen(1))
		})

		It("returns empty slice for GroupAll (no grouped columns)", func() {
			expr := GroupAll(Field("price"))
			result := getGroupedExprs(expr)
			Expect(result).To(BeEmpty())
		})

		It("returns all columns for Ungrouped", func() {
			// Ungrouped(Concat(Field("a"), Field("b"))) -> groupedCount=2, groupingCount=0
			// all = [a, b], grouped = all[0:] = [a, b]
			expr := Ungrouped(Concat(Field("a"), Field("b")))
			result := getGroupedExprs(expr)
			Expect(result).To(HaveLen(2))
		})
	})

	Describe("tupleGreater", func() {
		It("returns true when a > b", func() {
			Expect(tupleGreater(tuple.Tuple{int64(10)}, tuple.Tuple{int64(5)})).To(BeTrue())
		})

		It("returns false when a < b", func() {
			Expect(tupleGreater(tuple.Tuple{int64(5)}, tuple.Tuple{int64(10)})).To(BeFalse())
		})

		It("returns false when a == b", func() {
			Expect(tupleGreater(tuple.Tuple{int64(5)}, tuple.Tuple{int64(5)})).To(BeFalse())
		})

		It("compares strings", func() {
			Expect(tupleGreater(tuple.Tuple{"z"}, tuple.Tuple{"a"})).To(BeTrue())
			Expect(tupleGreater(tuple.Tuple{"a"}, tuple.Tuple{"z"})).To(BeFalse())
		})

		It("compares multi-element tuples", func() {
			Expect(tupleGreater(tuple.Tuple{int64(1), int64(2)}, tuple.Tuple{int64(1), int64(1)})).To(BeTrue())
			Expect(tupleGreater(tuple.Tuple{int64(1), int64(1)}, tuple.Tuple{int64(1), int64(2)})).To(BeFalse())
		})
	})

	Describe("tupleLess", func() {
		It("returns true when a < b", func() {
			Expect(tupleLess(tuple.Tuple{int64(5)}, tuple.Tuple{int64(10)})).To(BeTrue())
		})

		It("returns false when a > b", func() {
			Expect(tupleLess(tuple.Tuple{int64(10)}, tuple.Tuple{int64(5)})).To(BeFalse())
		})

		It("returns false when a == b", func() {
			Expect(tupleLess(tuple.Tuple{int64(5)}, tuple.Tuple{int64(5)})).To(BeFalse())
		})

		It("compares strings", func() {
			Expect(tupleLess(tuple.Tuple{"a"}, tuple.Tuple{"z"})).To(BeTrue())
			Expect(tupleLess(tuple.Tuple{"z"}, tuple.Tuple{"a"})).To(BeFalse())
		})
	})

	Describe("splitEqualRangeForRank", func() {
		It("returns nil for nil low range", func() {
			groupPrefix, trailing, err := splitEqualRangeForRank(TupleRangeAll, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(groupPrefix).To(BeNil())
			Expect(trailing).To(BeNil())
		})

		It("returns only group prefix when values <= groupPrefixSize", func() {
			scanRange := TupleRange{
				Low:         tuple.Tuple{int64(42)},
				High:        tuple.Tuple{int64(42)},
				LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeRangeInclusive,
			}
			groupPrefix, trailing, err := splitEqualRangeForRank(scanRange, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(groupPrefix).To(Equal([]any{int64(42)}))
			Expect(trailing).To(BeNil())
		})

		It("splits group prefix and trailing values", func() {
			scanRange := TupleRange{
				Low:         tuple.Tuple{int64(1), int64(100)},
				High:        tuple.Tuple{int64(1), int64(100)},
				LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeRangeInclusive,
			}
			groupPrefix, trailing, err := splitEqualRangeForRank(scanRange, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(groupPrefix).To(Equal([]any{int64(1)}))
			Expect(trailing).To(Equal(tuple.Tuple{int64(100)}))
		})

		It("handles zero group prefix size", func() {
			scanRange := TupleRange{
				Low:         tuple.Tuple{int64(50)},
				High:        tuple.Tuple{int64(50)},
				LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeRangeInclusive,
			}
			groupPrefix, trailing, err := splitEqualRangeForRank(scanRange, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(groupPrefix).To(BeEmpty())
			Expect(trailing).To(Equal(tuple.Tuple{int64(50)}))
		})

		It("handles multiple trailing values", func() {
			scanRange := TupleRange{
				Low:         tuple.Tuple{int64(1), int64(2), int64(3)},
				High:        tuple.Tuple{int64(1), int64(2), int64(3)},
				LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeRangeInclusive,
			}
			groupPrefix, trailing, err := splitEqualRangeForRank(scanRange, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(groupPrefix).To(Equal([]any{int64(1)}))
			Expect(trailing).To(Equal(tuple.Tuple{int64(2), int64(3)}))
		})

		It("handles multiple group prefix elements", func() {
			scanRange := TupleRange{
				Low:         tuple.Tuple{int64(1), int64(2), int64(3)},
				High:        tuple.Tuple{int64(1), int64(2), int64(3)},
				LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeRangeInclusive,
			}
			groupPrefix, trailing, err := splitEqualRangeForRank(scanRange, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(groupPrefix).To(Equal([]any{int64(1), int64(2)}))
			Expect(trailing).To(Equal(tuple.Tuple{int64(3)}))
		})

		It("returns only group prefix when values exactly equal groupPrefixSize", func() {
			scanRange := TupleRange{
				Low:         tuple.Tuple{int64(1), int64(2)},
				High:        tuple.Tuple{int64(1), int64(2)},
				LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeRangeInclusive,
			}
			groupPrefix, trailing, err := splitEqualRangeForRank(scanRange, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(groupPrefix).To(Equal([]any{int64(1), int64(2)}))
			Expect(trailing).To(BeNil())
		})
	})
})
