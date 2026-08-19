package values

import (
	"sort"
	"strings"
)

// OrdinalSeedLegWindow is one leg's window in a pristine ordinal join seed's
// merged positional layout: the leg's starting slot and its flowed record
// type. The layout derivation lives in
// ONE place — this package — with the planner's existential rebase
// delegating here and the executor's span derivation pinned to agree by a
// cross-agreement fixture; independent walks drift, and layout drift is
// wrong-offset wrong-rows.
type OrdinalSeedLegWindow struct {
	// Kind says whether Offset starts a flat RUN of the leg's columns or names
	// the SINGLE slot holding the leg's whole row. Its zero value is
	// LegKindUnset, which is invalid: every reader below declines or fails loud
	// on it rather than defaulting to flatRun, because defaulting is an inference
	// about which column a read addresses and the language is not entitled to
	// make it. See LegKind's own doc for why no structural inference works.
	Kind LegKind

	// Offset is the leg's first slot in the merged row for a flatRun window, and
	// the leg's ONE slot for a nested one. Reading it without dispatching on Kind
	// is the wrong-offset wrong-rows failure this authority exists to prevent.
	Offset int
	// Typ is the leg's own record type, under BOTH kinds — never a one-field
	// wrapper describing the slot. Readers bound a leg-local ordinal against it
	// before composing, so a wrapper would decline every leg-local ordinal >= 1
	// and resolve ordinal 0 against the wrapper: a silent wrong-column read on
	// exactly the shape the bound check exists to catch.
	Typ *RecordType

	// Alias is the window's leg IDENTITY: the correlation of the quantifier whose
	// row occupies this window, carried VERBATIM from the seed's own
	// QuantifiedObjectValue.
	//
	// It used to be minted from the map KEY instead — NamedCorrelationIdentifier of
	// the upper-folded alias — with the QOV's correlation in scope at both
	// construction sites. That kept Name == Alias.Name() trivially true, but by
	// making the identity a function of the text rather than the other way round:
	// the fold is a no-op where the correlation is already upper and manufactures a
	// forgery where it is not, since the machine namespace is LOWERCASE and folding
	// a minted q$N yields the Q$N that SameLeg exists to exclude.
	//
	// The map's KEYS are now this same identifier, so Alias is no longer one of two
	// namespaces held apart — it is the only one. Every keyed reader was measured
	// first, by a census built to answer exactly that question and retired with it:
	// over the real-FDB corpus all 1400 lookups had a correlation in hand and the
	// identity selected the same window the fold did, on every one; the two readers
	// that had only text were unreachable by panic probe across the whole
	// relational tree. That is a DATED POINT MEASUREMENT of a namespace that no
	// longer exists, not a live claim.
	//
	// What stands in its place is the seed-window READER census
	// (seed_window_reader_census.go), a STANDING instrument: it floors each of the
	// five keyed readers so one going dark reds instead of printing a clean-looking
	// zero, and it hard-zeros the two DECLINE classes that replaced the text
	// lookups. The conversion's own evidence is history; the readers' continued
	// exercise is checked on every suite run.
	//
	// What the key change buys is not tidiness. A text key merges the two alias
	// namespaces the rest of this package keeps deliberately DISJOINT — user
	// correlations upper-folded at the semantic scope, machine mints lowercase — so
	// a quoted "q$5" and a planner-minted q$5 were one key while being two legs.
	// SameLeg exists to refuse exactly that, and the map used to undo it.
	Alias CorrelationIdentifier
}

