package hostcfg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type mediaProbe struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at"`
	Blocks     *struct {
		GPUIndex *int `json:"gpu_index"`
	} `json:"blocks"`
}

type hardwareGPU struct {
	Index          int
	Vendor         string
	RenderNode     string
	DriverIdentity string
	Probe          mediaProbe
}

type hardwareReportGPU struct {
	Index            int    `json:"index"`
	Vendor           string `json:"vendor"`
	RenderNode       string `json:"render_node"`
	DriverIdentity   string `json:"driver_identity"`
	EncodeSlotsTotal int    `json:"encode_slots_total"`
}

// automaticHardwareEvidence uses reports received after this host registered.
// The agent still checks the actual device and its most recent probe at offer
// acceptance; a database report is never treated as proof of application.
func (s *Store) automaticHardwareEvidence(ctx context.Context, hostID string, selectedNode string) (*hardwareGPU, []ApprovalFact, error) {
	var readiness, gpuRaw []byte
	var connectionID string
	err := s.pool.QueryRow(ctx, `SELECT e.gpus,e.readiness,e.connection_incarnation::text
		FROM host_hardware_evidence e JOIN hosts h ON h.id=e.host_id
		JOIN host_journal_reconciliation j ON j.host_id=e.host_id
		WHERE e.host_id=$1::uuid AND j.state='complete'
		AND e.connection_incarnation=j.connection_incarnation
		AND e.received_at>=h.last_registered_at`, hostID).Scan(&gpuRaw, &readiness, &connectionID)
	if err == pgx.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var checks []mediaProbe
	if err := json.Unmarshal(readiness, &checks); err != nil {
		return nil, nil, nil
	}
	probes := make(map[string]mediaProbe, len(checks))
	for _, check := range checks {
		probes[check.ID] = check
	}
	var reported []hardwareReportGPU
	if err := json.Unmarshal(gpuRaw, &reported); err != nil {
		return nil, nil, nil
	}
	var matched []hardwareGPU
	for _, report := range reported {
		if report.Index < 0 || report.EncodeSlotsTotal <= 0 || report.RenderNode == "" || report.DriverIdentity == "" {
			continue
		}
		gpu := hardwareGPU{Index: report.Index, Vendor: report.Vendor,
			RenderNode: report.RenderNode, DriverIdentity: report.DriverIdentity}
		if selectedNode != "" && gpu.RenderNode != selectedNode {
			continue
		}
		probe, ok := probes[fmt.Sprintf("media_probe_gpu%d", gpu.Index)]
		if !ok || probe.Status != "pass" || probe.Source != "host_probe" || probe.ObservedAt == "" ||
			(probe.Blocks != nil && (probe.Blocks.GPUIndex == nil || *probe.Blocks.GPUIndex != gpu.Index)) ||
			strings.ContainsAny(probe.ObservedAt, "\x00\n") || !asciiRFC3339(probe.ObservedAt) {
			continue
		}
		gpu.Probe = probe
		matched = append(matched, gpu)
	}
	if len(matched) != 1 {
		return nil, nil, nil
	}
	gpu := matched[0]
	for _, value := range []string{gpu.RenderNode, gpu.DriverIdentity} {
		if strings.ContainsRune(value, 0) {
			return nil, nil, nil
		}
	}
	deviceBytes := []byte(fmt.Sprintf("gpu\x00%d\x00%s\x00%s\n", gpu.Index, gpu.RenderNode, gpu.DriverIdentity))
	deviceSum := sha256.Sum256(deviceBytes)
	probeBytes := []byte(fmt.Sprintf("media_probe_gpu%d\x00%s\x00%s\x00host_probe\x00pass\n", gpu.Index, hex.EncodeToString(deviceSum[:]), connectionID))
	probeSum := sha256.Sum256(probeBytes)
	facts := []ApprovalFact{
		{Kind: "accessible_device", ID: hex.EncodeToString(deviceSum[:])},
		{Kind: "driver_identity", ID: gpu.DriverIdentity},
		{Kind: "host_probe_result", ID: hex.EncodeToString(probeSum[:])},
	}
	return &gpu, facts, nil
}

