package agentsession

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// RecordResponse writes one model call to a session: every output item
// of resp as an item entry carrying resp.ID, then the response entry
// with the response's model, status, usage, incomplete details, error,
// the hash of req and the latency. req must be the request as it was
// sent, so the recorded hash is the one a reader rebuilds from the
// path; pass a zero latency to leave latency_ms out. It returns the ID
// of the response entry.
//
// The caller records the user items and function call outputs that
// precede the request before calling this, as the format requires.
func RecordResponse(ctx context.Context, store Store, sessionID string, req openresponses.Request, resp *openresponses.Response, latency time.Duration) (string, error) {
	if resp == nil {
		return "", fmt.Errorf("agentsession: nil response")
	}
	hash, err := RequestHash(req)
	if err != nil {
		return "", err
	}
	for i, item := range resp.Output {
		if item == nil {
			return "", fmt.Errorf("agentsession: output[%d] is nil", i)
		}
		if _, err := store.Append(ctx, sessionID, &ItemEntry{Item: item, ResponseID: resp.ID}); err != nil {
			return "", fmt.Errorf("agentsession: record output[%d]: %w", i, err)
		}
	}
	entry := &ResponseEntry{
		ResponseID:  resp.ID,
		Model:       resp.Model,
		Status:      resp.Status,
		Usage:       resp.Usage,
		Incomplete:  resp.IncompleteDetails,
		Error:       resp.Error,
		RequestHash: hash,
	}
	if latency > 0 {
		entry.LatencyMS = latency.Milliseconds()
	}
	id, err := store.Append(ctx, sessionID, entry)
	if err != nil {
		return "", fmt.Errorf("agentsession: record response: %w", err)
	}
	return id, nil
}

// requestOnlyKeys are request members that describe one call rather
// than the settings in force, so ConfigFromRequest leaves them out.
var requestOnlyKeys = map[string]bool{
	"input":                true,
	"previous_response_id": true,
	"store":                true,
	"stream":               true,
	"stream_options":       true,
	"background":           true,
}

// settingsKeys are request members that Settings carries as named
// fields rather than in Extra.
var settingsKeys = map[string]bool{
	"model":        true,
	"instructions": true,
	"reasoning":    true,
	"text":         true,
	"tools":        true,
}

// ConfigFromRequest builds the full config entry for the settings a
// request was sent with: its model, instructions, reasoning, text and
// tools, with every other request member that is not about the one
// call (temperature, tool_choice, metadata, provider passthrough keys
// and so on) in Extra under its wire name. Replay of the result over
// empty settings followed by Settings.Request over the same input
// hashes to the same value as the request, which is what the first
// entry on a root should establish. Replace is set so the entry stands
// alone wherever it is placed.
func ConfigFromRequest(req openresponses.Request) (*ConfigEntry, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("agentsession: encode request: %w", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("agentsession: decode request: %w", err)
	}
	cfg := &ConfigEntry{Model: req.Model, Replace: true}
	if req.Instructions != "" {
		instructions := req.Instructions
		cfg.Instructions = &instructions
	}
	if !req.Reasoning.IsZero() {
		reasoning := req.Reasoning
		cfg.Reasoning = &reasoning
	}
	if !req.Text.IsZero() {
		text := req.Text
		cfg.Text = &text
	}
	if len(req.Tools) > 0 {
		cfg.ToolsAdded = append(openresponses.Tools(nil), req.Tools...)
	}
	for k, raw := range all {
		if requestOnlyKeys[k] || settingsKeys[k] {
			continue
		}
		if cfg.Extra == nil {
			cfg.Extra = map[string]json.RawMessage{}
		}
		cfg.Extra[k] = raw
	}
	return cfg, nil
}