// OrdinalSeedLegWindows derives per-leg windows (leg IDENTITY → window, in a
// map) plus the merged row's RecordType from a gated ordinal seed RC. TWO shapes
// are accepted (decline-not-panic — nil windows for anything else:
// translated/fused, folded, positional-merge):
//
//   - PRISTINE (fully-baked AS+AT, or the 2+1 join seed): EVERY field a
//     single-accessor frontier-pinned bake over a leg QOV, consecutive
//     full-coverage runs (AssertOrdinalJoinSeed's shape).
//   - MIXED single-source lateral-unnest (no-AT): a full baked OUTER leg run
//     followed by EXACTLY ONE trailing bare-QuantifiedObjectValue element over a
//     NON-record type (Java's isPrimitive() whole-object scalar element, which
//     cannot be ofOrdinal-baked). Its OWN 1-field leg window is synthesized so
//     `<AS>.<AS>` resolves positionally — the element and the outer each carry
//     their own ALIAS.COL namespace, which is what stops a name shared by the
//     element AS alias and an outer column from mis-resolving.
//
// This MUST agree bit-for-bit with the executor's ordinalJoinSpans/
// unnestMixedSeedSpans (the cross-agreement invariant — independent walks drift,
// and layout drift is wrong-offset wrong-rows; pinned by a fixture).
//
// The merged type's field names are the seed's OUTPUT names in order (the element
// name uppercased to match the executor); duplicates SURVIVE (positional access).
//
// THIS ENTRY'S ACCEPT SET IS FROZEN, and freezing it is a design decision rather
// than inertia. It has many call sites and most of them consume the result as a
// nil/non-nil PREDICATE — "is this an ordinal seed?" — and at a nil/non-nil
// predicate, POPULATION IS MEANING: a shape that starts returning non-nil does
// not keep the same semantics over a larger population, it flips that consumer's
// branch. Widening here to admit the nested kind would silently change rule arms
// nobody analysed. The nested acceptance is
// OrdinalSeedLegWindowsAcceptingNested, a separate opt-in entry.
//
// It also DECLINES, fail-closed, any seed carrying a nested leg — rather than
// returning it top-level-only. A caller given top-level-only windows would
// silently be missing sub-windows it would have had for a flat box leg, and a
// declined optimization is recoverable while a wrong ordinal is not.
func OrdinalSeedLegWindows(rc *RecordConstructorValue) (map[CorrelationIdentifier]OrdinalSeedLegWindow, *RecordType) {
	w, m, _ := ordinalSeedLegWindows(rc, false)
	return w, m
}

// OrdinalSeedLegLayout is OrdinalSeedLegWindowsAcceptingNested plus the
// TOP-LEVEL RUN LIST: the windows that TILE the merged row, in offset order.
//
// THE MAP CANNOT SERVE THIS, and that is why the walk returns it rather than a
// caller deriving it. finalizeSeedWindows' rightmost-leaf case REPLACES a box
// run's own entry with a narrower sub-window — deliberately, because the box IS
// its rightmost leaf under the sourceBinding convention and an alias-qualified
// read must window the leaf rather than look the name up across the whole
// concat. So
// after finalization "the windows that tile the row" is simply not recoverable
// from the map: one of the tiles has been overwritten by something narrower, and
// nothing distinguishes that from a seed that always had a narrow leg there.
//
// It is the planner twin of what the executor already returns —
// ordinalJoinSpansOf's `spans []legSpan`, in offset order — which is the shape
// this list is modelled on rather than invented against.
//
// Callers that only need the addressable map keep the two-value entries; this
// answers the different question "how many legs TILE this row, and what shape is
// each".
//
// ITS ONE PRODUCTION CONSUMER IS GONE. It served an orientation check inside the
// three-quantifier NLJ arm, and both retired with RFC-235. The run list is kept
// because the question it answers is not derivable from the map — finalization
// replaces a box run's entry with a narrower sub-window, after which the map no
// longer states which windows tile the row — so a future consumer would have to
// rebuild exactly this. A caller returning here should say why the map cannot
// serve it.
func OrdinalSeedLegLayout(rc *RecordConstructorValue) (map[CorrelationIdentifier]OrdinalSeedLegWindow, *RecordType, []OrdinalSeedLegWindow) {
	return ordinalSeedLegWindows(rc, true)
}

// OrdinalSeedLegWindowsAcceptingNested is OrdinalSeedLegWindows plus the NESTED
// leg kind (RFC-200). It is a separate entry point, opted into by exactly three
// sites, and the separation is the whole design: see OrdinalSeedLegWindows for
// why widening the shared boundary would flip consumers this acceptance never
// analysed.
//
// The flag controls exactly two decisions and nothing else:
//
//   - whether a whole-RC POSITIONAL MERGE is recognized at the head, yielding one
//     nested window per slot;
//   - whether finalizeSeedWindows emits a nested SUB-window for a nested leg of a
//     carrying run, instead of declining the seed.
//
// Everything else — the pristine walk, the mixed-element walk, the coverage
// checks, the >= 2 window rule — is byte-identical between the two entries.
func OrdinalSeedLegWindowsAcceptingNested(rc *RecordConstructorValue) (map[CorrelationIdentifier]OrdinalSeedLegWindow, *RecordType) {
	w, m, _ := ordinalSeedLegWindows(rc, true)
	return w, m
}

