package images

import (
	"context"
	"strings"
	"testing"
)

func TestUpsertHostImageStoresATruncatedMultiByteError(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	hostID := seedHost(t, pool, "utf8-error")
	msg := truncateImageError(strings.Repeat("a", maxStoredImageError-1) + "é tail")
	if _, err := upsertHostImage(context.Background(), pool, hostID, imgID, imgVer, "failed", msg, nil); err != nil {
		t.Fatal(err)
	}
}
