package recordlayer

import (
	"fmt"
	"strconv"
)

// SPFresh index options (RFC-094 §10). All structural options are immutable for
// an existing index — enforced by the metadata-evolution validator — because
// the lifecycle invariants (topology, posting sizes, closure replication) are
// derived from them. Runtime knobs (probe width w, k_c, ε, re-rank C, refresh
// interval, rebalancer pacing) are deliberately NOT index options: they are
// query/maintenance-time parameters and are never stored.
const (
	// IndexOptionSPFreshNumDimensions is the vector dimensionality. Required.
	IndexOptionSPFreshNumDimensions = "spfreshNumDimensions"
	// IndexOptionSPFreshMetric is the distance metric (EUCLIDEAN_METRIC,
	// COSINE_METRIC, DOT_PRODUCT_METRIC — same names the HNSW index accepts).
	IndexOptionSPFreshMetric = "spfreshMetric"
	// IndexOptionSPFreshLmax is the posting-list split threshold in entries.
	// Sized so one posting fits a single range-reply (REPLY_BYTE_LIMIT = 80 KB).
	IndexOptionSPFreshLmax = "spfreshLmax"
	// IndexOptionSPFreshLminRatio divides Lmax to produce the merge threshold.
	IndexOptionSPFreshLminRatio = "spfreshLminRatio"
	// IndexOptionSPFreshCellTarget is the fine-centroids-per-cell build target;
	// sized so one L2 cell load fits a single range-reply.
	IndexOptionSPFreshCellTarget = "spfreshCellTarget"
	// IndexOptionSPFreshCellMax is the coarse-split threshold in fine centroids.
	IndexOptionSPFreshCellMax = "spfreshCellMax"
	// IndexOptionSPFreshReplication is the closure replication cap r.
	IndexOptionSPFreshReplication = "spfreshReplication"
	// IndexOptionSPFreshAlpha is the RNG closure threshold: keep centroid c_i of
	// the r nearest iff dist(v,c_i) <= alpha * dist(v,c_1). Must be > 1.0 or
	// only the nearest centroid is ever admitted (effective r=1).
	IndexOptionSPFreshAlpha = "spfreshAlpha"
	// IndexOptionSPFreshKn is the NPA reassignment neighborhood (centroids).
	IndexOptionSPFreshKn = "spfreshKn"
	// IndexOptionSPFreshCooldownSec is the post-split merge cooldown.
	IndexOptionSPFreshCooldownSec = "spfreshCooldownSec"
	// IndexOptionSPFreshRaBitQNumExBits is the RaBitQ extended-bits parameter
	// for posting residual codes.
	IndexOptionSPFreshRaBitQNumExBits = "spfreshRaBitQNumExBits"
	// IndexOptionSPFreshSidecar enables the fp16 SIDECAR subspace (re-rank
	// source). Default true; disabling falls back to source-record reads.
	IndexOptionSPFreshSidecar = "spfreshSidecar"
)

// SPFresh tuning defaults (RFC-094 §3/§9; frozen after the 094.1 benchmark).
const (
	spfreshDefaultLmax        = 256
	spfreshDefaultLminRatio   = 8
	spfreshDefaultCellTarget  = 48
	spfreshDefaultCellMax     = 96
	spfreshDefaultReplication = 2
	spfreshDefaultAlpha       = 1.2
	spfreshDefaultKn          = 8
	spfreshDefaultCooldown    = 600 // seconds
	spfreshDefaultNumExBits   = 1
	// spfreshCSplitDeferLimit: consecutive coarse-split deferrals before
	// fine-split task issuance for the cell is paused (starvation guard, §6b).
	spfreshCSplitDeferLimit = 8
	// spfreshReplyByteBudget is the per-range-reply byte budget the layout is
	// sized against (FDB REPLY_BYTE_LIMIT, ClientKnobs.cpp:66).
	spfreshReplyByteBudget = 80000
)

// spfreshTxByteBudget bounds the single-tx split worst case (4×Lmax entries
// read + ~2× written) far below FDB's 10 MB transaction limit. Variable so
// the chunked-drain dispatch is testable at small scale.
var spfreshTxByteBudget = 4 << 20

