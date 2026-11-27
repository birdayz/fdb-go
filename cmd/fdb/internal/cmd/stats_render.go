package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

// statsCollectResult is the typed JSON shape of `stats collect -o json`.
type statsCollectResult struct {
	Schema         string           `json:"schema"`
	RecordsScanned int64            `json:"records_scanned"`
	DurationMS     int64            `json:"duration_ms"`
	Collected      map[string]int64 `json:"collected"`
	// Skipped maps a record type to WHY it has no statistic. Distinct from a
	// zero count: "no rows" and "not counted" are different facts and only one
	// of them describes an empty table.
	Skipped map[string]string `json:"skipped"`
}

func renderCollectReport(
	cmd *cobra.Command,
	outputFmt, schema string,
	report *recordlayer.CollectionReport,
	elapsed time.Duration,
) error {
	// ONE decoding decision for the whole report, over the union of every stored
	// name it prints. Deciding per-map lets a colliding pair straddle collected
	// and skipped: neither map sees a collision and the output still shows one
	// name for two tables. See safeDecoderOver.
	skippedRaw := report.Skipped
	if skippedRaw == nil {
		skippedRaw = map[string]string{}
	}
	decode := safeDecoderOver(append(storedKeysOf(report.Collected), storedKeysOf(skippedRaw)...), nil)
	collected := make(map[string]int64, len(report.Collected))
	for name, st := range report.Collected {
		collected[decode(name)] = st.Count
	}
	if outputFmt == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(statsCollectResult{
			Schema:         schema,
			RecordsScanned: report.RecordsScanned,
			DurationMS:     elapsed.Milliseconds(),
			Collected:      collected,
			Skipped:        keyedBy(decode, skippedRaw),
		})
	}
	out := cmd.OutOrStdout()
	// An ABORTED run stored nothing, and this renderer is reached on that path
	// too -- RunE prints the report before returning its non-zero error. An
	// unconditional "collected" headline then announces success for a run whose
	// own later line says the types were NOT collected, which is the reading an
	// operator takes away from a scrollback.
	if len(collected) == 0 && len(report.Skipped) > 0 {
		fmt.Fprintf(out, "collection ABORTED for %s — nothing was stored\n", schema)
	} else {
		fmt.Fprintf(out, "collected statistics for %s\n", schema)
	}
	fmt.Fprintf(out, "  records scanned: %d in %s\n", report.RecordsScanned, elapsed.Round(time.Millisecond))
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tROWS")
	for _, name := range sortedKeys(collected) {
		fmt.Fprintf(tw, "%s\t%d\n", name, collected[name])
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(report.Skipped) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "not collected (the planner refuses statistics until every type has one):")
		skippedByUser := keyedBy(decode, report.Skipped)
		for _, name := range sortedKeys(skippedByUser) {
			fmt.Fprintf(out, "  %s: %s\n", name, skippedByUser[name])
		}
	}
	return nil
}

// statsShowResult is the typed JSON shape of `stats show -o json`.
type statsShowResult struct {
	Schema string `json:"schema"`
	// Usable is the planner's own verdict, from the planner's own code.
	Usable  bool   `json:"usable"`
	Refusal string `json:"refusal,omitempty"`
	// Found is TRI-STATE, so it is a pointer: true when statistics are known to
	// be stored, false when the store is known to hold none, and NULL when
	// existence was not established.
	//
	// A plain bool collapsed the third case into the second and contradicted the
	// field's own contract: a TORN set is known to hold entries, while a failed
	// read and the synthetic preflight establish nothing -- the first could not
	// read, the second deliberately did not look. Serialising all three as
	// `found: false` tells a machine consumer the store is empty in two cases
	// where that is not known and one where it is known to be wrong.
	Found *bool `json:"found"`
	// PerType is present whenever statistics were found, usable or not — a
	// stale count is still the number an operator wants to look at.
	PerType              map[string]int64 `json:"per_type,omitempty"`
	CollectedAtVersion   int64            `json:"collected_at_version,omitempty"`
	CollectedAtUnixNanos int64            `json:"collected_at_unix_nanos,omitempty"`
	CurrentVersion       int64            `json:"current_version,omitempty"`
	AgeVersions          int64            `json:"age_versions,omitempty"`
	MaxAgeVersions       int64            `json:"max_age_versions"`
	SyntheticTypes       []string         `json:"synthetic_types,omitempty"`
	AmbiguousTypes       []string         `json:"ambiguous_types,omitempty"`
	// ReadRefusal names WHICH way a stored set is torn, when Refusal is
	// StatisticsTorn. Present on BOTH render paths deliberately: the text path
	// named it and JSON did not, so an automated caller could see THAT the set
	// was unusable and never WHY -- and a diagnosis on one render path is half a
	// diagnosis, which is the same reason the synthetic and ambiguous names are
	// on both.
	ReadRefusal string `json:"read_refusal,omitempty"`
	// ReadError is the underlying failure when Refusal is StatisticsReadFailed,
	// where existence is UNKNOWN rather than absent. Same reasoning as above:
	// it was reaching only the text path.
	ReadError    string   `json:"read_error,omitempty"`
	MissingTypes []string `json:"missing_types,omitempty"`
	ExtraTypes   []string `json:"extra_types,omitempty"`
}

