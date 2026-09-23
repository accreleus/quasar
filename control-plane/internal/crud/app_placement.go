package crud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

type appPlacementHost struct {
	HostID   string  `json:"host_id"`
	Selected bool    `json:"selected"`
	Prepared *bool   `json:"prepared"`
	Ready    *bool   `json:"ready"`
	Reason   *string `json:"reason"`
}

type appPlacementView struct {
	AppID         string             `json:"app_id"`
	InheritedFrom *string            `json:"inherited_from"`
	Mode          string             `json:"mode"`
	HostIDs       []string           `json:"host_ids"`
	Revision      string             `json:"revision"`
	Hosts         []appPlacementHost `json:"hosts"`
}

type placementPatch struct {
	ExpectedRevision string   `json:"expected_revision"`
	Mode             string   `json:"mode"`
	HostIDs          []string `json:"host_ids"`
}

// appPlacementView reads policy selection separately from preparation and
// readiness. Neither a selected host nor an image cache claims launchability.
func (h *Handler) appPlacementView(ctx context.Context, id string) (appPlacementView, error) {
	v := appPlacementView{HostIDs: []string{}, Hosts: []appPlacementHost{}}
	var canonical string
	err := h.store.pool.QueryRow(ctx, `
		SELECT COALESCE(parent_app_id,id)::text, parent_app_id::text
		FROM apps WHERE id=$1::uuid`, id).Scan(&canonical, &v.InheritedFrom)
	if err != nil {
		return v, err
	}
	v.AppID = canonical
	err = h.store.pool.QueryRow(ctx, `SELECT mode, revision::text FROM app_placement WHERE app_id=$1::uuid`, canonical).Scan(&v.Mode, &v.Revision)
	if err != nil {
		return v, err
	}
	rows, err := h.store.pool.Query(ctx, `SELECT host_id::text FROM app_placement_hosts WHERE app_id=$1::uuid ORDER BY host_id`, canonical)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var hostID string
		if err := rows.Scan(&hostID); err != nil {
			rows.Close()
			return v, err
		}
		v.HostIDs = append(v.HostIDs, hostID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return v, err
	}
	rows.Close()

	// All enrolled hosts are shown, including offline ones. Preparation is
	// unknown for unmanaged images and absent reports; readiness is a distinct
	// current host observation, never inferred from selection alone.
	rows, err = h.store.pool.Query(ctx, `
		SELECT h.id::text,
		       CASE WHEN NOT prep.managed THEN NULL::boolean ELSE prep.prepared END,
		       CASE WHEN h.status='offline' THEN false
		            WHEN h.status='online' AND h.capacity_detection='ok'
		              AND NOT h.readiness_block_host AND NOT h.readiness_block_homes
		              AND h.config_policy_gate_connection IS NULL THEN true
		            ELSE false END
		FROM hosts h
		CROSS JOIN apps a
		LEFT JOIN LATERAL (
			SELECT count(*) > 0 AS managed,
			       bool_and(CASE WHEN hi.state IS NULL THEN NULL::boolean
			            ELSE hi.state='ready' AND (hi.version='' OR hi.version=ii.version) END) AS prepared
			FROM installed_images ii
			LEFT JOIN host_images hi ON hi.host_id=h.id AND hi.image_id=ii.image_id
			WHERE ii.registry_ref=a.runtime_spec->>'image' OR ii.local_tag=a.runtime_spec->>'image'
		) prep ON true
		WHERE a.id=$1::uuid ORDER BY h.node_name,h.id`, canonical)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	selected := make(map[string]bool, len(v.HostIDs))
	for _, hostID := range v.HostIDs {
		selected[hostID] = true
	}
	for rows.Next() {
		var host appPlacementHost
		if err := rows.Scan(&host.HostID, &host.Prepared, &host.Ready); err != nil {
			return v, err
		}
		host.Selected = v.Mode == "all_eligible" || selected[host.HostID]
		if host.Selected && host.Prepared != nil && !*host.Prepared {
			reason := "awaiting_preparation"
			host.Reason = &reason
		}
		v.Hosts = append(v.Hosts, host)
	}
	return v, rows.Err()
}

func (h *Handler) handleGetAppPlacement(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidUUID(id) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid app ID")
		return
	}
	v, err := h.appPlacementView(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load app placement")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (h *Handler) handlePatchAppPlacement(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidUUID(id) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid app ID")
		return
	}
	var p placementPatch
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&p)
	var extra any
	if decodeErr == nil {
		decodeErr = decoder.Decode(&extra)
		if errors.Is(decodeErr, io.EOF) {
			decodeErr = nil
		} else if decodeErr == nil {
			decodeErr = errors.New("multiple JSON values")
		}
	}
	if decodeErr != nil || p.HostIDs == nil ||
		(p.Mode != "fixed" && p.Mode != "all_eligible") || (p.Mode == "all_eligible" && len(p.HostIDs) != 0) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid placement selection")
		return
	}
	revision, err := strconv.ParseInt(p.ExpectedRevision, 10, 64)
	if err != nil || revision < 0 {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid expected revision")
		return
	}
	sort.Strings(p.HostIDs)
	for i, hostID := range p.HostIDs {
		if !isValidUUID(hostID) || (i > 0 && hostID == p.HostIDs[i-1]) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid or duplicate host ID")
			return
		}
	}
	ctx := r.Context()
	tx, err := h.store.pool.Begin(ctx)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not begin placement edit")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var canonical string
	var parent *string
	err = tx.QueryRow(ctx, `SELECT COALESCE(parent_app_id,id)::text,parent_app_id::text FROM apps WHERE id=$1::uuid FOR KEY SHARE`, id).Scan(&canonical, &parent)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not lock app")
		return
	}
	if parent != nil {
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{
			"error":         map[string]any{"code": "inherited_placement", "message": "derived tiles inherit their parent app placement"},
			"parent_app_id": canonical,
		})
		return
	}
	var current int64
	err = tx.QueryRow(ctx, `SELECT revision FROM app_placement WHERE app_id=$1::uuid FOR UPDATE`, canonical).Scan(&current)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not lock app placement")
		return
	}
	if current != revision {
		v, readErr := h.appPlacementView(ctx, id)
		if readErr != nil {
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load current placement")
			return
		}
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{
			"error":   map[string]any{"code": "stale_revision", "message": "placement changed; review the current selection"},
			"current": v,
		})
		return
	}
	if len(p.HostIDs) > 0 {
		var existing int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM hosts WHERE id=ANY($1::uuid[])`, p.HostIDs).Scan(&existing); err != nil || existing != len(p.HostIDs) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "selection includes an unknown host")
			return
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app_placement_hosts WHERE app_id=$1::uuid`, canonical); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save placement")
		return
	}
	for _, hostID := range p.HostIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO app_placement_hosts(app_id,host_id) VALUES ($1::uuid,$2::uuid)`, canonical, hostID); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save placement")
			return
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE app_placement SET mode=$2,revision=revision+1 WHERE app_id=$1::uuid`, canonical, p.Mode); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save placement")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not commit placement")
		return
	}
	v, err := h.appPlacementView(ctx, id)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "placement saved but could not load it")
		return
	}
	h.recordActivity(r, "app.placement.update", "app", canonical, map[string]any{"fields": []string{"mode", "host_ids"}})
	httpx.WriteJSON(w, http.StatusOK, v)
}