// SPFreshConfig is the structural configuration of an SPFresh index. Every
// field here is immutable for an existing index (RFC-094 §10).
type SPFreshConfig struct {
	NumDimensions int
	Metric        VectorMetric
	Lmax          int     // posting split threshold (entries)
	LminRatio     int     // Lmin = Lmax / LminRatio (merge threshold)
	CellTarget    int     // fine centroids per cell, build target
	CellMax       int     // coarse-split threshold (fine centroids)
	Replication   int     // closure replication cap r
	Alpha         float64 // RNG closure threshold (> 1.0)
	Kn            int     // NPA reassignment neighborhood
	CooldownSec   int     // post-split merge cooldown
	NumExBits     int     // RaBitQ extended bits for posting residual codes
	Sidecar       bool    // fp16 sidecar for re-rank
}

// Lmin returns the merge threshold in entries.
func (c SPFreshConfig) Lmin() int { return c.Lmax / c.LminRatio }

// stagingScanBatch bounds the assignment scan's records-per-transaction by
// BYTES, not just rows: each staged record writes a STAGING and a SIDECAR
// fp16 vector (2 bytes/dim each) in the scan's own transaction, so a fixed
// 1000-row batch at 4096 dims would exceed FDB's 10 MB transaction limit
// (codex 094.2 r1 P2). Capped at spfreshScanBatchSize for small vectors.
func (c SPFreshConfig) stagingScanBatch() int {
	perRecord := 2*(2*c.NumDimensions) + 128 // staging + sidecar values, key overhead
	n := spfreshTxByteBudget / perRecord
	if n < 1 {
		n = 1
	}
	if n > spfreshScanBatchSize {
		n = spfreshScanBatchSize
	}
	return n
}

// postingEntryBytes is the worst-case size of one posting entry: the RaBitQ
// residual code plus key overhead (subspace prefix, fineID, nested pk).
func (c SPFreshConfig) postingEntryBytes() int {
	// RaBitQ code: header + (1+numExBits) bits/dim rounded up per plane.
	codeBytes := 32 + (c.NumDimensions*(1+c.NumExBits)+7)/8
	return codeBytes + 64 // generous key overhead allowance
}

// centroidRowBytes is the worst-case size of one CENTROIDS row: raw fp16
// vector plus state/epoch/children and key overhead.
func (c SPFreshConfig) centroidRowBytes() int {
	return 2*c.NumDimensions + 96
}

// DefaultSPFreshConfig returns the RFC-094 defaults for the given
// dimensionality.
func DefaultSPFreshConfig(numDimensions int) SPFreshConfig {
	return SPFreshConfig{
		NumDimensions: numDimensions,
		Metric:        VectorMetricEuclidean,
		Lmax:          spfreshDefaultLmax,
		LminRatio:     spfreshDefaultLminRatio,
		CellTarget:    spfreshDefaultCellTarget,
		CellMax:       spfreshDefaultCellMax,
		Replication:   spfreshDefaultReplication,
		Alpha:         spfreshDefaultAlpha,
		Kn:            spfreshDefaultKn,
		CooldownSec:   spfreshDefaultCooldown,
		NumExBits:     spfreshDefaultNumExBits,
		Sidecar:       true,
	}
}