func renderStatsStatus(
	cmd *cobra.Command,
	outputFmt, schema string,
	st embedded.StatisticsStatus,
) error {
	// Keyed by the SQL identifier from here down, so BOTH renderings get it from
	// one decode. Decoding per consumer is what left the text path leaking the
	// storage name while JSON was correct -- the same one-of-two-consumers shape
	// that put ReadRefusal on the text path only.
	// ONE decoding decision for the whole status, over every stored name it
	// prints: the per-type keys AND the missing/extra lists. Deciding per-list
	// lets a colliding pair straddle two of them, so each is individually correct
	// and the output still shows one name for two tables. See safeDecoderOver.
	// ExtraTypes is a SUBSET of PerType's keys, so feeding it would add only
	// duplicates. That is now genuinely harmless -- SafeDecoderOver keys `seen`
	// by the decoded name and VALUES it by the stored one, so a repeat of the
	// same name is not a collision. It was not always: with a set, re-adding
	// ExtraTypes forced stored names for every status carrying an orphan type.
	// SyntheticTypes is different again: it is printed VERBATIM alongside these,
	// so a decoded name equalling one of them is a real collision, which is why
	// it goes in as `verbatim` rather than being left out.
	decode := safeDecoderOver(
		append(storedKeysOf(st.Stats.PerType), st.MissingTypes...), st.SyntheticTypes)
	if len(st.AmbiguousTypes) > 0 {
		// AmbiguousTypes are decoded at their SOURCE (AmbiguousDeclaredNames
		// returns user identifiers), so they are not in the union above -- and a
		// decoded name from one of the lists that happens to equal one of them
		// would print the same label for two different things. Their presence
		// says the schema is ambiguous, so nothing else here decodes either.
		//
		// Local on purpose: "unreachable because the reader refuses first" is the
		// non-local argument that produced the straddling bug two commits ago.
		decode = func(s string) string { return s }
	}
	perType := make(map[string]int64, len(st.Stats.PerType))
	for name, s := range st.Stats.PerType {
		perType[decode(name)] = s.Count
	}
	if outputFmt == "json" {
		return renderStatsStatusJSON(cmd.OutOrStdout(), schema, st, decode, perType)
	}

	out := cmd.OutOrStdout()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Schema:\t%s\n", schema)
	if st.Usable {
		fmt.Fprintf(tw, "Planner verdict:\tUSABLE\n")
	} else {
		fmt.Fprintf(tw, "Planner verdict:\tNOT USABLE — %s\n", st.Refusal)
	}
	if st.Found {
		fmt.Fprintf(tw, "Collected at:\tversion %d", st.Stats.CollectedAtVersion)
		if st.Stats.CollectedAtUnixNanos > 0 {
			fmt.Fprintf(tw, " (%s)", time.Unix(0, st.Stats.CollectedAtUnixNanos).UTC().Format(time.RFC3339))
		}
		fmt.Fprintln(tw)
		if st.CurrentVersion > 0 {
			fmt.Fprintf(tw, "Age:\t%s of %s allowed\n",
				renderVersionAge(st.AgeVersions), renderVersionAge(st.MaxAgeVersions))
		}
	}
	if len(st.SyntheticTypes) > 0 {
		// Naming them is the difference between a verdict and an instruction.
		fmt.Fprintf(tw, "Synthetic types:\t%s (unmodeled by this port; statistics are refused for the schema)\n",
			strings.Join(st.SyntheticTypes, ", ")) // VERBATIM
	}
	if len(st.AmbiguousTypes) > 0 {
		// Same reason as synthetic types: a verdict an operator cannot act on is
		// half a diagnosis. Both names are shown because either one of the two
		// tables can be renamed or quoted differently to break the collision.
		fmt.Fprintf(tw, "Ambiguous types:\t%s (one name is the other's escaped form, so a "+
			"lookup cannot say which table is meant; statistics are refused for the schema)\n",
			strings.Join(st.AmbiguousTypes, ", "))
	}
	if len(st.MissingTypes) > 0 {
		fmt.Fprintf(tw, "Missing types:\t%s\n", strings.Join(sortedBy(decode, st.MissingTypes), ", "))
	}
	if len(st.ExtraTypes) > 0 {
		// Orphans never refuse: the planner asks by declared type name and
		// simply never names a dropped table. Reported so an operator can tell
		// a stale entry from a schema they misremembered.
		fmt.Fprintf(tw, "Orphan types:\t%s (dropped from the schema; harmless)\n",
			strings.Join(sortedBy(decode, st.ExtraTypes), ", "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if !st.Found {
		renderStatsStatusNotFound(out, st)
		return nil
	}
	fmt.Fprintln(out)
	rows := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(rows, "TYPE\tROWS")
	for _, name := range sortedKeys(perType) {
		fmt.Fprintf(rows, "%s\t%d\n", name, perType[name])
	}
	if err := rows.Flush(); err != nil {
		return err
	}
	if st.Usable {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "the planner uses these only when the connection opts in:")
		fmt.Fprintln(out, "  ?planner_statistics=true")
	}
	return nil
}

func renderStatsStatusJSON(
	out io.Writer,
	schema string,
	st embedded.StatisticsStatus,
	decode func(string) string,
	perType map[string]int64,
) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(statsShowResult{
		Schema:               schema,
		Usable:               st.Usable,
		Refusal:              string(st.Refusal),
		Found:                foundTriState(st),
		PerType:              perType,
		CollectedAtVersion:   st.Stats.CollectedAtVersion,
		CollectedAtUnixNanos: st.Stats.CollectedAtUnixNanos,
		CurrentVersion:       st.CurrentVersion,
		AgeVersions:          st.AgeVersions,
		MaxAgeVersions:       st.MaxAgeVersions,
		SyntheticTypes:       st.SyntheticTypes, // VERBATIM -- see userName's note
		AmbiguousTypes:       st.AmbiguousTypes,
		ReadRefusal:          string(st.ReadRefusal),
		ReadError:            errString(st.ReadErr),
		MissingTypes:         sortedBy(decode, st.MissingTypes),
		ExtraTypes:           sortedBy(decode, st.ExtraTypes),
	})
}

func renderStatsStatusNotFound(out io.Writer, st embedded.StatisticsStatus) {
	fmt.Fprintln(out)
	// The hint is gated on the REFUSAL, not on Found. A synthetic-type
	// refusal also leaves Found false — it returns before any read — but
	// collection rejects those schemas too, so recommending it would send an
	// operator to run a command that cannot succeed, from the one verdict
	// that is permanent.
	switch st.Refusal {
	case embedded.StatisticsNotCollected:
		fmt.Fprintln(out, "run `fdb rl stats collect` to gather them")
	case embedded.StatisticsAmbiguousNames:
		// Same shape as the synthetic arm: decided from metadata WITHOUT a read,
		// so claiming absence would assert a fact this path went out of its way
		// not to look up. What is certain is that collection cannot help.
		fmt.Fprintf(out, "this schema declares record types whose names collide across "+
			"the SQL and storage namespaces (%s), so a lookup cannot say which "+
			"table is meant; statistics can never be used for it and collection "+
			"is refused too\n", strings.Join(st.AmbiguousTypes, " and "))
		fmt.Fprintln(out, "whether any are still stored was not read")
	case embedded.StatisticsSyntheticTypes:
		// This verdict is reached WITHOUT READING -- the synthetic gate decides
		// before any I/O, deliberately, since the answer cannot depend on it. So
		// existence is unknown, and entries can genuinely still be there: a
		// schema collected before its metadata was rebound to a version
		// declaring a joined type leaves its old entries in place. Saying
		// "nothing is stored" asserts a fact this path went out of its way not to
		// look up. What IS certain is that collection cannot help.
		fmt.Fprintln(out, "this schema declares record types this port does not model, so "+
			"statistics can never be used for it; collection is refused too")
		fmt.Fprintln(out, "whether any are still stored was not read")
	case embedded.StatisticsTorn:
		// Stored, and unusable — the opposite of absent, and the opposite of
		// permanent. The default arm below says "nothing is stored, and
		// collection will not help", which is wrong TWICE for this refusal:
		// something is stored, and a collect is exactly the repair, because it
		// ClearRanges the range and rewrites header and entries in one
		// transaction.
		fmt.Fprintf(out, "statistics ARE stored but cannot be vouched for: %s\n", st.ReadRefusal)
		fmt.Fprintln(out, "run `fdb rl stats collect` to replace them")
	case embedded.StatisticsReadFailed:
		// Found is false here because existence is UNKNOWN, not because the
		// store is empty: the read itself failed. Saying "nothing is stored"
		// turns a transient, permission or cluster fault into a confident
		// statement of absence -- and an operator who believes it collects
		// again, which does not diagnose the fault either.
		fmt.Fprintf(out, "could not read them, so whether any are stored is UNKNOWN: %s\n",
			st.Refusal)
		if st.ReadErr != nil {
			fmt.Fprintf(out, "  %v\n", st.ReadErr)
		}
	default:
		fmt.Fprintf(out, "nothing is stored, and collection will not help: %s\n", st.Refusal)
	}
}

// renderVersionAge turns an FDB version delta into something an operator reads
// without doing arithmetic. FDB advances ~1,000,000 versions per second, so the
// conversion is exact enough to be useful and approximate enough to be marked
// as such.
func renderVersionAge(versions int64) string {
	if versions < 0 {
		return fmt.Sprintf("%d versions (ahead of the cluster)", versions)
	}
	d := time.Duration(versions) * time.Microsecond
	return fmt.Sprintf("%d versions (~%s)", versions, d.Round(time.Second))
}

// sortedKeys returns a map's keys in sorted order, so text and JSON output are
// byte-stable across runs — a CLI whose output reorders cannot be diffed.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// describeSkippedTypes renders why a collection abandoned its run, sorted so
// repeated invocations are diffable.
func describeSkippedTypes(decode func(string) string, skipped map[string]string) string {
	// DECODED, like every other operator-facing surface. This one is the error
	// banner of the abort path, printed directly beneath a report body that was
	// already decoded -- so leaving it raw made ONE run of `fdb rl stats collect`
	// print MY$TABLE in the not-collected block and MY__1TABLE in the error line
	// under it. Two spellings of one table, three lines apart.
	byUser := keyedBy(decode, skipped)
	parts := make([]string, 0, len(byUser))
	for _, name := range sortedKeys(byUser) {
		parts = append(parts, name+": "+byUser[name])
	}
	return strings.Join(parts, "; ")
}

// errString renders an error for a JSON field, empty when there is none. Kept
// separate so the omitempty tag means "no error" rather than "an error whose
// text happened to be empty".
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// foundTriState renders existence as known-true, known-false or unknown.
//
// The gate's Found is a plain bool and cannot carry the third state, so the
// mapping is made here from the REFUSAL, which is what actually distinguishes
// them: a torn set is known to hold entries; a failed read and the synthetic
// preflight establish nothing, the first because it could not read and the
// second because it deliberately did not look.
func foundTriState(st embedded.StatisticsStatus) *bool {
	yes, no := true, false
	switch st.Refusal {
	case embedded.StatisticsTorn:
		return &yes // entries were read; only the header is unusable
	case embedded.StatisticsReadFailed,
		embedded.StatisticsSyntheticTypes,
		embedded.StatisticsAmbiguousNames:
		// Existence NOT established. The first could not read; the other two are
		// metadata-only verdicts that short-circuit before the read on purpose,
		// so Found is false because nobody looked -- and entries from an earlier
		// metadata version may well still be there.
		//
		// Ambiguity joined this list the moment the read was skipped for it. That
		// is the coupling worth naming: making a verdict cheaper by not reading
		// turns its Found into an unknown, and any refusal that gains a
		// short-circuit has to be added here in the same change.
		return nil
	}
	if st.Found {
		return &yes
	}
	return &no
}

// userName converts a STORAGE record-type name to the SQL identifier an
// operator wrote, at the rendering boundary and nowhere earlier.
//
// Metadata and the collector both key by storage names — a table quoted as
// "MY$TABLE" is stored as MY__1TABLE — and the operator-facing surfaces in this
// file were copying those keys verbatim. That names a table the operator does
// not have, and `per_type` is a documented interface, so a script keying by
// table name silently misses rather than failing.
//
// NOT every name reaching this file is storage-keyed, and the difference is not
// visible by looking. StatisticsStatus.AmbiguousTypes is ALREADY decoded, at its
// source in RecordMetaData.AmbiguousDeclaredNames, so passing it through here
// would decode twice -- and ToUserIdentifier is NOT idempotent:
// MY__01TABLE -> MY__1TABLE -> MY$TABLE, pinned by
// TestToUserIdentifierIsNotIdempotent. A second decode does not fail, it renames
// the table. Check the field's documented namespace before wrapping it.
//
// Decoding HERE, not upstream, is deliberate: the collision the reader detects
// lives in storage space, and the map the planner is handed must stay keyed the
// way the planner asks. Only what a human or a script reads gets translated.
//
// DECODES ONLY WHEN THE RESULT PROVABLY MAPS BACK. This port's metadata does not
// only come from the SQL layer -- RecordMetaDataBuilder.SetRecords copies
// protobuf identifiers verbatim -- so a record type may legally be named
// __0Order without ever having been escaped from anything. Decoding that yields
// __Order, which re-encodes to __Order and NOT to __0Order, so the name shown to
// the operator would resolve to nothing: GetRecordType misses on the direct key
// and then skips its escape retry, because the escape is a no-op.
//
// The round trip IS the provenance test, and it needs no extra bookkeeping: if
// encode(decode(s)) == s then the decoded spelling addresses exactly the same
// type, which is the whole promise made to whoever copies a name out of the
// output. When it does not hold, the stored name is shown unchanged -- uglier,
// and correct.
//
// WHAT THIS DOES NOT PROVE. The round trip shows encode(decode(s)) == s, which
// makes GetRecordType's SECOND step land on s. It says nothing about the FIRST
// step: if some other record type is stored under the decoded spelling itself,
// the direct-key hit answers first and returns that one instead. That is the
// non-injectivity hazard, it is a property of the lookup rather than of this
// decode, and AmbiguousDeclaredNames is what detects it -- see
// TestGetRecordTypeMisResolvesAnAmbiguousPair.
//
// So this guard is not the whole story, and deliberately so: it is applied
// unconditionally, because a name that resolves to NOTHING is always wrong and
// needs no context to rule out. The ambiguous-pair case needs the declared set,
// so it lives in userNamesFor/userNameFor -- which every renderer holding an md
// now routes through, not just the completer. An earlier round gated only the
// completer on the argument that a read-only renderer showing an ambiguous name
// is merely misleading; that was wrong twice over, because `record scan`'s
// record_type is documented as feeding --type, and because a listing an
// operator copies from is not read-only in any useful sense.
func userName(storage string) string {
	// Delegated, not copied. A byte-identical second implementation is a second
	// chance to diverge, and one output already mixes both paths -- `decode`
	// from the shared helper, userNames/userNameFor from here.
	return recordlayer.DecodeOnceIfReversible(storage)
}

// userNames is userName over a slice, re-sorted BY THE DECODED NAME.
//
// The sort has to happen after decoding, not before, because the two orders
// differ: storage-sorted [A__0B, A__1B] prints as [A__B, A$B], while a reader
// scanning the output expects [A$B, A__B]. The gate sorts these lists in storage
// space -- correctly, since that is the namespace it holds them in -- so the
// renderer is where the order has to be restated in the namespace it prints.
//
// DECODING GOES THROUGH THESE HELPERS AND NOWHERE ELSE, which is the point:
// a list of call SITES has been wrong three times here -- written as FOUR,
// repaired to THREE by subtracting rather than re-sweeping, and still missing
// the sql.go sites and meta_diff's sortSection. A set of functions is
// small enough to read; a set of call sites is open and rots. It is NOT closed
// by the compiler: the policy is exported from recordlayer, so a new caller can
// reach past these.
//
//	userName          one name, round-trip guarded, no declared-set context
//	userNames         a slice, decoded then RE-SORTED in the printed namespace
//	userNamesFor      userNames plus the ambiguity gate, for callers holding md
//	userNameFor       userNamesFor's single-name form
//	userFieldNames    key-expression fields: decoded, ORDER PRESERVED, no gate
//	userFieldName     userFieldNames' single-name form
//	safeDecoderOver   ONE decision for a whole output, from every name it prints
//	keyedBy           re-keys a map through one such decision
//	sortedBy          decodes a list through one such decision, sorted by output
//
// The sort has to be restated after decoding because the two orders differ, and
// order has to be LEFT ALONE for key expressions because position is semantic
// there. fleet's describeSkipped is the one decoder outside this file; it sorts
// after decoding for the same reason userNames does.
//
// SyntheticRecordTypesNotModeledError.Error() decodes NOTHING and must not:
// Java stores a synthetic type's name verbatim, so decoding invents a
// declaration. It also must not re-sort -- a test asserts it preserves input
// order -- because with no decode there is only one namespace.
func userNames(storage []string) []string {
	if storage == nil {
		return nil
	}
	out := make([]string, len(storage))
	for i, s := range storage {
		out[i] = userName(s)
	}
	sort.Strings(out)
	return out
}

// userNamesFor is userNames with the ambiguity gate applied, for callers that
// hold the metadata.
//
// userName takes a STRING, so it cannot see whether some other record type is
// declared under the decoded spelling — and that is the case where a decoded
// name resolves to the WRONG type rather than to none. Anything holding an
// md should come through here instead.
//
// Under a collision the STORED names are returned, sorted in storage space.
// They are deliberately the less pleasant answer: they are the only spellings
// that resolve at GetRecordType's first step, so they are the only ones that
// mean what they say. A schema in this state wants renaming, not a prettier
// listing — which is the same call the statistics reader makes when it refuses
// outright on the identical condition.
func userNamesFor(md *recordlayer.RecordMetaData, names []string) []string {
	if md != nil {
		if _, ambiguous := md.AmbiguousDeclaredNames(); ambiguous {
			out := append([]string(nil), names...)
			sort.Strings(out)
			return out
		}
	}
	return userNames(names)
}

// userNameFor is userNamesFor for a single name.
func userNameFor(md *recordlayer.RecordMetaData, name string) string {
	if md != nil {
		if _, ambiguous := md.AmbiguousDeclaredNames(); ambiguous {
			return name
		}
	}
	return userName(name)
}

// userFieldNames decodes a key expression's field names for display, PRESERVING
// ORDER.
//
// A column name is a protobuf FIELD name and is escaped exactly like a record
// type name, so `fdb sql`'s \d printing COL$X while `fdb rl meta types describe`
// printed COL__1X for that same column was one defect wearing two spellings.
//
// Two deliberate differences from userNamesFor. Order is never touched: these
// are key expressions, where position is semantic — sorting a primary key would
// misreport the key. And the ambiguity gate does not apply: the collision it
// guards is a property of the declared RECORD TYPE set, which is what
// AmbiguousDeclaredNames inspects, and no destructive command resolves a target
// by column name.
func userFieldNames(fields []string) []string {
	if fields == nil {
		return nil
	}
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = userName(f)
	}
	return out
}

