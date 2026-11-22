package main

import (
	"net/http"
	"slices"
	"strings"
)

type modelInfo struct {
	ID              string   `json:"id"`
	Object          string   `json:"object"`
	OwnedBy         string   `json:"owned_by"`
	Name            string   `json:"name"`
	CanonicalID     string   `json:"canonical_id,omitempty"`
	ReasoningEffort []string `json:"reasoning_efforts,omitempty"`
}

func thoughtLevels(options []any) []string {
	var levels []string
	var visit func([]any)
	visit = func(items []any) {
		for _, raw := range items {
			o := asObj(raw)
			if v := str(o["value"]); v != "" && !slices.Contains(levels, v) {
				levels = append(levels, v)
			}
			visit(arr(o["options"]))
		}
	}
	for _, raw := range options {
		o := asObj(raw)
		if o["id"] == "thought_level" || o["category"] == "thought_level" {
			visit(arr(o["options"]))
		}
	}
	return levels
}

func (g *gateway) modelDirectoryLocked() []modelInfo {
	ids := g.modelsLocked()
	models := make([]modelInfo, 0, len(ids))
	for _, id := range ids {
		native := id
		if target := g.cfg.models[id]; target != "" && slices.Contains(g.catalog, target) {
			native = target
		}
		m := g.modelDetails[native]
		m.ID, m.Object, m.OwnedBy = id, "model", "agy"
		if m.Name == "" {
			m.Name = native
		}
		if native != id {
			m.CanonicalID = native
		}
		models = append(models, m)
	}
	return models
}

func (g *gateway) serveModels(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		return problem(405, "method_not_allowed", "Method not allowed.")
	}
	g.mu.Lock()
	models := g.modelDirectoryLocked()
	if len(models) == 0 {
		err := g.unavailableLocked()
		g.mu.Unlock()
		return err
	}
	g.mu.Unlock()
	if r.URL.Path == "/v1/models" {
		_ = writeJSON(w, 200, obj{"object": "list", "data": models})
		return nil
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	for _, m := range models {
		if m.ID == id {
			_ = writeJSON(w, 200, m)
			return nil
		}
	}
	return problem(404, "model_not_found", "The requested model is unavailable.")
}

func (g *gateway) updateThoughtLevelsLocked(id string, options []any) {
	if model, ok := g.modelDetails[id]; ok && options != nil {
		model.ReasoningEffort = thoughtLevels(options)
		g.modelDetails[id] = model
	}
}
