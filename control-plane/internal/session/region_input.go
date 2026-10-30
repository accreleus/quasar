// region_input.go — per-user Region and Input settings (amendment 49, #845):
// GET/PATCH /v1/me/region-input, and the nine env keys every app gets at
// launch. Semantics: control-api.md §Region and Input settings.
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/settings"
)

const scopeSiteRegionInput = "launch.region_input"

// RegionSettings and InputSettings are the user's own values; nil = not set.
type RegionSettings struct {
	Timezone     *string `json:"timezone"`
	Language     *string `json:"language"`
	RegionFormat *string `json:"region_format"`
}

type InputSettings struct {
	Keyboard         *settings.Keyboard `json:"keyboard"`
	MatchCursor      *string            `json:"match_cursor"`
	MouseSensitivity *float64           `json:"mouse_sensitivity"`
	RawMouse         *bool              `json:"raw_mouse"`
	FlipScroll       *bool              `json:"flip_scroll"`
	NintendoLayout   *bool              `json:"nintendo_layout"`
}

type RegionInput struct {
	Region RegionSettings `json:"region"`
	Input  InputSettings  `json:"input"`
}

func defaultsAsRegionInput(d settings.RegionInputDefaults) RegionInput {
	kb := d.Keyboard
	return RegionInput{
		Region: RegionSettings{Timezone: &d.Timezone, Language: &d.Language, RegionFormat: d.RegionFormat},
		Input: InputSettings{Keyboard: &kb, MatchCursor: &d.MatchCursor, MouseSensitivity: &d.MouseSensitivity,
			RawMouse: &d.RawMouse, FlipScroll: &d.FlipScroll, NintendoLayout: &d.NintendoLayout},
	}
}

const regionInputColumns = `timezone, language, region_format, keyboard_layout, keyboard_variant,
	match_cursor, mouse_sensitivity, raw_mouse, flip_scroll, nintendo_layout`

func scanRegionInput(row pgx.Row) (RegionInput, error) {
	var r RegionInput
	var layout, variant *string
	err := row.Scan(&r.Region.Timezone, &r.Region.Language, &r.Region.RegionFormat, &layout, &variant,
		&r.Input.MatchCursor, &r.Input.MouseSensitivity, &r.Input.RawMouse, &r.Input.FlipScroll, &r.Input.NintendoLayout)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegionInput{}, nil
	}
	if layout != nil && variant != nil {
		r.Input.Keyboard = &settings.Keyboard{Layout: *layout, Variant: *variant}
	}
	return r, err
}

// GetRegionInput is the user's row; no row reads every field nil.
func (s *Store) GetRegionInput(ctx context.Context, userID string) (RegionInput, error) {
	r, err := scanRegionInput(s.pool.QueryRow(ctx,
		`SELECT `+regionInputColumns+` FROM user_region_input WHERE user_id = $1::uuid`, userID))
	if err != nil {
		return RegionInput{}, fmt.Errorf("read region and input settings: %w", err)
	}
	return r, nil
}

// RegionInputDefaults is the instance_settings defaults; no row reads the built-in ones.
func (s *Store) RegionInputDefaults(ctx context.Context) (settings.RegionInputDefaults, error) {
	d, err := settings.ScanRegionInputDefaults(s.pool.QueryRow(ctx,
		`SELECT `+settings.RegionInputDefaultsColumns+` FROM instance_settings WHERE id = true`))
	if errors.Is(err, pgx.ErrNoRows) {
		return settings.BuiltInRegionInputDefaults, nil
	}
	if err != nil {
		return settings.RegionInputDefaults{}, fmt.Errorf("read region and input defaults: %w", err)
	}
	return d, nil
}

// SaveRegionInput applies patch to the user's row under a row lock and
// returns the row before and after.
func (s *Store) SaveRegionInput(ctx context.Context, userID string, patch func(*RegionInput)) (before, after RegionInput, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return before, after, fmt.Errorf("begin region and input save: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO user_region_input (user_id) VALUES ($1::uuid)
		ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return before, after, fmt.Errorf("seed region and input settings: %w", err)
	}
	if before, err = scanRegionInput(tx.QueryRow(ctx, `SELECT `+regionInputColumns+
		` FROM user_region_input WHERE user_id = $1::uuid FOR UPDATE`, userID)); err != nil {
		return before, after, fmt.Errorf("lock region and input settings: %w", err)
	}
	after = before
	patch(&after)
	var layout, variant *string
	if kb := after.Input.Keyboard; kb != nil {
		layout, variant = &kb.Layout, &kb.Variant
	}
	in := after.Input
	if _, err := tx.Exec(ctx, `UPDATE user_region_input SET timezone = $2, language = $3,
		region_format = $4, keyboard_layout = $5, keyboard_variant = $6, match_cursor = $7,
		mouse_sensitivity = $8, raw_mouse = $9, flip_scroll = $10, nintendo_layout = $11
		WHERE user_id = $1::uuid`, userID, after.Region.Timezone, after.Region.Language,
		after.Region.RegionFormat, layout, variant, in.MatchCursor, in.MouseSensitivity,
		in.RawMouse, in.FlipScroll, in.NintendoLayout); err != nil {
		return before, after, fmt.Errorf("save region and input settings: %w", err)
	}
	return before, after, tx.Commit(ctx)
}

