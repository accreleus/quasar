package images

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const defaultLazyBuildTimeout = 15 * time.Minute

// WithLazyBuildTimeout bounds the first-launch template build independently
// of the HTTP request and the post-start session watchdog.
func WithLazyBuildTimeout(d time.Duration) EnsureOption {
	return func(e *Ensurer) {
		if d > 0 {
			e.lazyBuildTimeout = d
		}
	}
}

// PrepareLazyTemplate is the coordinator's post-reservation preparation seam.
// Prebuilt, eager and unmanaged images keep their existing launch behavior.
// A lazy template waits for an authenticated current-connection ready report.
func (e *Ensurer) PrepareLazyTemplate(ctx context.Context, hostID, imageRef string) error {
	img, lazy, err := e.lazyTemplateForRef(ctx, imageRef)
	if err != nil || !lazy {
		return err
	}
	if img.ContextSHA == "" {
		return errors.New("template build context is unresolved")
	}
	if e.disp == nil {
		return errors.New("image dispatcher unavailable")
	}
	epoch, connected := e.imageConnectionIdentity(hostID)
	if !connected {
		return errors.New("host agent is offline")
	}
	if ready, err := e.lazyReady(ctx, hostID, img, epoch); err != nil || ready {
		return err
	}
	// A joiner shares a build only for the same adopted version on the same
	// connection; anything else gets its own verdict.
	key := hostID + "|" + img.ImageID + "|" + img.Version + "|" + epoch
	if !e.addWork() {
		return errors.New("image preparation is shutting down")
	}
	e.lazyMu.Lock()
	op := e.lazyBuilds[key]
	if op == nil {
		op = &lazyBuild{done: make(chan struct{})}
		e.lazyBuilds[key] = op
		go func() {
			defer e.wg.Done()
			buildCtx, cancel := context.WithTimeout(e.ctx, e.lazyBuildTimeout)
			defer cancel()
			op.err = e.runLazyBuild(buildCtx, hostID, img, epoch)
			// Unpublish before signalling, so no caller can join a finished build.
			e.lazyMu.Lock()
			delete(e.lazyBuilds, key)
			e.lazyMu.Unlock()
			close(op.done)
		}()
	} else {
		e.wg.Done()
	}
	e.lazyMu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-op.done:
		if op.err != nil {
			return op.err
		}
		ready, err := e.lazyReady(ctx, hostID, img, epoch)
		if err != nil {
			return err
		}
		if !ready {
			return errors.New("template readiness was not verified")
		}
		return nil
	}
}

// lazyTemplateForRef resolves the adoption that owns a launch ref's local tag.
// It reports lazy=true only for a lazy template adoption. An eager adoption or
// an unmanaged ref is not its concern: placement already required a ready
// report for the former, and the agent refuses to pull a missing local tag.
// A lazy adoption owning the tag that is not a buildable template is catalog
// drift. It fails closed: assigning it would ask the agent for a tag nothing
// built.
func (e *Ensurer) lazyTemplateForRef(ctx context.Context, ref string) (installedImage, bool, error) {
	if ref == "" {
		return installedImage{}, false, nil
	}
	rows, err := e.pool.Query(ctx, `SELECT ii.lazy, ii.image_id, ii.version, ii.registry_ref, ic.kind,
		ii.local_tag, ii.context_repo, ii.context_sha, ii.dockerfile, ii.build_args
		FROM installed_images ii JOIN image_catalog ic ON ic.id = ii.image_id
		WHERE ii.local_tag = $1`, ref)
	if err != nil {
		return installedImage{}, false, fmt.Errorf("read lazy template adoption: %w", err)
	}
	defer rows.Close()
	var found *installedImage
	for rows.Next() {
		var lazy bool
		var img installedImage
		var buildArgs []byte
		if err := rows.Scan(&lazy, &img.ImageID, &img.Version, &img.RegistryRef, &img.Kind, &img.LocalTag,
			&img.ContextRepo, &img.ContextSHA, &img.Dockerfile, &buildArgs); err != nil {
			return installedImage{}, false, fmt.Errorf("read lazy template adoption: %w", err)
		}
		img.BuildArgs = buildArgs
		if !lazy {
			continue
		}
		if img.Kind != "template" || img.RegistryRef != "" || found != nil {
			return installedImage{}, false, fmt.Errorf("lazy adoption %q for %s is not a single buildable template (catalog kind %q)",
				img.ImageID, ref, img.Kind)
		}
		found = &img
	}
	if err := rows.Err(); err != nil {
		return installedImage{}, false, fmt.Errorf("read lazy template adoption: %w", err)
	}
	if found == nil {
		return installedImage{}, false, nil
	}
	return *found, true, nil
}

