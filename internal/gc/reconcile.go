package gc

import (
	"context"
	"fmt"
	"time"

	"github.com/mantle-sh/mantle/internal/oci"
)

// ReconcileReport compares the blob store against the catalog (§12.2 phase 6).
type ReconcileReport struct {
	// OrphanBytes are files present in storage with no blobs row. They cost
	// money and nothing else — a cost alarm.
	OrphanBytes []OrphanBlob `json:"orphan_bytes"`
	// DanglingRows are blobs rows whose bytes are missing. Every one of these
	// is an image that will fail to pull — a correctness alarm.
	DanglingRows []DanglingRow `json:"dangling_rows"`

	BlobsInStorage  int    `json:"blobs_in_storage"`
	BlobsInCatalog  int    `json:"blobs_in_catalog"`
	OrphanByteCount int64  `json:"orphan_byte_count"`
	Duration        string `json:"duration"`
	// Truncated reports that the lists were capped, so a report showing a
	// handful of entries on a badly diverged instance is not mistaken for a
	// clean one.
	Truncated bool `json:"truncated"`
}

// OrphanBlob is a stored file with no catalog row.
type OrphanBlob struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size_bytes"`
}

// DanglingRow is a catalog row with no stored bytes.
type DanglingRow struct {
	Digest    string    `json:"digest"`
	Size      int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	State     string    `json:"state"`
}

// maxReconcileFindings bounds the report so that a badly diverged instance
// produces a usable summary rather than a gigabyte of JSON.
const maxReconcileFindings = 1000

// walker is the optional storage capability reconcile needs. A driver that
// cannot enumerate its contents can still be checked in one direction.
type walker interface {
	WalkBlobs(ctx context.Context, fn func(digest oci.Digest, size int64) error) error
}

// Reconcile compares storage against the catalog and reports discrepancies.
//
// It reports and never deletes, deliberately. The two findings have opposite
// causes and opposite remedies, and automatically "fixing" either one would be
// destructive: an orphan byte may be an upload that is committing right now,
// and a dangling row may be a storage backend that is temporarily unreachable.
// Deleting on that evidence is how a reconciliation job becomes an outage.
func (c *Collector) Reconcile(ctx context.Context) (*ReconcileReport, error) {
	start := time.Now()
	report := &ReconcileReport{
		OrphanBytes:  []OrphanBlob{},
		DanglingRows: []DanglingRow{},
	}

	// --- catalog side ---
	catalogued := map[string]bool{}
	rows, err := c.opts.Pool.Query(ctx, `SELECT digest, size_bytes, created_at, state FROM blobs`)
	if err != nil {
		return nil, fmt.Errorf("listing catalogued blobs: %w", err)
	}
	type row struct {
		digest    string
		size      int64
		createdAt time.Time
		state     string
	}
	var catalogRows []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.digest, &r.size, &r.createdAt, &r.state); err != nil {
			rows.Close()
			return nil, err
		}
		catalogued[r.digest] = true
		catalogRows = append(catalogRows, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	report.BlobsInCatalog = len(catalogRows)

	// --- storage side ---
	enumerable, ok := c.opts.Storage.(walker)
	if !ok {
		return nil, fmt.Errorf(
			"the %s storage driver cannot enumerate its contents, so reconcile is unavailable",
			c.opts.Storage.Name())
	}

	stored := map[string]bool{}
	err = enumerable.WalkBlobs(ctx, func(digest oci.Digest, size int64) error {
		report.BlobsInStorage++
		key := digest.String()
		stored[key] = true
		if catalogued[key] {
			return nil
		}
		report.OrphanByteCount += size
		if len(report.OrphanBytes) < maxReconcileFindings {
			report.OrphanBytes = append(report.OrphanBytes, OrphanBlob{Digest: key, Size: size})
		} else {
			report.Truncated = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking the blob store: %w", err)
	}

	// --- rows with no bytes ---
	for _, r := range catalogRows {
		if stored[r.digest] {
			continue
		}
		// A blob mid-delete legitimately has no bytes: the sweep removed them
		// and is about to remove the row.
		if r.state == "deleting" {
			continue
		}
		if len(report.DanglingRows) < maxReconcileFindings {
			report.DanglingRows = append(report.DanglingRows, DanglingRow{
				Digest: r.digest, Size: r.size, CreatedAt: r.createdAt, State: r.state,
			})
		} else {
			report.Truncated = true
		}
	}

	report.Duration = time.Since(start).String()

	if len(report.DanglingRows) > 0 {
		c.opts.Logger.Error("reconcile found catalog rows with no stored content",
			"count", len(report.DanglingRows),
			"impact", "these images will fail to pull",
			"remedy", "restore the blob store from backup, or delete the affected manifests")
	}
	if len(report.OrphanBytes) > 0 {
		c.opts.Logger.Warn("reconcile found stored content with no catalog row",
			"count", len(report.OrphanBytes),
			"bytes", report.OrphanByteCount,
			"impact", "wasted storage cost only; no image is affected")
	}
	return report, nil
}