func ordinalSeedLegWindows(rc *RecordConstructorValue, acceptNested bool) (map[CorrelationIdentifier]OrdinalSeedLegWindow, *RecordType, []OrdinalSeedLegWindow) {
	if rc == nil || len(rc.Fields) == 0 {
		return nil, nil, nil
	}
	// THE WHOLE-RC RECOGNIZER RUNS BEFORE THE PER-FIELD WALK, and the ordering is
	// LOAD-BEARING rather than stylistic.
	//
	// The walk below tests isMixedSeedElement FIRST, and an UNTYPED slot is not a
	// *RecordType, so IsMixedSeedElementType returns true for it. A merge row with
	// untyped slots — which the corpus has, 750 of 18246 merge slots state no type
	// at all, every distinct witness an unnest ELEMENT alias whose array element
	// type Go does not infer — would therefore have those slots claimed as scalar
	// elements before any merge recognition could run. The recognizer has to
	// precede the loop or it can only ever see the fully-typed subset.
	if acceptNested && IsPositionalMergeRC(rc) {
		return positionalMergeWindows(rc)
	}
	n := len(rc.Fields)
	windows := map[CorrelationIdentifier]OrdinalSeedLegWindow{}
	mergedFields := make([]Field, n)
	counts := map[CorrelationIdentifier]int{}
	// names carries each window's DISPLAY binding for the merged type's leg
	// table, which still has live text readers (the dotted channel). It is a
	// producer-local side table rather than the map's key, and the difference is
	// the whole conversion: a display name that TRAVELS with a window is a label,
	// while a display name that SELECTS one is an identity decided by text.
	names := map[CorrelationIdentifier]string{}
	var curAlias CorrelationIdentifier
	var curStart int
	for i := 0; i < n; i++ {
		f := rc.Fields[i]
		// The MIXED scalar element (a bare QOV over a NON-record type — the
		// whole-object element the seed cannot ofOrdinal-bake) can appear at ANY
		// FROM position: `FROM A, B, A.arr AS x` (trailing), `FROM A, A.arr AS x, B`
		// (mid-list — splits the leg run), `FROM A.arr AS x, B` (leading). Synthesize
		// its OWN 1-field window at its slot — an internal step-over that offsets the
		// trailing legs — and RESET the run so a leg AFTER the element windows fresh.
		// The element's window lets its `<AS>.<AS>` shadow read resolve, and (H)'s
		// field-id in the group-by consumes the element by rc index either way.
		if isMixedSeedElement(f) {
			elemQOV := f.Value.(*quantifiedObjectValue)
			elemName := strings.ToUpper(f.Name)
			if _, dup := windows[elemQOV.correlation]; dup {
				return nil, nil, nil // two element fields over one correlation — not a seed
			}
			// The window's IDENTITY is the element QOV's own correlation, carried —
			// and it is now the KEY as well. It used to be minted as
			// NamedCorrelationIdentifier of the UPPER fold of that same correlation,
			// with the correlation itself in scope: an identifier manufactured from
			// the text of the identifier beside it. The fold is a no-op wherever the
			// correlation is already upper and a FORGERY generator wherever it is not
			// — the machine namespace is lowercase, so folding a minted q$N yields
			// Q$N, which is exactly the spelling SameLeg exists to keep out of the
			// minted leg's window.
			windows[elemQOV.correlation] = OrdinalSeedLegWindow{
				// The mixed seed's scalar element is a synthesized ONE-COLUMN flat
				// run, numerically identical to what it has always been. Stamped
				// rather than left to the zero value because a synthesized window is
				// exactly the kind of producer that never decided.
				Kind:   LegKindFlatRun,
				Offset: i,
				Typ:    &RecordType{Fields: []Field{{Name: elemName, FieldType: elemQOV.Type(), Ordinal: 0}}},
				Alias:  elemQOV.correlation,
			}
			counts[elemQOV.correlation] = 1
			names[elemQOV.correlation] = strings.ToUpper(elemQOV.correlation.Name())
			mergedFields[i] = Field{Name: elemName, FieldType: elemQOV.Type(), Ordinal: i}
			curAlias = CorrelationIdentifier{}
			continue
		}
		fv, isFV := f.Value.(*fieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return nil, nil, nil
		}
		acc, single := fv.Resolved.Single()
		if !single {
			return nil, nil, nil
		}
		qov, isQOV := fv.Child.(*quantifiedObjectValue)
		if !isQOV {
			return nil, nil, nil
		}
		legType := qov.physicalFlowedRecordType()
		if legType == nil {
			return nil, nil, nil
		}
		alias := qov.correlation
		if alias != curAlias {
			if _, dup := windows[alias]; dup {
				return nil, nil, nil // a split run — not pristine
			}
			if acc.Ordinal != 0 {
				return nil, nil, nil
			}
			curAlias = alias
			curStart = i
			// Identity throughout: the leg QOV's own correlation is the window's Alias
			// AND the key it is filed under. The run boundary is the same comparison —
			// it used to compare the UPPER folds of two correlations, which merges a
			// pair the machine namespace keeps deliberately apart.
			// A baked leg RUN: consecutive slots, one per leg column.
			windows[alias] = OrdinalSeedLegWindow{Kind: LegKindFlatRun, Offset: curStart, Typ: legType, Alias: alias}
			names[alias] = strings.ToUpper(alias.Name())
		} else if acc.Ordinal != i-curStart {
			return nil, nil, nil
		}
		counts[alias]++
		mergedFields[i] = Field{Name: f.Name, FieldType: fv.Typ, Ordinal: i}
	}
	// Full coverage per window (a baked leg's run width == its leg type's field count;
	// the element's synthesized 1-field window has count 1).
	for alias, w := range windows {
		if counts[alias] != len(w.Typ.Fields) {
			return nil, nil, nil
		}
	}
	// ACCEPT-EQUIVALENCE with the executor twin (ordinalJoinSpansOf/unnestMixedSeedSpans,
	// pinned bit-for-bit by the cross-agreement fixture): a pristine ≥2-leg concat OR a
	// mixed seed (≥1 outer leg + the element window) — BOTH need ≥2 windows. A lone baked
	// leg is a folded projection; a lone element is not a gather. finalizeSeedWindows then
	// emits per-buried-leg sub-windows for any clustered BOX leg (box-alias→rightmost-leaf).
	if len(windows) < 2 {
		return nil, nil, nil
	}
	return finalizeSeedWindows(windows, names, mergedFields, acceptNested, topLevelRuns(windows))
}

