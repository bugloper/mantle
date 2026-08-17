//go:build unix

package filesystem

import (
	"context"
	"fmt"
	"syscall"

	"github.com/mantle-sh/mantle/internal/storage/driver"
)

// Usage reports filesystem capacity, which preflight checks against min_free
// before the installer writes anything and which doctor re-checks against a
// live install (§14.1, §14.2).
//
// Available blocks rather than free blocks: the difference is the reserve that
// only root may use, and reporting space Mantle cannot actually write into is
// how a registry fills a disk while insisting it has headroom.
func (d *Driver) Usage(ctx context.Context) (driver.Usage, bool, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(d.root, &stat); err != nil {
		return driver.Usage{}, false, fmt.Errorf("checking free space on %s: %w", d.root, err)
	}
	blockSize := uint64(stat.Bsize)
	return driver.Usage{
		TotalBytes:     int64(stat.Blocks * blockSize),
		AvailableBytes: int64(stat.Bavail * blockSize),
	}, true, nil
}
