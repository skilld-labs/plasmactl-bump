package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/launchrctl/launchr"
)

// mkfile creates file with content, including parent dirs.
func mkfile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newTestInventory builds an Inventory from a temp dir with CalculateVariablesUsage already called.
func newTestInventory(t *testing.T, dir, vaultPass string) *Inventory {
	t.Helper()
	inv, err := NewInventory(dir, launchr.Log())
	if err != nil {
		t.Fatalf("NewInventory: %v", err)
	}
	if err = inv.CalculateVariablesUsage(vaultPass); err != nil {
		t.Fatalf("CalculateVariablesUsage: %v", err)
	}
	return inv
}

func TestCalculateVariablesUsage_VarUsedInTemplate(t *testing.T) {
	dir := t.TempDir()

	// plain var
	mkfile(t, dir, "interaction/group_vars/all/vars.yaml", "plain_var:\n  value: hello\n")
	// resource meta (needed for inventory init)
	mkfile(t, dir, "interaction/skills/roles/skill-a/meta/plasma.yaml", "plasma:\n  version: ver1\n")
	// template referencing plain_var
	mkfile(t, dir, "interaction/skills/roles/skill-a/templates/config.j2", "value: {{ plain_var.value }}\n")

	inv := newTestInventory(t, dir, "")

	if !inv.IsUsedVariable(false, "plain_var", "interaction") {
		t.Fatal("plain_var should be detected as used by skill-a template")
	}

	resources := inv.GetVariableResources("plain_var", "interaction")
	if len(resources) != 1 || resources[0] != "interaction__skills__skill-a" {
		t.Errorf("GetVariableResources = %v, want [interaction__skills__skill-a]", resources)
	}
}

func TestCalculateVariablesUsage_VarUsedInConfiguration(t *testing.T) {
	dir := t.TempDir()

	mkfile(t, dir, "interaction/group_vars/all/vars.yaml", "cfg_var:\n  value: hello\n")
	mkfile(t, dir, "interaction/skills/roles/skill-b/meta/plasma.yaml", "plasma:\n  version: ver1\n")
	// configuration.yaml referencing cfg_var
	mkfile(t, dir, "interaction/skills/roles/skill-b/tasks/configuration.yaml", "- name: set\n  set_fact:\n    x: \"{{ cfg_var.value }}\"\n")

	inv := newTestInventory(t, dir, "")

	if !inv.IsUsedVariable(false, "cfg_var", "interaction") {
		t.Fatal("cfg_var should be detected via tasks/configuration.yaml")
	}
	resources := inv.GetVariableResources("cfg_var", "interaction")
	if len(resources) != 1 || resources[0] != "interaction__skills__skill-b" {
		t.Errorf("GetVariableResources = %v, want [interaction__skills__skill-b]", resources)
	}
}

func TestCalculateVariablesUsage_VarNotUsed(t *testing.T) {
	dir := t.TempDir()

	mkfile(t, dir, "interaction/group_vars/all/vars.yaml", "unused_var:\n  value: hello\n")
	mkfile(t, dir, "interaction/skills/roles/skill-a/meta/plasma.yaml", "plasma:\n  version: ver1\n")
	// template does NOT reference unused_var
	mkfile(t, dir, "interaction/skills/roles/skill-a/templates/config.j2", "value: static\n")

	inv := newTestInventory(t, dir, "")

	if inv.IsUsedVariable(false, "unused_var", "interaction") {
		t.Fatal("unused_var should NOT be detected as used")
	}
	resources := inv.GetVariableResources("unused_var", "interaction")
	if len(resources) != 0 {
		t.Errorf("expected no resources, got %v", resources)
	}
}

func TestCalculateVariablesUsage_VarToVarDependency(t *testing.T) {
	dir := t.TempDir()

	// Real-world pattern: vars reference other vars as "{{ other_var }}" (no dot suffix).
	// e.g. account_api_password: "{{ account_admin_password }}"
	// Note: "{{ base_var.field }}" is NOT detected — findDependencies matches " key " with
	// spaces on both sides, so a dot following the name breaks detection.
	mkfile(t, dir, "interaction/group_vars/all/vars.yaml",
		"base_var: secret\nderived_var: \"{{ base_var }}\"\n")
	mkfile(t, dir, "interaction/skills/roles/skill-a/meta/plasma.yaml", "plasma:\n  version: ver1\n")
	mkfile(t, dir, "interaction/skills/roles/skill-a/templates/config.j2", "x: {{ derived_var.value }}\n")

	inv := newTestInventory(t, dir, "")

	// derived_var directly used in template
	if !inv.IsUsedVariable(false, "derived_var", "interaction") {
		t.Fatal("derived_var should be used")
	}
	// base_var used transitively via derived_var
	if !inv.IsUsedVariable(false, "base_var", "interaction") {
		t.Fatal("base_var should be used transitively through derived_var")
	}
}