// topLevelRuns snapshots the windows that TILE the merged row, in offset order,
// BEFORE finalizeSeedWindows can alter them.
//
// The snapshot has to be taken here and cannot be recovered later, and the
// reason is a deliberate feature of finalization rather than an accident: the
// rightmost-leaf case REPLACES a clustered box run's own entry with a narrower
// sub-window, because the box IS its rightmost leaf under the sourceBinding
// convention and an alias-qualified read must window the leaf rather than look
// the name up across the whole concat. After that, one of the tiles has been
// overwritten by something narrower and nothing in the map distinguishes it from
// a seed that always had a narrow leg there.
//
// Offset order, not map order, because the consumer asks a POSITIONAL question:
// which physical leg occupies which slot. Map iteration order is randomised in
// Go, so an unsorted list would make that consumer nondeterministic.
func topLevelRuns(windows map[CorrelationIdentifier]OrdinalSeedLegWindow) []OrdinalSeedLegWindow {
	runs := make([]OrdinalSeedLegWindow, 0, len(windows))
	for _, w := range windows {
		runs = append(runs, w)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Offset < runs[j].Offset })
	return runs
}

// positionalMergeWindows derives one window per slot of a POSITIONAL MERGE row —
// Java's own merged row, read directly.
//
// The shape is PartitionSelectRule.java:283-291's
// `RecordConstructorValue.ofColumns(… Column::unnamedOf)` verbatim: one unnamed
// column per collapsed lower quantifier, each holding that quantifier's WHOLE
// row. Go builds it 1:1 in positionalMergeCase. Java needs no window machinery
// for it at all — its row is a proto Message tree, RecordConstructorValue.eval
// sets one field per child WHOLE, and a reference to a collapsed sibling is a
// one-step ordinal walk (FieldValue.ofOrdinalNumber over the merge quantifier)
// that returns the leg's whole Message because the field is of message type.
// Nesting is free there. Here it needs a window that SAYS the slot is nested.
func positionalMergeWindows(rc *RecordConstructorValue) (map[CorrelationIdentifier]OrdinalSeedLegWindow, *RecordType, []OrdinalSeedLegWindow) {
	n := len(rc.Fields)
	windows := make(map[CorrelationIdentifier]OrdinalSeedLegWindow, n)
	names := make(map[CorrelationIdentifier]string, n)
	mergedFields := make([]Field, n)
	for i, f := range rc.Fields {
		// Guaranteed by IsPositionalMergeRC: every field is a bare QOV of a
		// DISTINCT quantifier, named OrdinalFieldName(i) in position order.
		qov := f.Value.(*quantifiedObjectValue)
		mergedFields[i] = Field{Name: f.Name, FieldType: qov.Type(), Ordinal: i}
		names[qov.correlation] = strings.ToUpper(qov.correlation.Name())

		// THE PER-SLOT RECORD TEST BINDS THE TYPE AND REQUIRES NON-NIL, and both
		// halves matter.
		//
		// Routing this through IsMixedSeedElementType instead would be a trap:
		// that predicate returns FALSE for a nil type, so a slot whose QOV states
		// nil would be classified NOT-an-element — i.e. nested — i.e. a window with
		// a nil Typ, which panics at the first w.Typ.FieldIndexUnique in the keyed
		// readers. The assertion is needed for Typ anyway; BINDING it is both the
		// correct test and the value the window carries. (Binding is also what the
		// single-authority ban permits — it forbids DISCARDED assertions.)
		legType, isRT := qov.FlowedType().(*RecordType)
		if !isRT || legType == nil {
			// A non-record slot keeps the EXISTING element treatment: a synthesized
			// 1-field flat window at its own slot, numerically identical to today.
			// Merge rows with such slots are real — the corpus has 750 of them, every
			// distinct witness an unnest ELEMENT alias.
			elemName := strings.ToUpper(f.Name)
			windows[qov.correlation] = OrdinalSeedLegWindow{
				Kind:   LegKindFlatRun,
				Offset: i,
				Typ:    &RecordType{Fields: []Field{{Name: elemName, FieldType: qov.Type(), Ordinal: 0}}},
				Alias:  qov.correlation,
			}
			continue
		}
		// Offset is the FIELD INDEX of the slot holding the whole leg row, not the
		// first column of a run. Typ is the LEG's own record type, so a reader's
		// leg-local bound check measures the leg's real width.
		windows[qov.correlation] = OrdinalSeedLegWindow{
			Kind:   LegKindNested,
			Offset: i,
			Typ:    legType,
			Alias:  qov.correlation,
		}
	}
	// IsPositionalMergeRC already requires >= 2 fields over DISTINCT quantifiers,
	// so the >= 2 window rule the other branches enforce holds by construction.
	return finalizeSeedWindows(windows, names, mergedFields, true, topLevelRuns(windows))
}

