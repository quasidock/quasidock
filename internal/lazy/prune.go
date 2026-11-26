package lazy

import (
	"fmt"
	"os"

	"github.com/quasidock/quasidock/internal/cloudfiles"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

// PruneLayer removes only this layer's unused deployment and streaming state.
// The caller holds the exclusive store lock and has checked image, container,
// and build-cache roots. Dry runs neither stop helpers nor hydrate placeholders.
// The byte count includes only successfully removed trees (or estimates in dry runs).
func PruneLayer(st *store.Store, digest string, dry bool) (uint64, error) {
	hex := oci.DigestHex(digest)
	if hex == "" {
		return 0, fmt.Errorf("invalid layer digest %q", digest)
	}
	if st.DeploymentInUse(digest) {
		return 0, nil
	}
	deploy, err := st.DeployPath(digest)
	if err != nil {
		return 0, err
	}
	dir := DirPath(st, hex)
	if dry {
		return mounts.TreeSize(deploy) + mounts.TreeSize(dir), nil
	}
	stopHelper(st, hex)
	if HelperAlive(st, hex) {
		return 0, fmt.Errorf("lazy helper for %s did not stop", digest)
	}
	if st.DeploymentInUse(digest) {
		return 0, nil
	}
	if _, err := os.Stat(deploy); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	if cloudfiles.IsSyncRoot(deploy) {
		if err := cloudfiles.Unregister(deploy); err != nil {
			return 0, err
		}
	}
	deployBytes := mounts.TreeSize(deploy)
	deleted, err := st.DeleteDeployment(digest)
	if err != nil {
		return 0, err
	}
	if deleted == store.DeletionInUse {
		if st.DeploymentInUse(digest) {
			return 0, nil
		}
		return 0, fmt.Errorf("could not delete deployment %s", digest)
	}
	lazyBytes := mounts.TreeSize(dir)
	if err := store.DeleteTree(dir); err != nil {
		return deployBytes, err
	}
	// Remove only this layer's idle helper/start/pull locks, never helper binaries
	// or another layer's state. Lock-file sizes are not part of reclaimed data.
	for _, kind := range []string{"helper", "start", "pull"} {
		path := LockPath(st, hex, kind)
		if !store.IsLocked(path) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return deployBytes + lazyBytes, err
			}
		}
	}
	return deployBytes + lazyBytes, nil
}