// userFieldName is userFieldNames for a single column name.
//
// Separate from userName so the CALL SITE says which namespace it is in. A
// column takes no ambiguity gate (that collision is a property of the declared
// record-type set) while a record type does, and a bare userName at a site
// holding an md is exactly the bug that let `record scan` print a
// record_type resolving to the wrong table.
func userFieldName(field string) string { return userName(field) }

// safeDecoderOver is recordlayer.SafeDecoderOver, kept as a local name because
// every renderer in this file calls it.
//
// `verbatim` carries names the output prints UNCHANGED -- synthetic record
// types, which Java stores exactly as the caller passed them. They take part in
// the decision without being subject to it: a decoded name that equals one of
// them is the same one-label-two-things hazard as any other collision.
func safeDecoderOver(decoded, verbatim []string) func(string) string {
	return recordlayer.SafeDecoderOver(decoded, verbatim)
}

// storedKeysOf collects a map's keys, for feeding safeDecoderOver.
func storedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// keyedBy re-keys a map through one decoding decision.
func keyedBy[V any](decode func(string) string, m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[decode(k)] = v
	}
	return out
}

// sortedBy decodes a list through one decision and sorts by what it prints.
func sortedBy(decode func(string) string, stored []string) []string {
	if stored == nil {
		return nil
	}
	out := make([]string, len(stored))
	for i, s := range stored {
		out[i] = decode(s)
	}
	sort.Strings(out)
	return out
}
