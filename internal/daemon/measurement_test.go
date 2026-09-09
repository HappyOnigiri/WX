package daemon

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// 履歴は上限件数だけを新しい順で残し、slot と貸出先 session のどちらからでも引ける。
func TestPrepareMeasurementsKeepRecentRunsAndFilterByTarget(t *testing.T) {
	m := &Manager{}
	for index := range prepareMeasurementHistory + 4 {
		id := strconv.Itoa(index)
		m.recordPrepareMeasurement(PrepareMeasurement{SlotID: "slot-" + id, SessionID: "session-" + id, TotalMS: int64(index)})
	}
	all := m.PrepareMeasurements("", "")
	if len(all) != prepareMeasurementHistory {
		t.Fatalf("measurements=%d, want the history limit %d", len(all), prepareMeasurementHistory)
	}
	newest := strconv.Itoa(prepareMeasurementHistory + 3)
	if all[0].SlotID != "slot-"+newest {
		t.Fatalf("measurements[0]=%+v, want the newest run first", all[0])
	}
	if got := m.PrepareMeasurements("slot-"+newest, ""); len(got) != 1 || got[0].SessionID != "session-"+newest {
		t.Fatalf("by slot=%+v, want only the requested slot", got)
	}
	if got := m.PrepareMeasurements("", "session-"+newest); len(got) != 1 || got[0].SlotID != "slot-"+newest {
		t.Fatalf("by session=%+v, want only the requested session", got)
	}
	if got := m.PrepareMeasurements("", "session-0"); len(got) != 0 {
		t.Fatalf("dropped run=%+v, want nothing beyond the history limit", got)
	}
}

// 下位区間は親の直後へ並べ替える。記録順のままでは、親の計測が終わる前に記録される下位区間が先に現れる。
func TestOrderPreparePhasesPlacesSubPhasesAfterTheirParent(t *testing.T) {
	ordered := orderPreparePhases([]PreparePhase{
		{Name: "checkout"},
		{Name: "cow.compare"},
		{Name: "cow.clone"},
		{Name: "cow"},
		{Name: "ready-lock"},
		{Name: "update.swap"},
	})
	var names []string
	for _, phase := range ordered {
		names = append(names, phase.Name)
	}
	want := []string{"checkout", "cow", "cow.compare", "cow.clone", "ready-lock", "update.swap"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("order=%v, want %v", names, want)
	}
}

// 実際の準備は区間内訳と Early Ready の到達を残し、`wx bench` がその slot の記録を引ける。
func TestPrepareMeasurementRecordsPhasesOfARealPreparation(t *testing.T) {
	f := newReuseStandbyFixture(t)
	standby := f.readyStandby(t)
	measurements := f.manager.PrepareMeasurements(standby.ID, "")
	if len(measurements) != 1 {
		t.Fatalf("measurements=%+v, want one run for the prepared standby", measurements)
	}
	measurement := measurements[0]
	if measurement.Failed || measurement.StartedAt == "" {
		t.Fatalf("measurement=%+v, want a successful run with its start time", measurement)
	}
	recorded := map[string]int{}
	for _, phase := range measurement.Phases {
		recorded[phase.Name] = phase.Count
	}
	// 二段階準備の節目（登録・先行配置・Early Ready の公開・残りの展開・READY への移行）が揃うことを見る。
	for _, name := range []string{"git-register", "early-index", "early-checkout", "early-ready", "checkout", "ready-lock"} {
		if recorded[name] == 0 {
			t.Fatalf("phases=%+v, want %s recorded", measurement.Phases, name)
		}
	}
}

// standby の退役は待機中の READY だけを STALE にし、次の貸出を cold start に戻す。
func TestRetireStandbyStalesWaitingSlotsOnly(t *testing.T) {
	f := newReuseStandbyFixture(t)
	standby := f.readyStandby(t)
	result, err := f.manager.RetireStandby(context.Background(), f.repository)
	if err != nil {
		t.Fatal(err)
	}
	retired, _ := result["retired"].([]string)
	if len(retired) != 1 || retired[0] != standby.ID {
		t.Fatalf("retired=%v, want the waiting standby %s", retired, standby.ID)
	}
	if got := f.slotState(t, standby.ID); got != "STALE" {
		t.Fatalf("standby state=%s, want STALE", got)
	}
	// 退役済みの slot は二度目の要求では対象にならない。
	second, err := f.manager.RetireStandby(context.Background(), f.repository)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := second["retired"].([]string); len(again) != 0 {
		t.Fatalf("retired again=%v, want nothing left to retire", again)
	}
}