// ValidateSPFreshConfig enforces the invariants the RFC-094 lifecycle and
// sizing arguments depend on. Called by the maintainer at construction; a
// violation is a config error, never a silently degraded index.
func ValidateSPFreshConfig(c SPFreshConfig) error {
	if c.NumDimensions < 1 || c.NumDimensions > 4096 {
		return fmt.Errorf("spfresh: numDimensions must be in [1, 4096], got %d", c.NumDimensions)
	}
	if c.Lmax < 16 || c.Lmax > 4096 {
		return fmt.Errorf("spfresh: lmax must be in [16, 4096], got %d", c.Lmax)
	}
	if c.LminRatio < 2 {
		return fmt.Errorf("spfresh: lminRatio must be >= 2 (split/merge hysteresis), got %d", c.LminRatio)
	}
	if c.CellTarget < 4 {
		return fmt.Errorf("spfresh: cellTarget must be >= 4, got %d", c.CellTarget)
	}
	if c.CellMax < 2*c.CellTarget {
		return fmt.Errorf("spfresh: cellMax (%d) must be >= 2*cellTarget (%d) for split hysteresis", c.CellMax, 2*c.CellTarget)
	}
	if c.Replication < 1 || c.Replication > 4 {
		return fmt.Errorf("spfresh: replication must be in [1, 4], got %d", c.Replication)
	}
	// alpha == 1.0 with the <= rule admits only the nearest centroid when all
	// distances are distinct — silently making r = 1 and invalidating the
	// closure sizing and recall math (RFC-094 §5; the rev-3 alpha bug).
	if c.Replication > 1 && c.Alpha <= 1.0 {
		return fmt.Errorf("spfresh: alpha must be > 1.0 when replication > 1 (got %g — closure would never admit a second centroid)", c.Alpha)
	}
	if c.Kn < 1 || c.Kn > 64 {
		return fmt.Errorf("spfresh: kn must be in [1, 64], got %d", c.Kn)
	}
	if c.CooldownSec < 0 {
		return fmt.Errorf("spfresh: cooldownSec must be >= 0, got %d", c.CooldownSec)
	}
	if c.NumExBits < 0 || c.NumExBits > 8 {
		return fmt.Errorf("spfresh: raBitQNumExBits must be in [0, 8], got %d", c.NumExBits)
	}
	// One posting = one range reply (RFC-094 §3): Lmax entries must fit the
	// reply byte budget, or the constant-round-trip query claim is false.
	if got := c.Lmax * c.postingEntryBytes(); got > spfreshReplyByteBudget {
		return fmt.Errorf("spfresh: lmax (%d) * entry bytes (%d) = %d exceeds the %d-byte range-reply budget — one posting must fit one reply",
			c.Lmax, c.postingEntryBytes(), got, spfreshReplyByteBudget)
	}
	// One L2 cell load = one range reply at target fill (RFC-094 §3).
	if got := c.CellTarget * c.centroidRowBytes(); got > spfreshReplyByteBudget {
		return fmt.Errorf("spfresh: cellTarget (%d) * centroid row bytes (%d) = %d exceeds the %d-byte range-reply budget — one L2 cell must fit one reply",
			c.CellTarget, c.centroidRowBytes(), got, spfreshReplyByteBudget)
	}
	// Splits are single-transaction by spec — chunking is forbidden (RFC-094
	// §6): the 4×Lmax inline-split worst case must fit the tx byte budget.
	if got := 4 * c.Lmax * c.postingEntryBytes() * 3; got > spfreshTxByteBudget {
		return fmt.Errorf("spfresh: worst-case split (4*lmax entries, read+rewrite) = %d bytes exceeds the %d-byte single-transaction budget",
			got, spfreshTxByteBudget)
	}
	return nil
}

// parseSPFreshConfig builds an SPFreshConfig from index options, applying
// RFC-094 defaults for absent options. Invalid values fall back to defaults
// (matching parseHNSWConfig's tolerance); ValidateSPFreshConfig is the
// hard gate and runs at maintainer construction.
func parseSPFreshConfig(index *Index) SPFreshConfig {
	config := DefaultSPFreshConfig(0)

	if v, ok := index.Options[IndexOptionSPFreshNumDimensions]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			config.NumDimensions = n
		}
	}
	if v, ok := index.Options[IndexOptionSPFreshMetric]; ok {
		switch v {
		case "EUCLIDEAN_METRIC":
			config.Metric = VectorMetricEuclidean
		case "COSINE_METRIC":
			config.Metric = VectorMetricCosine
		case "DOT_PRODUCT_METRIC":
			config.Metric = VectorMetricInnerProduct
		}
	}
	parseInt := func(key string, dst *int) {
		if v, ok := index.Options[key]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	parseInt(IndexOptionSPFreshLmax, &config.Lmax)
	parseInt(IndexOptionSPFreshLminRatio, &config.LminRatio)
	parseInt(IndexOptionSPFreshCellTarget, &config.CellTarget)
	parseInt(IndexOptionSPFreshCellMax, &config.CellMax)
	parseInt(IndexOptionSPFreshReplication, &config.Replication)
	parseInt(IndexOptionSPFreshKn, &config.Kn)
	parseInt(IndexOptionSPFreshCooldownSec, &config.CooldownSec)
	parseInt(IndexOptionSPFreshRaBitQNumExBits, &config.NumExBits)
	if v, ok := index.Options[IndexOptionSPFreshAlpha]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			config.Alpha = f
		}
	}
	if v, ok := index.Options[IndexOptionSPFreshSidecar]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			config.Sidecar = b
		}
	}
	return config
}

// spfreshCoarseSampleCap bounds the coarse-k-means training sample in
// BuildSPFreshIndex (reservoir sampling past the cap; K₀ still derives from
// the full record count). 250k keeps ≥ ~30 sample points per coarse centroid
// up to ~2.5M records — raise it (or add hierarchical sampling, §8) beyond
// that. Var, not const: scale tests tighten it.
var spfreshCoarseSampleCap = 250_000