func TestCalculateVariablesUsage_MultipleResourcesDependOnSameVar(t *testing.T) {
	dir := t.TempDir()

	mkfile(t, dir, "interaction/group_vars/all/vars.yaml", "shared_var:\n  value: x\n")
	mkfile(t, dir, "interaction/skills/roles/skill-a/meta/plasma.yaml", "plasma:\n  version: ver1\n")
	mkfile(t, dir, "interaction/skills/roles/skill-a/templates/cfg.j2", "v: {{ shared_var.value }}\n")
	mkfile(t, dir, "interaction/skills/roles/skill-b/meta/plasma.yaml", "plasma:\n  version: ver1\n")
	mkfile(t, dir, "interaction/skills/roles/skill-b/templates/cfg.j2", "v: {{ shared_var.value }}\n")

	inv := newTestInventory(t, dir, "")

	resources := inv.GetVariableResources("shared_var", "interaction")
	if len(resources) != 2 {
		t.Errorf("expected 2 resources, got %v", resources)
	}
}

func TestCalculateVariablesUsage_VaultVar(t *testing.T) {
	dir := t.TempDir()

	// Pre-generated ansible-vault content (password: "test"), decrypts to:
	// vault_secret_var:
	//   value: secret
	mkfile(t, dir, "interaction/group_vars/all/vault.yaml",
		"$ANSIBLE_VAULT;1.1;AES256\n"+
			"39333862393031373163646433343062383363373233323839613561343163666164646334363331\n"+
			"6666616163383131343661616463643138393636393465630a333835653363393963633030613030\n"+
			"66313238353431386237303237623432313264376163633964373063623338363264633730353864\n"+
			"3537323263643562380a373364643434613933313239326436326134303730623963656362653263\n"+
			"61643238303033393733303865306463363032663330333366303265333039653661386664643039\n"+
			"3539336461383032316134663038326262313038393435626663\n")
	mkfile(t, dir, "interaction/skills/roles/skill-a/meta/plasma.yaml", "plasma:\n  version: ver1\n")
	mkfile(t, dir, "interaction/skills/roles/skill-a/templates/secret.j2", "s: {{ vault_secret_var.value }}\n")

	inv := newTestInventory(t, dir, "test")

	if !inv.IsUsedVariable(false, "vault_secret_var", "interaction") {
		t.Fatal("vault_secret_var should be detected as used")
	}
	resources := inv.GetVariableResources("vault_secret_var", "interaction")
	if len(resources) != 1 || resources[0] != "interaction__skills__skill-a" {
		t.Errorf("GetVariableResources = %v, want [interaction__skills__skill-a]", resources)
	}
}

func TestCalculateVariablesUsage_VaultWrongPassword(t *testing.T) {
	dir := t.TempDir()

	mkfile(t, dir, "interaction/group_vars/all/vault.yaml",
		"$ANSIBLE_VAULT;1.1;AES256\n"+
			"39333862393031373163646433343062383363373233323839613561343163666164646334363331\n"+
			"6666616163383131343661616463643138393636393465630a333835653363393963633030613030\n"+
			"66313238353431386237303237623432313264376163633964373063623338363264633730353864\n"+
			"3537323263643562380a373364643434613933313239326436326134303730623963656362653263\n"+
			"61643238303033393733303865306463363032663330333366303265333039653661386664643039\n"+
			"3539336461383032316134663038326262313038393435626663\n")

	_, err := NewInventory(dir, launchr.Log())
	if err != nil {
		t.Fatalf("NewInventory: %v", err)
	}
	inv, _ := NewInventory(dir, launchr.Log())
	err = inv.CalculateVariablesUsage("wrongpassword")
	if err == nil {
		t.Fatal("expected error with wrong vault password, got nil")
	}
}
