package daemon

import (
	"context"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/state"
)

// legacyLeaseFixture は旧リリースが残した UNBOUND の回収を検証する fixture である。
func legacyLeaseFixture(m *Manager, agent string, pid int) (Lease, error) {
	root, rootID, err := m.activeRoot()
	if err != nil {
		return Lease{}, err
	}
	id, err := newSlotID()
	if err != nil {
		return Lease{}, err
	}
	token, err := state.TokenHex()
	if err != nil {
		return Lease{}, err
	}
	rel := filepath.Join(unboundNamespace, id)
	path := filepath.Join(root, rel)
	release, err := m.holdRootForPath(path)
	if err != nil {
		return Lease{}, err
	}
	defer release()
	identity, _, err := m.createSlotRoot(path, path)
	if err != nil {
		return Lease{}, err
	}
	if err := m.retainLease(id, path); err != nil {
		return Lease{}, err
	}
	se := state.Session{ID: id, SlotID: id, State: "UNBOUND", AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	_, err = m.store.CreateSlotSession(context.Background(), state.Slot{ID: id, RootID: rootID, RelPath: rel, DirIdentity: identity, State: "UNBOUND"}, nil, se, "")
	if err != nil {
		m.releaseLease(id)
		return Lease{}, err
	}
	return Lease{SessionID: id, Token: token, Path: path, RootIdentity: identity}, nil
}