// finalizeSeedWindows runs the ADDITIVE per-buried-leg sub-window derivation over
// the top-level leg windows and builds the merged type's Legs — shared by the
// pristine (≥2-leg concat) and mixed (baked-prefix + element) branches so a
// clustered BOX outer disambiguates its buried leaves IDENTICALLY in both.
//
// A clustered box leg's type carries its buried-leg boundaries
// (RecordType.Legs, recorded by the translator's ordinalLegType — the one place
// that walks the box); a read qualified by a buried binding resolves positionally
// exactly like a top-level leg's (Java's rewire-by-ordinal — a buried source is
// just another window). Sub-windows are slices of a run (no counts), never
// overwrite a top-level run's own window.
func finalizeSeedWindows(windows map[CorrelationIdentifier]OrdinalSeedLegWindow, names map[CorrelationIdentifier]string, mergedFields []Field, acceptNested bool, runs []OrdinalSeedLegWindow) (map[CorrelationIdentifier]OrdinalSeedLegWindow, *RecordType, []OrdinalSeedLegWindow) {
	for _, w := range windows {
		// A NESTED window's own Typ is a leg row ONE LEVEL DOWN, so its .Legs
		// describe boundaries inside that row — two steps from the merged row, and
		// this authority has no two-step window to express them with.
		//
		// DECLINE the whole seed rather than skip them, which is the rule this
		// function already applies to an Alias-less leg: a seed missing a sub-window
		// is a seed whose qualified reads resolve to the WRONG SLOTS, so the honest
		// answer is no ordinal layout at all and the name model instead.
		if w.Kind == LegKindNested && len(w.Typ.Legs) > 0 {
			return nil, nil, nil
		}
		for li, leg := range w.Typ.Legs {
			if leg.Name == "" {
				continue
			}
			// THE NARROW ENTRY'S FAIL-CLOSED DECLINE. A seed carrying a nested leg is
			// refused outright rather than returned top-level-only: a caller given
			// top-level-only windows would silently be missing sub-windows it WOULD
			// have had for a flat box leg, and it has no way to tell the two apart.
			if leg.Kind == LegKindNested && !acceptNested {
				return nil, nil, nil
			}
			// A leg that states a NAME but no IDENTITY declines the whole seed.
			//
			// The map below is keyed by identity, so an Alias-less leg files under
			// the ZERO key — and a second one OVERWRITES it. Two buried leaves then
			// share one window: the first is silently dropped, the merged leg table
			// reports one entry where the box has two, and every read qualified by
			// the dropped leaf resolves into its sibling's slots. Nothing downstream
			// can notice, because a window filed under the zero key is
			// indistinguishable from a window nobody filed.
			//
			// NewRecordTypeLeg's doc states this hazard about its own construction
			// ("a leg whose identity is the zero CorrelationIdentifier — which every
			// reader then fails to bind, silently ... deleting `Alias:` from two
			// producers left the whole suite green"). The compile-time half of that
			// defence is the positional constructor; this is the runtime half, at the
			// one place where two such legs become one.
			//
			// LOUD, not skip-this-leg: a seed missing a sub-window is a seed whose
			// qualified reads resolve to the wrong slots, so the honest answer is no
			// ordinal layout at all and the name model instead.
			if leg.Alias.IsZero() {
				return nil, nil, nil
			}
			if LegIdentityCensusEnabled() {
				// The RETIRED predicate is reproduced from the identity so the census
				// keeps measuring what it always measured. The old `alias` was the map
				// key, which WAS the upper fold of this window's own correlation — so
				// spelling it that way here is not an approximation of the retired test,
				// it is the retired test with its input named honestly.
				RecordLegIdentityConversion(LegSiteFinalizeSeedWindows, leg.Alias, w.Alias,
					leg.Name == strings.ToUpper(w.Alias.Name()))
				RecordLegIdentityLeg(leg)
			}
			// "Is this buried leg the box run's RIGHTMOST LEAF?" — an IDENTITY question,
			// answered by the one comparison every identity question routes through.
			//
			// TWO producers mint the box QOV's correlation this compares against, and
			// they answer this predicate DIFFERENTLY. Both dispositions are intended:
			//
			//  1. The unnest/mixed and chained seeds mint it through the translator's
			//     unnestOuterCorrelation — sourceAlias(box), which recurses to a join's
			//     RIGHT operand and upper-folds at every arm. So the box's correlation IS
			//     its rightmost leaf's, and this predicate MATCHES for that leaf: the
			//     REPLACE branch below swaps the box run's window for the leaf's
			//     sub-window, which is what keeps an alias-qualified read off the concat.
			//     This is the sourceBinding convention — a box run is named, and
			//     identified, by its rightmost leaf — and the two spellings agree only
			//     because that one chokepoint folds.
			//  2. The PRISTINE gated-join seed mints it through the translator's
			//     legBinding — sourceBinding(box) + "$BOX" for a join leg — whose stated
			//     purpose is that the box level and the leaf level be DIFFERENT
			//     identifiers (one plan carries both, and a shared name collides in the
			//     widen invariant). So this predicate DECLINES for every buried leaf of
			//     such a box, rightmost included.
			//
			// The decline is not a dropped window. The map KEY is the buried leg's own
			// IDENTITY and the box run's key is the minted `C$BOX` binding's, so those
			// are two different identifiers: the `taken` test below misses and the leaf
			// sub-window is filed anyway — ADDED beside the box run rather than
			// replacing it. The retired TEXT predicate (`leg.Name == alias`) declined
			// that pair too, so the conversion changed nothing for this producer, and
			// the premise test pins both arms.
			//
			// So the two producers' dispositions above survive the key change intact:
			// producer 1 makes the box's correlation and its rightmost leaf's the SAME
			// identifier (REPLACE), producer 2's $BOX suffix makes them different ones
			// (ADD beside). What the key change removes is the case-folding that used
			// to sit under both — a buried `c` and a box `C` collided as one text key
			// while being two identities, which is the collision SameLeg exists to
			// refuse.
			//
			// Three earlier justifications for keeping a TEXT comparison here are
			// withdrawn, and the last of them was wrong in an instructive way. It claimed
			// the two identifiers are "legitimately different — box quantifier vs leaf",
			// and cited a red in the planner/seed cross-agreement fixture as proof the
			// site could not convert. Measured: over the real-FDB corpus this comparison
			// decides IDENTICALLY in both namespaces on all 1311 pairs, and the fixture's
			// red came from the fixture, which hand-minted the box correlation as
			// LOWERCASE "c" where production mints sourceAlias(box) = "C". The convention
			// makes the two identifiers EQUAL; it does not make them different — under
			// producer 1. Producer 2's $BOX suffix makes them differ by design; see above.
			if !SameLeg(leg.Alias, w.Alias) {
				if _, taken := windows[leg.Alias]; taken {
					continue
				}
			}
			if leg.Kind == LegKindNested {
				// A NESTED sub-leg occupies exactly ONE slot of the carrying run, and
				// a read into it must DESCEND. Its window therefore names that slot and
				// carries the leg's own row type.
				//
				// Typ is an EXTRACTION, never a slice — and this is not stylistic. The
				// keyed readers bound a leg-local ordinal against Typ before composing;
				// if Typ were a one-field wrapper record describing "the slot", every
				// leg-local ordinal >= 1 would be declined and every ordinal 0 would
				// resolve against the wrapper — a silent wrong-column read on exactly
				// the shape the bound check exists to catch. Typ must be the LEG's own
				// record type so the bound is the leg's real width.
				if leg.Start < 0 || leg.Start >= len(w.Typ.Fields) {
					return nil, nil, nil // malformed bounds on a nested leg — no layout at all
				}
				subType, isRT := w.Typ.Fields[leg.Start].FieldType.(*RecordType)
				if !isRT || subType == nil {
					// The carrying run says slot leg.Start holds a whole leg row and the
					// TYPE says otherwise. Two producers disagreeing about a slot's shape
					// is not something to approximate around.
					return nil, nil, nil
				}
				// A sub-leg whose OWN row carries leg boundaries puts those two steps
				// from the merged row, and this authority has no two-step window to
				// express them with — the same refusal positionalMergeSpans makes on
				// the executor side, stated here so the two walks decline together.
				//
				// IT MUST BE TESTED AT THE INSERTION, NOT ONLY AT THE HEAD OF THE
				// LOOP, and that is a correctness requirement rather than tidiness.
				// This loop RANGES the same map it inserts into, and Go's spec leaves
				// it unspecified whether an entry added during iteration is visited.
				// The head-of-loop guard above would therefore decline this seed on
				// the runs where the range happens to reach the newly inserted
				// sub-window and accept it on the runs where it does not — making
				// ACCEPT/DECLINE depend on map iteration order, which is randomised.
				// A planner whose layout decision is nondeterministic produces
				// different plans for the same query on different runs.
				if len(subType.Legs) > 0 {
					return nil, nil, nil
				}
				// The `taken` test above already ran and is not repeated here: a second
				// copy of the ADD-beside/REPLACE rule is a second place for it to drift.
				windows[leg.Alias] = OrdinalSeedLegWindow{
					Kind:   LegKindNested,
					Offset: w.Offset + leg.Start,
					Typ:    subType,
					Alias:  leg.Alias,
				}
				names[leg.Alias] = leg.Name
				continue
			}
			end := len(w.Typ.Fields)
			if li+1 < len(w.Typ.Legs) {
				end = w.Typ.Legs[li+1].Start
			}
			if leg.Start < 0 || end > len(w.Typ.Fields) || leg.Start >= end {
				continue // malformed bounds — leave the buried read loud
			}
			sub := make([]Field, end-leg.Start)
			for k := range sub {
				f := w.Typ.Fields[leg.Start+k]
				sub[k] = Field{Name: f.Name, FieldType: f.FieldType, Ordinal: k}
			}
			// The rightmost-leaf case (SameLeg above) REPLACES the box-run window with
			// the LEAF's sub-window: the box IS its rightmost leaf
			// (sourceBinding), so an alias-qualified read must window the leaf — the
			// run-wide window would resolve the name across the whole concat, where an
			// earlier buried leg carrying the same name makes it ambiguous — so the
			// read declines rather than reaching the leaf it named. Also what keeps the merged type's
			// Legs in lockstep with the executor twin, which emits EVERY sub of a box
			// run (ordinalJoinSpansOf).
			// Filed under the BURIED leg's own IDENTITY — the whole point of a
			// sub-window is that a read correlated to the buried source addresses that
			// source, not the box run carrying it, and the readers now arrive holding
			// that correlation.
			windows[leg.Alias] = OrdinalSeedLegWindow{
				// A buried sub-leg of a clustered BOX run is a flat SLICE of that
				// run — the sub slice built just above IS its columns.
				Kind:   LegKindFlatRun,
				Offset: w.Offset + leg.Start,
				Typ:    &RecordType{Nullable: w.Typ.Nullable, Fields: sub},
				Alias:  leg.Alias,
			}
			// The buried leg's own display binding, carried for the merged type's leg
			// table. It is the leg's stated Name rather than a fold of its identity:
			// the dotted channel's counterparty is the text a producer wrote, and
			// re-deriving it from the identity would quietly re-spell it.
			names[leg.Alias] = leg.Name
		}
	}
	// The merged type carries every window's boundary for the dotted-read bridge —
	// but a clustered box RUN emits its SUBS only: the run's name is its rightmost
	// leaf's (sourceBinding), and a run-level entry would shadow that leaf with the
	// whole concat (the RIGHT-box collision; see the executor twin). Sorted by Start
	// for deterministic order.
	mergedLegs := make([]RecordTypeLeg, 0, len(windows))
	for alias, w := range windows {
		if w.Kind == LegKindFlatRun && len(w.Typ.Legs) > 0 {
			continue // box run: its subs are their own windows above
		}
		// The IDENTITY is the map key and the window's Alias — one fact, stated once.
		// The NAME comes from the producer-local side table, and the two entries in
		// that table have DIFFERENT provenance, which is worth stating precisely
		// because an earlier version of this comment claimed they never derive from
		// each other and that is false for one of them:
		//
		//   - a TOP-LEVEL leg's name IS derived from its identity —
		//     `names[alias] = strings.ToUpper(alias.Name())`, right where the window
		//     is filed. For this producer the text-vs-identity census can report
		//     nothing but agreement, because agreement is what the assignment
		//     constructs. Its slice of that census is VACUOUS and must not be read as
		//     evidence;
		//   - a BURIED leg's name is the leg's own stated Name, carried from a leg
		//     table this function did not build. That one is an independent
		//     spelling, and it is the only half the census can actually test.
		//
		// The reason for the asymmetry is the one type.go states at its own text
		// boundary: the buried leg's counterparty is the text a producer wrote, so
		// re-deriving it from the identity would quietly re-spell it, while a
		// top-level leg has no such counterparty and the fold is the only name there
		// is. Neither case launders anything — the derived one cannot diverge, and
		// the carried one is measured.
		//
		// WIDTH IS A SLOT COUNT, which is why it cannot simply be len(Typ.Fields).
		// A nested leg occupies exactly ONE slot however wide its row is, and every
		// consumer of this table computes Start+Width as a slot RANGE into the
		// carrying type's Fields — so the flat reading would put a nested leg's
		// range over its neighbours. The leg's COLUMN count is not lost: it is the
		// window's Typ, which the readers go through.
		width := len(w.Typ.Fields)
		if w.Kind == LegKindNested {
			width = 1
		}
		mergedLegs = append(mergedLegs, NewRecordTypeLeg(w.Kind, w.Alias, names[alias], w.Offset, width))
	}
	sort.Slice(mergedLegs, func(i, j int) bool {
		if mergedLegs[i].Start != mergedLegs[j].Start {
			return mergedLegs[i].Start < mergedLegs[j].Start
		}
		return mergedLegs[i].Name < mergedLegs[j].Name
	})
	return windows, &RecordType{Fields: mergedFields, Legs: mergedLegs}, runs
}

