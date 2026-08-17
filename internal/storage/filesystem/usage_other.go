//go:build !unix

package filesystem

import (
	"context"

	"github.com/mantle-sh/mantle/internal/storage/driver"
)

// Usage is unavailable on this platform. Reporting "unknown" is correct here:
// preflight then skips the free-space check and says so, rather than inventing
// a number that would make the check meaningless.
func (d *Driver) Usage(context.Context) (driver.Usage, bool, error) {
	return driver.Usage{}, false, nil
}
