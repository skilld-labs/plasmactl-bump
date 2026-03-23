package sync

import (
	"os"
	"path/filepath"
	"testing"
)

const testMRN = "interaction__skills__skill-e"

func TestProcessResourcePath(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		wantPlatform string
		wantKind     string
		wantRole     string
		wantErr      bool
	}{
		{
			name:         "valid path",
			path:         "interaction/skills/roles/skill-e/tasks/main.yaml",
			wantPlatform: "interaction",
			wantKind:     "skills",
			wantRole:     "skill-e",
		},
		{
			name:         "valid meta path",
			path:         "platform/applications/roles/app-x/meta/plasma.yaml",
			wantPlatform: "platform",
			wantKind:     "applications",
			wantRole:     "app-x",
		},
		{
			name:    "too short path",
			path:    "interaction/skills/roles",
			wantErr: true,
		},
		{
			name:    "empty string",
			path:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform, kind, role, err := ProcessResourcePath(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if platform != tt.wantPlatform {
				t.Errorf("platform = %q, want %q", platform, tt.wantPlatform)
			}
			if kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", kind, tt.wantKind)
			}
			if role != tt.wantRole {
				t.Errorf("role = %q, want %q", role, tt.wantRole)
			}
		})
	}
}

func TestConvertMRNtoPath(t *testing.T) {
	tests := []struct {
		name    string
		mrn     string
		want    string
		wantErr bool
	}{
		{
			name: "valid MRN",
			mrn:  testMRN,
			want: "interaction/skills/roles/skill-e",
		},
		{
			name:    "too few parts",
			mrn:     "interaction__skills",
			wantErr: true,
		},
		{
			name:    "too many parts",
			mrn:     "interaction__skills__skill-e__extra",
			wantErr: true,
		},
		{
			name:    "empty string",
			mrn:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConvertMRNtoPath(tt.mrn)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetMetaVersion(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]any
		want string
	}{
		{
			name: "normal version",
			meta: map[string]any{
				"plasma": map[string]any{"version": "abc1234567890"},
			},
			want: "abc1234567890",
		},
		{
			name: "propagated version",
			meta: map[string]any{
				"plasma": map[string]any{"version": "ver1-abc1234567890"},
			},
			want: "ver1-abc1234567890",
		},
		{
			name: "version is nil",
			meta: map[string]any{
				"plasma": map[string]any{"version": nil},
			},
			want: "",
		},
		{
			name: "plasma key missing",
			meta: map[string]any{
				"other": "value",
			},
			want: "",
		},
		{
			name: "empty map",
			meta: map[string]any{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetMetaVersion(tt.meta)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func writeMetaFile(t *testing.T, dir, mrn, content string) {
	t.Helper()
	parts := splitMRN(mrn)
	metaDir := filepath.Join(dir, parts[0], parts[1], "roles", parts[2], "meta")
	if err := os.MkdirAll(metaDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "plasma.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func splitMRN(mrn string) []string {
	var parts []string
	cur := ""
	for i := 0; i < len(mrn); i++ {
		if i+1 < len(mrn) && mrn[i] == '_' && mrn[i+1] == '_' {
			parts = append(parts, cur)
			cur = ""
			i++
		} else {
			cur += string(mrn[i])
		}
	}
	parts = append(parts, cur)
	return parts
}

func TestGetBaseVersion(t *testing.T) {
	tests := []struct {
		name         string
		yamlContent  string
		wantBase     string
		wantFull     string
		wantDebugLen int
	}{
		{
			name:        "plain version (no dash)",
			yamlContent: "plasma:\n  version: abc1234567890\n",
			wantBase:    "abc1234567890",
			wantFull:    "abc1234567890",
		},
		{
			name:        "propagated version (one dash)",
			yamlContent: "plasma:\n  version: ver1-abc1234567890\n",
			wantBase:    "ver1",
			wantFull:    "ver1-abc1234567890",
		},
		{
			name:         "malformed version (two dashes)",
			yamlContent:  "plasma:\n  version: ver1-hash1-hash2\n",
			wantBase:     "ver1",
			wantFull:     "ver1-hash1-hash2",
			wantDebugLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			mrn := testMRN
			writeMetaFile(t, dir, mrn, tt.yamlContent)

			r := NewResource(mrn, dir)
			base, full, debug, err := r.GetBaseVersion()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if base != tt.wantBase {
				t.Errorf("base = %q, want %q", base, tt.wantBase)
			}
			if full != tt.wantFull {
				t.Errorf("full = %q, want %q", full, tt.wantFull)
			}
			if len(debug) != tt.wantDebugLen {
				t.Errorf("debug len = %d, want %d: %v", len(debug), tt.wantDebugLen, debug)
			}
		})
	}
}

func TestIsUpdatableKind(t *testing.T) {
	validKinds := []string{
		"applications", "services", "softwares", "executors",
		"flows", "skills", "functions", "libraries", "entities",
	}
	for _, kind := range validKinds {
		if !IsUpdatableKind(kind) {
			t.Errorf("expected %q to be updatable", kind)
		}
	}

	invalidKinds := []string{"group_vars", "roles", "builders", "", "unknown"}
	for _, kind := range invalidKinds {
		if IsUpdatableKind(kind) {
			t.Errorf("expected %q to NOT be updatable", kind)
		}
	}
}

func TestOrderedMap(t *testing.T) {
	t.Run("Set and Get", func(t *testing.T) {
		m := NewOrderedMap[string]()
		m.Set("a", "1")
		m.Set("b", "2")
		m.Set("c", "3")

		v, ok := m.Get("b")
		if !ok || v != "2" {
			t.Errorf("Get(b) = %q, %v; want %q, true", v, ok, "2")
		}
		_, ok = m.Get("missing")
		if ok {
			t.Error("Get(missing) should return false")
		}
	})

	t.Run("preserves insertion order", func(t *testing.T) {
		m := NewOrderedMap[int]()
		keys := []string{"c", "a", "b"}
		for i, k := range keys {
			m.Set(k, i)
		}
		if got := m.Keys(); len(got) != 3 || got[0] != "c" || got[1] != "a" || got[2] != "b" {
			t.Errorf("Keys() = %v, want [c a b]", got)
		}
	})

	t.Run("Set existing key does not duplicate", func(t *testing.T) {
		m := NewOrderedMap[string]()
		m.Set("a", "1")
		m.Set("a", "2")
		if m.Len() != 1 {
			t.Errorf("Len() = %d, want 1", m.Len())
		}
		v, _ := m.Get("a")
		if v != "2" {
			t.Errorf("Get(a) = %q, want %q", v, "2")
		}
	})

	t.Run("Unset removes key", func(t *testing.T) {
		m := NewOrderedMap[string]()
		m.Set("a", "1")
		m.Set("b", "2")
		m.Unset("a")
		if m.Len() != 1 {
			t.Errorf("Len() = %d, want 1", m.Len())
		}
		if _, ok := m.Get("a"); ok {
			t.Error("Get(a) should return false after Unset")
		}
	})

	t.Run("Unset non-existing key is no-op", func(t *testing.T) {
		m := NewOrderedMap[string]()
		m.Set("a", "1")
		m.Unset("missing") // must not panic
		if m.Len() != 1 {
			t.Errorf("Len() = %d, want 1", m.Len())
		}
	})

	t.Run("OrderBy reorders keys", func(t *testing.T) {
		m := NewOrderedMap[int]()
		m.Set("c", 3)
		m.Set("a", 1)
		m.Set("b", 2)
		m.OrderBy([]string{"a", "b", "c"})
		if got := m.Keys(); len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
			t.Errorf("Keys() after OrderBy = %v, want [a b c]", got)
		}
	})

	t.Run("SortKeysAlphabetically", func(t *testing.T) {
		m := NewOrderedMap[int]()
		m.Set("z", 3)
		m.Set("a", 1)
		m.Set("m", 2)
		m.SortKeysAlphabetically()
		if got := m.Keys(); got[0] != "a" || got[1] != "m" || got[2] != "z" {
			t.Errorf("Keys() after SortKeysAlphabetically = %v, want [a m z]", got)
		}
	})
}
