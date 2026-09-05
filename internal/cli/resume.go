package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/sessions"
	"github.com/HappyOnigiri/WX/internal/sessions/identity"
)

type resumeStatus struct {
	WXSessionID    string `json:"wx_session_id"`
	Agent          string `json:"agent"`
	AgentSessionID string `json:"agent_session_id"`
	Expired        bool   `json:"expired"`
	Pending        bool   `json:"pending"`
	State          string `json:"state"`
}
type resumeTarget struct {
	Agent, AgentSessionID, WXSessionID, CWD string
}

func (c Client) ResolveSessionScope(ctx context.Context, root string) (sessions.PickerScope, error) {
	if err := c.ensureDaemon(ctx); err != nil {
		return sessions.PickerScope{}, err
	}
	var wire daemon.WorkspaceScope
	callCtx, cancel := context.WithTimeout(ctx, c.discoveryTimeout())
	defer cancel()
	if err := c.RPC.Call(callCtx, "WorkspaceScope", map[string]string{"cwd": root}, &wire); err != nil {
		return sessions.PickerScope{}, fmt.Errorf("resolve workspace history: %w; run wx daemon restart if the daemon has not been updated", err)
	}
	scope := sessions.PickerScope{Label: filepath.Base(wire.Root), Annotations: map[string]sessions.Annotation{}}
	scope.Roots = append(scope.Roots, sessions.ScopeRoot{Prefix: wire.Root, Label: scope.Label})
	for _, path := range wire.SlotPaths {
		scope.Roots = append(scope.Roots, sessions.ScopeRoot{Prefix: path, Label: scope.Label})
	}
	for _, se := range wire.Sessions {
		ids := []string{}
		if id := identity.ComputeSessionStableID(se.Agent, se.AgentSessionID); id != "" {
			ids = append(ids, id)
		}
		scope.StableIDs = append(scope.StableIDs, ids...)
		inUse := se.State == "STARTING" || se.State == "ACTIVE" || se.State == "RESTORING" || se.State == "UNBOUND"
		text := ""
		if inUse {
			text = "使用中"
		}
		for _, id := range ids {
			if _, exists := scope.Annotations[id]; !exists || inUse {
				scope.Annotations[id] = sessions.Annotation{Text: text, InUse: inUse}
			}
		}
	}
	return scope, nil
}

func (c Client) lookupManagedResume(ctx context.Context, agent, id string) (resumeTarget, bool, error) {
	var status resumeStatus
	err := c.RPC.Call(ctx, "ResumeStatus", map[string]string{"agent": agent, "agent_session_id": id}, &status)
	if err == nil {
		return resumeTarget{Agent: status.Agent, AgentSessionID: status.AgentSessionID, WXSessionID: status.WXSessionID}, true, nil
	}
	if !strings.Contains(err.Error(), "no rows") {
		return resumeTarget{}, false, err
	}
	return resumeTarget{}, false, nil
}

func (c Client) lookupResume(ctx context.Context, agent, id string) (resumeTarget, bool, error) {
	managed, found, err := c.lookupManagedResume(ctx, agent, id)
	if err != nil || found {
		return managed, found, err
	}
	target, found, err := sessions.Lookup(ctx, c.Config.Sessions, agent, id)
	if err != nil || !found {
		return resumeTarget{}, false, err
	}
	return resumeTarget{Agent: agent, AgentSessionID: id, CWD: target.CWD}, true, nil
}

func (c Client) resolveResume(ctx context.Context, agent, cwd string, intent resumeIntent, explicit string) (resumeTarget, bool, error) {
	if explicit != "" {
		return resumeTarget{Agent: agent, WXSessionID: explicit}, true, nil
	}
	switch intent.Kind {
	case resumeIntentNone:
		return resumeTarget{}, false, nil
	case resumeIntentLookup:
		return c.lookupResume(ctx, agent, intent.AgentSessionID)
	case resumeIntentContinueLatest, resumeIntentPicker:
		scope, err := c.ResolveSessionScope(ctx, cwd)
		if err != nil {
			return resumeTarget{}, false, err
		}
		var selectedScope *sessions.PickerScope = &scope
		if intent.WidenScope {
			selectedScope = nil
		}
		var target sessions.ResumeTarget
		if intent.Kind == resumeIntentContinueLatest {
			var found bool
			target, found, err = sessions.Continue(ctx, c.Config.Sessions, sessions.ContinueOptions{Tool: agent, Scope: selectedScope})
			if err == nil && !found {
				err = errors.New("no conversation found for this workspace")
			}
		} else {
			target, err = sessions.Pick(ctx, c.Config.Sessions, sessions.PickOptions{Tool: agent, Scope: selectedScope})
		}
		if err != nil {
			return resumeTarget{}, false, err
		}
		resolved, found, err := c.lookupManagedResume(ctx, target.Tool, target.SessionID)
		if err != nil {
			return resumeTarget{}, false, err
		}
		if found {
			return resolved, true, nil
		}
		return resumeTarget{Agent: target.Tool, AgentSessionID: target.SessionID, CWD: target.CWD}, true, nil
	default:
		return resumeTarget{}, false, errors.New("unknown resume intent")
	}
}

func resumeArgs(agent, id, path string, rest []string) []string {
	if id == "" {
		return rest
	}
	if agent == "codex" {
		return append([]string{"resume", "--cd", path, id}, rest...)
	}
	return append([]string{"--resume", id}, rest...)
}

func (c Client) RunAgent(ctx context.Context, agent string, args, branches []string, fresh bool) int {
	return c.runAgent(ctx, agent, args, branches, fresh, "")
}

func (c Client) RunResume(ctx context.Context, id, agent string, args, branches []string, fresh bool) int {
	return c.runAgent(ctx, agent, args, branches, fresh, id)
}

func validateResumeOptions(intent resumeIntent, explicit string, fresh bool, branches []string) error {
	resuming := intent.Kind != resumeIntentNone || explicit != ""
	if fresh && !resuming {
		return errors.New("--fresh requires a resume operation")
	}
	if len(branches) > 0 && !fresh && resuming {
		return errors.New("--branch requires --fresh when resuming")
	}
	return nil
}
