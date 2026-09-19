package cli

import (
	"context"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

// discovery timeout が未設定でも、repository discovery の既定予算を維持する。
func TestDiscoveryTimeoutUsesDefaultAtZero(t *testing.T) {
	client := Client{Config: config.Defaults()}
	client.Config.System.Discovery.Timeout.Duration = 0

	if got, want := client.discoveryTimeout(), defaultDiscoveryBudget+10*time.Second; got != want {
		t.Fatalf("discovery timeout=%s, want %s for a zero configured timeout", got, want)
	}
}

// resume の readiness timeout が 0 のときは、無期限指定として discovery の予算へ戻す。
// 0 の context で Resume を送ると、貸出前に client が期限切れになり、正常な再開を拒否してしまう。
func TestLaunchResumeWithZeroReadinessTimeoutUsesDiscoveryBudget(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.RepositoryDefaults.Readiness.Timeout = &config.Duration{}
	handler := &resumeLaunchHandler{lease: daemon.Lease{SessionID: "resume-session", Token: "resume-token", Path: root, Ready: true}}
	client, stop := serveResumeLaunchRPCWithConfig(t, handler, cfg)
	defer stop()

	exit, relaunch := client.launch(context.Background(), launchPlan{
		agent:    "true",
		cwd:      root,
		resuming: true,
		target:   resumeTarget{WXSessionID: "wx-session"},
	})
	if exit != 0 || relaunch != nil {
		t.Fatalf("launch with zero readiness timeout: exit=%d relaunch=%v", exit, relaunch)
	}
	if methods := handler.methodsSnapshot(); !containsMethod(methods, "Resume") {
		t.Fatalf("methods=%v, want Resume to reach the daemon", methods)
	}
}

// 12 桁ちょうどの OID はそのまま表示し、13 桁以上だけを短縮する。
func TestShortOIDKeepsExactlyTwelveCharacters(t *testing.T) {
	for _, tc := range []struct {
		name, oid, want string
	}{
		{name: "shorter", oid: "abcdef", want: "abcdef"},
		{name: "exact", oid: "abcdefghijkl", want: "abcdefghijkl"},
		{name: "longer", oid: "abcdefghijklmn", want: "abcdefghijkl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortOID(tc.oid); got != tc.want {
				t.Fatalf("shortOID(%q)=%q, want %q", tc.oid, got, tc.want)
			}
		})
	}
}

// 対象が空でも複数 repository の区間は workspace と位置を表示する。
func TestLeasePhaseTextLabelsWorkspaceAtTwoRepositories(t *testing.T) {
	localizer := i18n.New(string(i18n.English))
	phase := leasePhase{name: "checkout", index: 1, total: 2}

	if got, want := phase.text(localizer), "workspace (1/2) checkout"; got != want {
		t.Fatalf("lease phase text=%q, want %q", got, want)
	}
}

// repository が1件だけなら、位置の表記を付けずに区間名を表示する。
func TestLeasePhaseTextOmitsPositionForSingleRepository(t *testing.T) {
	localizer := i18n.New(string(i18n.English))
	phase := leasePhase{name: "checkout", target: "app", index: 1, total: 1}

	if got, want := phase.text(localizer), "app checkout"; got != want {
		t.Fatalf("lease phase text=%q, want %q", got, want)
	}
}
