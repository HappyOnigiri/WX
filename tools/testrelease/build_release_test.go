package testrelease

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const fakeGo = `#!/bin/sh
printf '%s\n' "$CGO_ENABLED/$GOOS/$GOARCH" > "$FAKE_GO_LOG"
printf '%s\n' "$@" >> "$FAKE_GO_LOG"
[ "${FAKE_GO_FAIL:-0}" = 0 ] || exit 23
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then shift; cp "$FAKE_BINARY" "$1"; exit 0; fi
  shift
done
exit 64
`

func TestReleaseBuildProducesWorkflowAssets(t *testing.T) {
	t.Parallel()
	f := newInstallFixture(t)
	destination := filepath.Join(f.root, "release output")
	output, err := runReleaseBuild(t, f, "v1.2.3", destination)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	log := readFile(t, filepath.Join(f.root, "go.log"))
	for _, expected := range []string{
		"0/darwin/arm64\n", "-trimpath\n", "internal/version.Version=v1.2.3", "internal/version.BuildMeta=\n",
	} {
		if !strings.Contains(log, expected) {
			t.Fatalf("build arguments do not contain %q: %s", expected, log)
		}
	}
	wf := readPublishWorkflow(t)
	var publish workflowStep
	for _, step := range wf.Jobs["publish"].Steps {
		if strings.HasPrefix(step.Uses, "HappyOnigiri/ReleaseActions/actions/publish-release@") {
			publish = step
		}
	}
	// 期待する集合そのものを書く。件数だけを見ると、YAML の読み取りが壊れて空になった場合を検出できない。
	assets := strings.Fields(publish.With["assets"])
	want := []string{"install.sh", "uninstall.sh", "wx-darwin-arm64", "checksums.txt"}
	names := make([]string, 0, len(assets))
	for _, asset := range assets {
		names = append(names, filepath.Base(asset))
	}
	slices.Sort(names)
	if wanted := slices.Sorted(slices.Values(want)); !slices.Equal(names, wanted) {
		t.Fatalf("workflow assets=%v, want %v", names, wanted)
	}
	for _, asset := range assets {
		if _, err := os.Stat(filepath.Join(destination, filepath.Base(asset))); err != nil {
			t.Fatalf("workflow asset %s is not produced: %v", asset, err)
		}
	}
	digest := sha256.Sum256([]byte(readFile(t, filepath.Join(destination, "wx-darwin-arm64"))))
	if got := readFile(t, filepath.Join(destination, "checksums.txt")); got != fmt.Sprintf("%x  wx-darwin-arm64\n", digest) {
		t.Fatalf("checksum mismatch: %q", got)
	}
	f.installer = readFile(t, filepath.Join(destination, "install.sh"))
	if strings.Contains(f.installer, "@WX_RELEASE_VERSION@") {
		t.Fatal("installer contains an unresolved version")
	}
	f.assets = destination
	if output, err := f.run(t); err != nil {
		t.Fatalf("install generated assets: %v\n%s", err, output)
	}
}

func TestReleaseBuildRejectsMissingOrInvalidVersion(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"", "main", "1.2.3", "v01.2.3", "v1.2.3-dev", "v1.2.3; touch unwanted"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			f := newInstallFixture(t)
			destination := filepath.Join(f.root, "output")
			output, err := runReleaseBuild(t, f, version, destination)
			if err == nil || !strings.Contains(output, "RELEASE_VERSION must be vX.Y.Z") {
				t.Fatalf("invalid version: %v\n%s", err, output)
			}
			if _, err := os.Stat(filepath.Join(f.root, "go.log")); !os.IsNotExist(err) {
				t.Fatalf("Go ran before version validation: %v", err)
			}
		})
	}
}

func TestReleaseBuildFailureDoesNotPublishStaleArtifacts(t *testing.T) {
	t.Parallel()
	f := newInstallFixture(t)
	destination := filepath.Join(f.root, "output")
	output, err := runReleaseBuild(t, f, "v1.2.3", destination, "FAKE_GO_FAIL=1")
	if err == nil {
		t.Fatalf("build unexpectedly succeeded: %s", output)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed build published artifacts: %v", err)
	}
}

func runReleaseBuild(t *testing.T, f installFixture, version, destination string, environment ...string) (string, error) {
	t.Helper()
	goPath := filepath.Join(f.bin, "go")
	writeFile(t, goPath, fakeGo, 0o755)
	command := exec.Command("make", "release")
	command.Dir = "../.."
	command.Env = append([]string{
		"PATH=/usr/bin:/bin", "GO=" + goPath,
		"RELEASE_VERSION=" + version, "RELEASE_DIR=" + destination,
		"FAKE_GO_LOG=" + filepath.Join(f.root, "go.log"),
		"FAKE_BINARY=" + filepath.Join(f.assets, "wx-darwin-arm64"),
		"TMPDIR=" + f.root,
	}, environment...)
	output, err := command.CombinedOutput()
	return string(output), err
}

type workflowStep struct {
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

type publishWorkflow struct {
	Jobs map[string]struct {
		Steps       []workflowStep `yaml:"steps"`
		Concurrency struct {
			Group  string `yaml:"group"`
			Cancel bool   `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
	} `yaml:"jobs"`
}

func readPublishWorkflow(t *testing.T) publishWorkflow {
	t.Helper()
	var wf publishWorkflow
	if err := yaml.Unmarshal([]byte(readFile(t, "../../.github/workflows/publish-release.yml")), &wf); err != nil {
		t.Fatal(err)
	}
	return wf
}

func TestReleaseWorkflowBuildsAndTagsTheSameCommit(t *testing.T) {
	t.Parallel()
	wf := readPublishWorkflow(t)
	job := wf.Jobs["publish"]
	var checkout, build, publish int = -1, -1, -1
	const commit = "${{ github.event.pull_request.merge_commit_sha }}"
	for i, step := range job.Steps {
		switch {
		case strings.HasPrefix(step.Uses, "actions/checkout@"):
			checkout = i
			if step.With["ref"] != commit {
				t.Fatalf("checkout must pin the merge commit: %+v", step.With)
			}
		case strings.Contains(step.Run, "make release"):
			build = i
			if step.Env["RELEASE_BRANCH"] != "${{ github.event.pull_request.head.ref }}" {
				t.Fatal("build version is not derived from the release branch")
			}
		case strings.HasPrefix(step.Uses, "HappyOnigiri/ReleaseActions/actions/publish-release@"):
			publish = i
			if step.With["target-commit"] != commit {
				t.Fatal("tag does not match the build commit")
			}
		}
	}
	if checkout < 0 || build <= checkout || publish <= build {
		t.Fatalf("invalid release order: checkout=%d build=%d publish=%d", checkout, build, publish)
	}
	if job.Concurrency.Group == "" || job.Concurrency.Cancel {
		t.Fatal("publishing must be serialized without cancellation")
	}
}
