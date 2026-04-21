package updater

import (
	"strings"
	"testing"

	"github.com/herbertgao/group-limit-bot/internal/version"
)

func TestCompareVersion(t *testing.T) {
	u := NewUpdater()
	cases := []struct {
		current, latest string
		wantNewer       bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"0.1.0", "v0.1.1", true},
		{"v1.0.0", "v1.0.0", false},
		{"v2.0.0", "v1.9.9", false},
		{"v1.2", "v1.2.1", true},
		{"v1.2.3", "v1.2", false},
		{"dev", "v0.1.0", true},          // "dev" parses to empty → 0.0.0; real tag wins.
		{"v0.1.0-rc1", "v0.1.0", false},  // rc1 parses to [0,1,0], equal.
		{"v1.2.3-rc1", "v1.2.2", false},  // current 1.2.3-rc1 > 1.2.2; reviewer regression.
		{"v1.2.2", "v1.2.3-rc1", true},
		{"v1.2.3-rc1", "v1.2.3", false},  // rc1 == release under this simplified scheme.
	}
	for _, tc := range cases {
		got := u.CompareVersion(tc.current, tc.latest)
		if got != tc.wantNewer {
			t.Errorf("CompareVersion(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.wantNewer)
		}
	}
}

// TestUpdate_RefusesDevBuild verifies that a non-release build (where the
// injected version has no numeric components) short-circuits before any
// network call, preventing an accidental downgrade to the latest release.
func TestUpdate_RefusesDevBuild(t *testing.T) {
	orig := version.Version
	version.Version = "dev-abc1234"
	t.Cleanup(func() { version.Version = orig })

	err := NewUpdater().Update()
	if err == nil {
		t.Fatal("expected Update to refuse dev builds, got nil error")
	}
	if !strings.Contains(err.Error(), "not a tagged release") {
		t.Errorf("error should mention 'not a tagged release': %v", err)
	}
}

func TestParseVersionParts(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"1.2.3", []int{1, 2, 3}},
		{"0.1.0", []int{0, 1, 0}},
		{"1.2", []int{1, 2}},
		{"1.2.3-rc1", []int{1, 2, 3}}, // prerelease suffix "-rc1" is stripped per-component.
		{"1.2.3-alpha.4", []int{1, 2, 3, 4}}, // continuing parts still parse leading digits.
		{"dev-4f650dc", []int{}}, // no numeric head → empty (blocks auto-downgrade).
		{"abc", []int{}},
		{"", []int{}},
	}
	for _, tc := range cases {
		got := parseVersionParts(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseVersionParts(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseVersionParts(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}
