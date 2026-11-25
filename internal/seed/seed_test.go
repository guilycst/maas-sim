package seed

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sample = `schemaVersion: 1
domain: lab.test
zones: [az1]
resourcePools: [lab]
tags: [worker]
machines:
  - hostname: LAB-NODE-01
    zone: az1
    resourcePool: lab
    tags: [worker]
    cpuCount: 4
    memoryMiB: 8192
    diskGiB: 80
`

func TestLoadYAMLAndJSON(t *testing.T) {
	yaml := loadTestSeed(t, "seed.yaml", sample)
	json := loadTestSeed(t, "seed.json", `{
		"schemaVersion": 1,
		"domain": "lab.test",
		"zones": ["az1"],
		"resourcePools": ["lab"],
		"tags": ["worker"],
		"machines": [{
			"hostname": "lab-node-01",
			"zone": "az1",
			"resourcePool": "lab",
			"tags": ["worker"],
			"cpuCount": 4,
			"memoryMiB": 8192,
			"diskGiB": 80
		}]
	}`)
	if !reflect.DeepEqual(yaml, json) {
		t.Fatalf("YAML and JSON inventories differ: %#v versus %#v", yaml, json)
	}
	if machine := yaml.Machines[0]; machine.SystemID != "ba6f8fd121d8" || machine.Hostname != "lab-node-01" {
		t.Fatalf("normalized machine = %#v", machine)
	}

	changed := strings.ReplaceAll(sample, "lab.test", "other.test")
	changed = strings.ReplaceAll(changed, "az1", "az2")
	changed = strings.ReplaceAll(changed, "worker", "other")
	if id := loadTestSeed(t, "renamed-groups.yaml", changed).Machines[0].SystemID; id != "ba6f8fd121d8" {
		t.Fatalf("changed groups changed system ID to %s", id)
	}
}

func TestLoadExplicitIDAndDefaults(t *testing.T) {
	inventory := loadTestSeed(t, "seed.yaml", `schemaVersion: 1
machines:
  - systemID: lab001
    hostname: renamed-machine
    cpuCount: 4
    memoryMiB: 8192
    diskGiB: 80
`)
	machine := inventory.Machines[0]
	if machine.SystemID != "lab001" || machine.Zone != "default" || machine.ResourcePool != "default" {
		t.Fatalf("unexpected machine defaults: %+v", machine)
	}
	if empty := loadTestSeed(t, "empty.yaml", "schemaVersion: 1\nmachines: []\n"); len(empty.Machines) != 0 {
		t.Fatalf("empty seed loaded %d machines", len(empty.Machines))
	}
}

func TestRejectOverlongMachineFQDN(t *testing.T) {
	file := filepath.Join(t.TempDir(), "seed.yaml")
	longDomain := strings.Repeat("a.", 124) + "test"
	content := strings.Replace(sample, "lab.test", longDomain, 1)
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("overlong machine FQDN returned %v", err)
	}
}

func TestRejectInvalidSeed(t *testing.T) {
	for _, tc := range []struct{ name, contents, want string }{
		{"missing version", "machines: []", "schemaVersion"},
		{"unknown version", "schemaVersion: 2\nmachines: []", "schemaVersion"},
		{"missing machines", "schemaVersion: 1", "machines"},
		{"unknown field", "schemaVersion: 1\nmachines: []\nextra: 1", "field extra"},
		{"duplicate field", "schemaVersion: 1\nmachines: []\nmachines: []", "machines at line 3: duplicate"},
		{"second document", "schemaVersion: 1\nmachines: []\n---\n{}", "exactly one document"},
		{"unknown zone", strings.Replace(sample, "zone: az1", "zone: missing", 1), "machines[0].zone"},
		{"unknown tag", strings.Replace(sample, "tags: [worker]", "tags: []", 1), "machines[0].tags[0]"},
		{"explicit null ID", strings.Replace(sample, "- hostname:", "- systemID: null\n    hostname:", 1), "machines[0].systemID"},
		{"nonpositive capacity", strings.Replace(sample, "cpuCount: 4", "cpuCount: 0", 1), "machines[0].cpuCount"},
		{"fractional capacity", strings.Replace(sample, "diskGiB: 80", "diskGiB: 80.1", 1), "machines[0].diskGiB"},
		{"generated ID collision", sample + `  - systemID: ba6f8fd121d8
    hostname: lab-node-02
    zone: az1
    resourcePool: lab
    cpuCount: 4
    memoryMiB: 8192
    diskGiB: 80
`, "machines[1].systemID duplicates machines[0].systemID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestSeed(t, "fixture.yaml", tc.contents)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("Load error = %v, want file and %q", err, tc.want)
			}
		})
	}
}

func loadTestSeed(t *testing.T, name, data string) Inventory {
	t.Helper()
	inventory, err := Load(writeTestSeed(t, name, data))
	if err != nil {
		t.Fatal(err)
	}
	return inventory
}

func writeTestSeed(t *testing.T, name, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