// isMixedSeedElement reports whether an RC field is the MIXED-seed scalar element
// (at ANY position — the walk is element-anywhere). LOAD-BEARING guard.
func isMixedSeedElement(f RecordConstructorField) bool {
	qov, isQOV := f.Value.(*quantifiedObjectValue)
	return isQOV && IsMixedSeedElementType(qov.Type())
}

// IsMixedSeedElementType decides whether a bare QuantifiedObjectValue carrying
// this type is the MIXED seed's whole-object SCALAR element — Java's
// `isPrimitive()` branch, the element the seed cannot ofOrdinal-bake and so gives
// its own synthesized 1-field window.
//
// It is one predicate rather than two identical ones because the planner's window
// derivation (OrdinalSeedLegWindows) and the executor's span derivation
// (unnestMixedSeedSpans / ordinalJoinSpans) must agree on it BIT FOR BIT: they
// walk independently, and a disagreement about which field is the element is a
// wrong-offset read of every field after it. Two copies of a rule agree until one
// of them is edited.
//
// The test is "not a RECORD", and it is a PROXY for the question actually being
// asked — is this field one slot, or is it a leg occupying width-many? — so it is
// worth being exact about why the proxy holds, because it did not always.
//
// It holds because a LEG's quantifier object now states its row. While the flowed
// object value was minted untyped, an untyped leg was not a record either, so a
// leg flowing a whole multi-column row was admitted as a one-column element and a
// 2-slot record constructor of bare untyped quantifier objects was accepted
// outright, at widths nobody had checked. Typing the flowed value closed that: a
// leg reads as a RecordType and is rejected here, measured as flowed 848 of 848
// leg derivations over the real-FDB corpus with underivable at zero. The
// bakeability census asserts that zero, so the fact this proxy rests on is
// enforced rather than assumed.
//
// What it does NOT do is demand a STATED type, and that is deliberate rather than
// an oversight. An unnest element over an array of STRUCTS is a genuine
// whole-object element — one slot, the whole struct, exactly the case this arm
// exists for — and its type is UNKNOWN because Go does not infer array element
// types that far. Requiring a stated type declines it and
// `SELECT "X" FROM TS, TS."ITEMS" AS "X"` stops resolving. So an unstated type
// stays admitted, and the leg side is what carries the discrimination.
func IsMixedSeedElementType(t Type) bool {
	if t == nil {
		return false
	}
	_, isRecord := t.(*RecordType)
	return !isRecord
}
