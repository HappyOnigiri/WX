package daemon

import (
	"context"
	"database/sql"
	"errors"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

type WorkspaceScope struct {
	WorkspaceID string               `json:"workspace_id"`
	Root        string               `json:"root"`
	Kind        string               `json:"kind"`
	Registered  bool                 `json:"registered"`
	SlotPaths   []string             `json:"slot_paths"`
	Sessions    []state.SessionScope `json:"sessions"`
}

func (m *Manager) WorkspaceScope(ctx context.Context, cwd string) (WorkspaceScope, error) {
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	w, err := discoverer.Resolve(ctx, cwd)
	if err != nil {
		return WorkspaceScope{}, err
	}
	w, err = m.store.CanonicalWorkspace(ctx, w)
	if err != nil {
		return WorkspaceScope{}, err
	}
	result := WorkspaceScope{WorkspaceID: string(w.ID), Root: string(w.Root), Kind: w.Kind, SlotPaths: []string{}, Sessions: []state.SessionScope{}}
	if _, err = m.store.Workspace(ctx, string(w.ID)); errors.Is(err, sql.ErrNoRows) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	result.Registered = true
	result.SlotPaths, err = m.store.WorkspaceSlotPaths(ctx, string(w.ID))
	if err != nil {
		return result, err
	}
	result.Sessions, err = m.store.WorkspaceSessionScopes(ctx, string(w.ID))
	return result, err
}