func (e *Ensurer) lazyReady(ctx context.Context, hostID string, img installedImage, epoch string) (bool, error) {
	current, connected := e.imageConnectionIdentity(hostID)
	if !connected || current != epoch {
		return false, errors.New("host agent reconnected during image preparation")
	}
	if next, adopted, err := e.lazyTemplateForRef(ctx, img.LocalTag); err != nil {
		return false, err
	} else if !adopted || !sameLazyIdentity(next, img) {
		return false, errors.New("template adoption changed during image preparation")
	}
	e.mu.Lock()
	inv := e.inventory[hostID]
	observedReady := inv != nil && inv.epoch == epoch && inv.ready[img.ImageID] == img.Version
	e.mu.Unlock()
	if !observedReady {
		return false, nil
	}
	var verified bool
	err := e.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM host_images hi JOIN installed_images ii ON ii.image_id=hi.image_id
		WHERE hi.host_id=$1::uuid AND hi.image_id=$2 AND hi.state='ready'
		AND hi.version=ii.version AND ii.version=$3 AND ii.local_tag=$4
		AND hi.updated_at>=ii.installed_at
		AND NOT EXISTS(SELECT 1 FROM host_image_operation_fences f
			WHERE f.host_id=hi.host_id AND f.image_id=hi.image_id AND f.state='removing'))`,
		hostID, img.ImageID, img.Version, img.LocalTag).Scan(&verified)
	return verified, err
}

func sameLazyIdentity(a, b installedImage) bool {
	return a.ImageID == b.ImageID && a.Version == b.Version && a.LocalTag == b.LocalTag &&
		a.ContextRepo == b.ContextRepo && a.ContextSHA == b.ContextSHA &&
		a.Dockerfile == b.Dockerfile && string(a.BuildArgs) == string(b.BuildArgs)
}

func (e *Ensurer) runLazyBuild(ctx context.Context, hostID string, img installedImage, epoch string) error {
	// A cache hit may have completed between the caller's first check and this
	// worker. It is still gated by current authenticated inventory and adoption.
	if ready, err := e.lazyReady(ctx, hostID, img, epoch); err != nil || ready {
		return err
	}
	e.mu.Lock()
	unsupported := e.unsupported[hostID]
	if inv := e.inventory[hostID]; inv != nil && inv.epoch == epoch {
		// Only a failure reported after this build request may end the wait.
		delete(inv.failed, img.ImageID)
	}
	e.mu.Unlock()
	if unsupported {
		return errors.New("host agent does not support image preparation")
	}
	removing, err := imageRemoving(ctx, e.pool, hostID, img.ImageID)
	if err != nil {
		return err
	}
	if removing {
		return errors.New("template image cleanup is in progress")
	}
	if !e.sendEnsure(hostID, img) {
		return errors.New("host rejected or did not accept template build")
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := e.lazyReady(ctx, hostID, img, epoch)
		if err != nil || ready {
			return err
		}
		e.mu.Lock()
		inv := e.inventory[hostID]
		failed := inv != nil && inv.epoch == epoch && inv.failed[img.ImageID] == img.Version
		e.mu.Unlock()
		if failed {
			return errors.New("template build failed on host")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
