package session

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// #268: the agent numbers GPUs by DRM card position, so a host's only usable GPU
// can carry index 1, and its Vulkan path follows the scheduled GPU's render node
// (bind_gpu needs no ordinal). These tests pin the scheduler to the same rule.

const (
	vkNode0 = "/dev/dri/by-path/pci-0000:04:00.0-render"
	vkNode1 = "/dev/dri/by-path/pci-0000:05:00.0-render"
)

func setHostSettings(t *testing.T, s seedIDs, exec func(string, ...any) error, settings string) {
	t.Helper()
	if err := exec(`UPDATE hosts SET effective_settings=$2::jsonb WHERE id::text=$1`, s.hostID, settings); err != nil {
		t.Fatalf("configure host %s: %v", settings, err)
	}
}

// TestVulkanBindingSQLHasNoIndexCondition pins the Vulkan arm without a database:
// no arm of the binding predicate may test the GPU's index.
func TestVulkanBindingSQLHasNoIndexCondition(t *testing.T) {
	if strings.Contains(schedulableBindingSQL, "g.index") {
		t.Fatalf("schedulableBindingSQL tests g.index; the agent's bind_gpu resolves every " +
			"encoder by render node and vendor, never by ordinal")
	}
	if !strings.Contains(normSQL(schedulableBindingSQL),
		"h.effective_settings->>'encoder' = 'vulkan' AND (COALESCE(h.effective_settings->>'render_node', '') = ''") {
		t.Fatalf("the Vulkan arm must keep its render-node match:\n%s", schedulableBindingSQL)
	}
}

// TestVulkanSingleGPUAtIndexOneIsSchedulable: the #268 host. One reported GPU,
// index 1, default Vulkan encoder, render node pinned to that GPU or unset.
func TestVulkanSingleGPUAtIndexOneIsSchedulable(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 2)
	ctx := context.Background()
	exec := func(q string, a ...any) error { _, err := pool.Exec(ctx, q, a...); return err }

	if err := exec(`UPDATE gpus SET index=1, vendor='amd', render_node=$2,
		device_path='/dev/dri/renderD129' WHERE id::text=$1`, s.gpuID, vkNode1); err != nil {
		t.Fatalf("configure gpu: %v", err)
	}

	var maxAttempt int
	attemptObserver = func(attempt int) {
		if attempt > maxAttempt {
			maxAttempt = attempt
		}
	}
	t.Cleanup(func() { attemptObserver = nil })

	for _, settings := range []string{
		`{"encoder":"vulkan","render_node":"/dev/dri/renderD129"}`, // device_path match (the #268 report)
		`{"encoder":"vulkan","render_node":"` + vkNode1 + `"}`,     // by-path match
		`{"encoder":"vulkan","render_node":""}`,                    // unpinned
		`{"encoder":"vulkan"}`,                                     // key absent
	} {
		setHostSettings(t, s, exec, settings)
		maxAttempt = 0
		sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
		if err != nil {
			t.Fatalf("schedule with %s: %v", settings, err)
		}
		if sess.GPUID == nil || *sess.GPUID != s.gpuID {
			t.Fatalf("with %s scheduled gpu=%v want %s", settings, sess.GPUID, s.gpuID)
		}
		if maxAttempt != 0 {
			t.Fatalf("with %s the pick and the under-lock re-check disagreed (%d retries)", settings, maxAttempt)
		}
		if err := exec(`UPDATE sessions SET state='stopped' WHERE id::text=$1`, sess.ID); err != nil {
			t.Fatalf("release session: %v", err)
		}
	}

	// A non-empty render node naming no reported GPU still excludes the host.
	setHostSettings(t, s, exec, `{"encoder":"vulkan","render_node":"/dev/dri/renderD999"}`)
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("mismatched render_node: got %v want ErrNoHostAvailable", err)
	}
}

// TestVulkanTwoGPUsFollowsRenderNode: two GPUs, Vulkan pinned to the second. The
// scheduler must pick index 1 and never index 0, and once index 1 is full the
// refusal is capacity_exhausted (totals sees it), not no_host_available.
func TestVulkanTwoGPUsFollowsRenderNode(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 1)
	setQuota(t, pool, s.userID, 20)
	ctx := context.Background()
	exec := func(q string, a ...any) error { _, err := pool.Exec(ctx, q, a...); return err }

	if err := exec(`UPDATE gpus SET vendor='amd', render_node=$2, device_path='/dev/dri/renderD128'
		WHERE id::text=$1`, s.gpuID, vkNode0); err != nil {
		t.Fatalf("configure gpu0: %v", err)
	}
	var gpu1 string
	if err := pool.QueryRow(ctx, `INSERT INTO gpus(host_id,index,vendor,vram_mb_total,encode_slots_total,render_node,device_path)
		VALUES($1,1,'amd',16384,1,$2,'/dev/dri/renderD129') RETURNING id::text`, s.hostID, vkNode1).Scan(&gpu1); err != nil {
		t.Fatalf("seed gpu1: %v", err)
	}
	setHostSettings(t, s, exec, `{"encoder":"vulkan","render_node":"/dev/dri/renderD129"}`)

	var maxAttempt int
	attemptObserver = func(attempt int) {
		if attempt > maxAttempt {
			maxAttempt = attempt
		}
	}
	t.Cleanup(func() { attemptObserver = nil })

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if sess.GPUID == nil || *sess.GPUID != gpu1 {
		t.Fatalf("scheduled gpu=%v want the render-node GPU (index 1) %s", sess.GPUID, gpu1)
	}
	if maxAttempt != 0 {
		t.Fatalf("pick and re-check disagreed (%d retries)", maxAttempt)
	}

	// index 1 is now full; index 0 has a free slot but is not the bound GPU.
	maxAttempt = 0
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("second launch: got %v want ErrCapacityExhausted (index 0 must not be used, "+
			"and totals must see index 1)", err)
	}
	if maxAttempt != 0 {
		t.Fatalf("the refusal burned %d retries", maxAttempt+1)
	}

	// Unpinned: both GPUs are eligible, so the free one (index 0) takes the launch.
	setHostSettings(t, s, exec, `{"encoder":"vulkan"}`)
	sess2, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("unpinned schedule: %v", err)
	}
	if sess2.GPUID == nil || *sess2.GPUID != s.gpuID {
		t.Fatalf("unpinned scheduled gpu=%v want the free index-0 GPU %s", sess2.GPUID, s.gpuID)
	}
}