// --- the route -----------------------------------------------------------------

func (h *Handler) handleGetRegionInput(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	mine, err := h.store.GetRegionInput(r.Context(), user.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load region and input settings")
		return
	}
	h.writeRegionInput(w, r, mine, nil)
}

func (h *Handler) writeRegionInput(w http.ResponseWriter, r *http.Request, mine RegionInput, stopped *bool) {
	defaults, err := h.store.RegionInputDefaults(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load region and input settings")
		return
	}
	body := map[string]any{"region": mine.Region, "input": mine.Input, "defaults": defaultsAsRegionInput(defaults)}
	if stopped != nil {
		body["parked_session_stopped"] = *stopped
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

func (h *Handler) handlePatchRegionInput(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	var req struct {
		Region   json.RawMessage `json:"region"`
		Input    json.RawMessage `json:"input"`
		Defaults json.RawMessage `json:"defaults"`
		Stopped  json.RawMessage `json:"parked_session_stopped"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	patch, err := parseRegionInputPatch(req.Region, req.Input)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}
	before, after, err := h.store.SaveRegionInput(r.Context(), user.ID, patch)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save region and input settings")
		return
	}
	stopped := false
	if !sameLaunchValues(before, after) {
		n, err := h.coord.stopParked(r.Context(), StopReasonParkedSuperseded, parkedFilter{UserID: user.ID})
		if err != nil {
			h.coord.log.Warn("region and input: stopping the parked session failed", "user_id", user.ID, "err", err)
		}
		stopped = n > 0
	}
	h.writeRegionInput(w, r, after, &stopped)
}

// sameLaunchValues compares the four values that reach the app; a change to
// one of them stops the user's parked session.
func sameLaunchValues(a, b RegionInput) bool {
	eq := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	ak, bk := a.Input.Keyboard, b.Input.Keyboard
	return eq(a.Region.Timezone, b.Region.Timezone) && eq(a.Region.Language, b.Region.Language) &&
		eq(a.Region.RegionFormat, b.Region.RegionFormat) &&
		(ak == nil) == (bk == nil) && (ak == nil || *ak == *bk)
}

// parseRegionInputPatch validates the whole body before anything is written:
// an absent field is unchanged, null clears it, keyboard is replaced whole.
func parseRegionInputPatch(region, input json.RawMessage) (func(*RegionInput), error) {
	var steps []func(*RegionInput)
	fields := func(raw json.RawMessage, name string, known map[string]func(json.RawMessage) (func(*RegionInput), error)) error {
		if len(raw) == 0 {
			return nil
		}
		var m map[string]json.RawMessage
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &m) != nil {
			return fmt.Errorf("%s must be an object", name)
		}
		for k, v := range m {
			parse, ok := known[k]
			if !ok {
				return fmt.Errorf("%s.%s is not a known field", name, k)
			}
			step, err := parse(v)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", name, k, err)
			}
			steps = append(steps, step)
		}
		return nil
	}
	err := fields(region, "region", map[string]func(json.RawMessage) (func(*RegionInput), error){
		"timezone": patchField(settings.ValidTimezone, "an IANA time zone name",
			func(r *RegionInput) **string { return &r.Region.Timezone }),
		"language": patchField(settings.ValidLanguage, "a language and region such as de-DE",
			func(r *RegionInput) **string { return &r.Region.Language }),
		"region_format": patchField(settings.ValidLanguage, "a language and region such as de-DE",
			func(r *RegionInput) **string { return &r.Region.RegionFormat }),
	})
	if err != nil {
		return nil, err
	}
	err = fields(input, "input", map[string]func(json.RawMessage) (func(*RegionInput), error){
		"keyboard": func(raw json.RawMessage) (func(*RegionInput), error) {
			if isNull(raw) {
				return func(r *RegionInput) { r.Input.Keyboard = nil }, nil
			}
			kb, err := settings.DecodeKeyboard(raw)
			if err != nil {
				return nil, err
			}
			return func(r *RegionInput) { r.Input.Keyboard = &kb }, nil
		},
		"match_cursor": patchField(settings.ValidMatchCursor, "auto, on or off",
			func(r *RegionInput) **string { return &r.Input.MatchCursor }),
		"mouse_sensitivity": patchField(settings.ValidMouseSensitivity, "a number from 0.25 to 4",
			func(r *RegionInput) **float64 { return &r.Input.MouseSensitivity }),
		"raw_mouse": patchField(func(bool) bool { return true }, "a boolean",
			func(r *RegionInput) **bool { return &r.Input.RawMouse }),
		"flip_scroll": patchField(func(bool) bool { return true }, "a boolean",
			func(r *RegionInput) **bool { return &r.Input.FlipScroll }),
		"nintendo_layout": patchField(func(bool) bool { return true }, "a boolean",
			func(r *RegionInput) **bool { return &r.Input.NintendoLayout }),
	})
	if err != nil {
		return nil, err
	}
	return func(r *RegionInput) {
		for _, step := range steps {
			step(r)
		}
	}, nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// patchField parses one field: null clears it, otherwise it must decode as T and pass valid.
func patchField[T any](valid func(T) bool, want string, field func(*RegionInput) **T) func(json.RawMessage) (func(*RegionInput), error) {
	return func(raw json.RawMessage) (func(*RegionInput), error) {
		if isNull(raw) {
			return func(r *RegionInput) { *field(r) = nil }, nil
		}
		var v T
		if json.Unmarshal(raw, &v) != nil || !valid(v) {
			return nil, fmt.Errorf("must be %s or null", want)
		}
		return func(r *RegionInput) { *field(r) = &v }, nil
	}
}

// --- the launch environment ------------------------------------------------------

// regionInputEnvKeys are the nine keys the control plane owns in every app's env.
var regionInputEnvKeys = []string{"TZ", "LANG", "LC_TIME", "LC_NUMERIC", "LC_MONETARY",
	"LC_MEASUREMENT", "LC_PAPER", "XKB_DEFAULT_LAYOUT", "XKB_DEFAULT_VARIANT"}

// regionInputEnv resolves the four launch values (user → browser → install
// default → built-in) into the nine env keys.
func regionInputEnv(mine RegionInput, device DeviceScope, d settings.RegionInputDefaults) map[string]string {
	first := func(vs ...*string) string {
		for _, v := range vs {
			if v != nil && *v != "" {
				return *v
			}
		}
		return ""
	}
	browser := func(v string) *string { return &v }
	tz := first(mine.Region.Timezone, browser(device.Timezone), &d.Timezone)
	lang := first(mine.Region.Language, browser(device.Language), &d.Language)
	format := first(mine.Region.RegionFormat, d.RegionFormat, &lang)
	kb := d.Keyboard
	if mine.Input.Keyboard != nil {
		kb = *mine.Input.Keyboard
	}
	locale := func(v string) string { return strings.Replace(v, "-", "_", 1) + ".UTF-8" }
	env := map[string]string{"TZ": tz, "LANG": locale(lang),
		"XKB_DEFAULT_LAYOUT": kb.Layout, "XKB_DEFAULT_VARIANT": kb.Variant}
	for _, k := range regionInputEnvKeys[2:7] {
		env[k] = locale(format)
	}
	return env
}

// applyRegionInput sets the nine keys over whatever the app or its preset
// carries. Every session_assign and session_swap_app spec goes through it.
func (c *Coordinator) applyRegionInput(ctx context.Context, spec []byte, userID string, deviceID *string) ([]byte, error) {
	mine, err := c.store.GetRegionInput(ctx, userID)
	if err != nil {
		return nil, err
	}
	d, err := c.store.RegionInputDefaults(ctx)
	if err != nil {
		return nil, err
	}
	device, err := c.store.ResolveDeviceScope(ctx, userID, strOrEmpty(deviceID), scopeSiteRegionInput)
	if err != nil {
		// As every scope read: proceed without the browser's values.
		c.log.Warn("region and input: device read failed, using the install defaults", "user_id", userID, "err", err)
		device = DeviceScope{}
	}
	env := regionInputEnv(mine, device, d)
	values := make(map[string]*string, len(env))
	for k, v := range env {
		values[k] = &v
	}
	return setSpecEnvs(spec, values)
}

// swapSpec is a swap target's dispatch spec: its home, then its region values.
func (c *Coordinator) swapSpec(ctx context.Context, app LaunchApp, sess Session) ([]byte, error) {
	spec, err := c.resolveHomeSpec(ctx, app, sess.UserID, *sess.HostID)
	if err != nil {
		return nil, err
	}
	return c.applyRegionInput(ctx, spec, sess.UserID, sess.DeviceID)
}