func asciiRFC3339(value string) bool {
	for _, b := range []byte(value) {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func automaticEncoder(vendor string) string {
	switch strings.ToLower(strings.TrimSpace(vendor)) {
	case "nvidia", "amd":
		return "vulkan"
	case "intel":
		return "va"
	default:
		return ""
	}
}

// ObserveHardwareReport records only a single authenticated capacity report's
// GPU and readiness arrays. A previous websocket cannot refresh or clear the
// projection after a new connection has taken the journal gate.
func (s *Store) ObserveHardwareReport(ctx context.Context, hostID, connectionID string, gpuRaw, readinessRaw json.RawMessage) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT id FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID); err != nil {
		return err
	}
	var current *string
	if err := tx.QueryRow(ctx, `SELECT connection_incarnation::text FROM host_journal_reconciliation
		WHERE host_id=$1::uuid`, hostID).Scan(&current); err != nil {
		return err
	}
	if current == nil || *current != connectionID {
		return ErrApprovalSuperseded
	}
	var oldGPUs, oldReadiness []byte
	oldErr := tx.QueryRow(ctx, `SELECT gpus,readiness FROM host_hardware_evidence
		WHERE host_id=$1::uuid AND connection_incarnation=$2::uuid`, hostID, connectionID).Scan(&oldGPUs, &oldReadiness)
	if oldErr != nil && oldErr != pgx.ErrNoRows {
		return oldErr
	}
	oldSignature := hardwareEvidenceSignature(oldGPUs, oldReadiness)
	newSignature := hardwareEvidenceSignature(gpuRaw, readinessRaw)
	if newSignature == "" {
		if _, err := tx.Exec(ctx, `DELETE FROM host_hardware_evidence WHERE host_id=$1::uuid AND connection_incarnation=$2::uuid`, hostID, connectionID); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `INSERT INTO host_hardware_evidence(host_id,connection_incarnation,gpus,readiness)
			VALUES($1::uuid,$2::uuid,$3::jsonb,$4::jsonb) ON CONFLICT(host_id) DO UPDATE SET
			connection_incarnation=excluded.connection_incarnation,gpus=excluded.gpus,
			readiness=excluded.readiness,received_at=now()`, hostID, connectionID, gpuRaw, readinessRaw); err != nil {
			return err
		}
	}
	if oldSignature != newSignature {
		var automatic bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_setting_choices
			WHERE host_id=$1::uuid AND source='automatic' AND key IN ('encoder','render_node'))`, hostID).Scan(&automatic); err != nil {
			return err
		}
		if automatic {
			if err := rotateHostReviewTokens(ctx, tx, hostID); err != nil {
				return err
			}
		}
		var liveID, state string
		err := tx.QueryRow(ctx, `SELECT a.id::text,a.state FROM host_config_approvals a
			WHERE a.host_id=$1::uuid AND a.group_key='hardware' AND a.state IN ('approved','offered')
			AND EXISTS(SELECT 1 FROM host_setting_choices c WHERE c.host_id=a.host_id
			AND c.source='automatic' AND c.key IN ('encoder','render_node'))`, hostID).Scan(&liveID, &state)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if err == nil {
			if state == "approved" {
				if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='superseded'
					WHERE id=$1::uuid AND state='approved'`, liveID); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions
					WHERE host_id=$1::uuid AND owner_kind='idle_apply' AND owner_id=$2::uuid`, hostID, liveID); err != nil {
					return err
				}
			} else {
				if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='cancel_pending'
					WHERE id=$1::uuid AND state='offered'`, liveID); err != nil {
					return err
				}
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE hosts SET status='online' WHERE id=$1::uuid
		AND status='draining' AND NOT EXISTS(SELECT 1 FROM host_admission_restrictions
		WHERE host_id=$1::uuid)`, hostID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// The signature intentionally excludes observed_at and prose so a repeated
// pass on the same device does not churn a waiting approval. It does include
// failures, identity and access-relevant GPU details.
func hardwareEvidenceSignature(gpuRaw, readinessRaw []byte) string {
	var gpus []hardwareReportGPU
	var checks []mediaProbe
	if len(gpuRaw) == 0 || len(readinessRaw) == 0 ||
		json.Unmarshal(gpuRaw, &gpus) != nil || json.Unmarshal(readinessRaw, &checks) != nil {
		return ""
	}
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].Index < gpus[j].Index })
	proof := make([]any, 0, len(gpus))
	for _, gpu := range gpus {
		var probe mediaProbe
		for _, check := range checks {
			if check.ID == fmt.Sprintf("media_probe_gpu%d", gpu.Index) {
				probe = check
				break
			}
		}
		proof = append(proof, []any{gpu.Index, gpu.Vendor, gpu.RenderNode,
			gpu.DriverIdentity, gpu.EncodeSlotsTotal > 0, probe.Status, probe.Source})
	}
	digest, err := digestJSON(proof)
	if err != nil {
		return ""
	}
	return digest
}
