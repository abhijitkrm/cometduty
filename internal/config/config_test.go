package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseYAML = `
enable_dashboard: true
listen_port: 8080
node_down_alert_minutes: 5
prometheus_enabled: true
prometheus_listen_port: 28660
chains:
  cosmoshub:
    chain_id: cosmoshub-4
    valoper_address: cosmosvaloper1abcdef
    nodes:
      - url: https://rpc.cosmos.example
        alert_if_down: true
    alerts:
      consecutive_enabled: true
      consecutive_missed: 5
`

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	p := writeTemp(t, dir, "config.yml", baseYAML)
	c, err := Load(p, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !c.EnableDashboard || c.ListenPort != 8080 {
		t.Errorf("dashboard fields: %+v", c)
	}
	ch := c.Chains["cosmoshub"]
	if ch == nil {
		t.Fatal("chain not loaded")
	}
	if ch.Name != "cosmoshub" {
		t.Errorf("chain name not populated: %q", ch.Name)
	}
	if ch.ChainID != "cosmoshub-4" {
		t.Errorf("chain_id: %q", ch.ChainID)
	}
	if !ch.Alerts.ConsecutiveEnabled || ch.Alerts.ConsecutiveMissed != 5 {
		t.Errorf("alerts: %+v", ch.Alerts)
	}
	if fatal, probs := Validate(c); fatal || len(probs) > 0 {
		t.Errorf("valid config produced problems: %v", probs)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("COMETDUTY_TEST_RPC", "https://expanded.example:443")
	dir := t.TempDir()
	p := writeTemp(t, dir, "config.yml", `
chains:
  test:
    chain_id: test-1
    valcons_override: ABCDEF
    nodes:
      - url: ${COMETDUTY_TEST_RPC}
`)
	c, err := Load(p, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Chains["test"].Nodes[0].URL; got != "https://expanded.example:443" {
		t.Errorf("env not expanded: %q", got)
	}
}

func TestChainDirMerge(t *testing.T) {
	dir := t.TempDir()
	chainDir := filepath.Join(dir, "chains.d")
	if err := os.Mkdir(chainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTemp(t, chainDir, "Osmosis.yml", `
chain_id: osmosis-1
valoper_address: osmovaloper1xyz
nodes:
  - url: https://rpc.osmo.example
`)
	writeTemp(t, chainDir, "With.Dots.yaml", `# interior dots stay in the name
chain_id: dots-1
valcons_override: ABCD
nodes:
  - url: https://x
`)
	writeTemp(t, chainDir, "ignored.txt", "not yaml")
	main := writeTemp(t, dir, "config.yml", "chains: {}\n")

	c, err := Load(main, chainDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Chains["Osmosis"] == nil || c.Chains["Osmosis"].ChainID != "osmosis-1" {
		t.Errorf("chains.d merge failed: %+v", c.Chains)
	}
	if c.Chains["With.Dots"] == nil {
		t.Errorf("interior-dot name mangled: %+v", c.Chains)
	}
	if _, ok := c.Chains["ignored"]; ok {
		t.Error("non-yaml file merged")
	}
}

func TestValidateProblems(t *testing.T) {
	// empty config → fatal (no chains)
	if fatal, _ := Validate(&Config{}); !fatal {
		t.Error("empty config not fatal")
	}
	// bad port → fatal
	c := &Config{EnableDashboard: true, ListenPort: 99999, Chains: map[string]*ChainConfig{
		"x": {ChainID: "x-1", ValoperAddress: "v"},
	}}
	if fatal, _ := Validate(c); !fatal {
		t.Error("bad port not fatal")
	}
	// webhook enabled without url → fatal
	c = &Config{Webhook: WebhookConfig{Enabled: true}, Chains: map[string]*ChainConfig{
		"x": {ChainID: "x-1", ValoperAddress: "v"},
	}}
	if fatal, _ := Validate(c); !fatal {
		t.Error("webhook without url not fatal")
	}
	// pagerduty oauth-looking key → fatal
	c = &Config{Pagerduty: PagerdutyConfig{Enabled: true, APIKey: "pdus+oauth_token"}, Chains: map[string]*ChainConfig{
		"x": {ChainID: "x-1", ValoperAddress: "v"},
	}}
	if fatal, _ := Validate(c); !fatal {
		t.Error("oauth-style pd key not fatal")
	}
	// chain with validator missing address → fatal
	c = &Config{Chains: map[string]*ChainConfig{
		"x": {ChainID: "x-1", Validators: []ValidatorConfig{{Label: "nothing"}}},
	}}
	if fatal, _ := Validate(c); !fatal {
		t.Error("validator without address not fatal")
	}
}

func TestValidatorTargets(t *testing.T) {
	cc := &ChainConfig{
		ValoperAddress: "valoper1",
		Validators: []ValidatorConfig{
			{ValoperAddress: "valoper2"},
			{ValconsOverride: "ABCD"},
		},
	}
	targets := cc.ValidatorTargets()
	if len(targets) != 3 {
		t.Fatalf("got %d targets", len(targets))
	}
	if targets[0].ValoperAddress != "valoper1" {
		t.Error("single-validator shorthand not first")
	}
	// empty shorthand not counted
	cc2 := &ChainConfig{}
	if len(cc2.ValidatorTargets()) != 0 {
		t.Error("empty chain produced targets")
	}
}

func TestAgeRoundTrip(t *testing.T) {
	plain := []byte("secret: config\nwith: data\n")
	enc, err := EncryptAge(plain, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(enc), "age-encryption.org/v1") {
		t.Error("output missing age marker")
	}
	dec, err := DecryptAge(enc, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if string(dec) != string(plain) {
		t.Error("roundtrip mismatch")
	}
	if _, err := DecryptAge(enc, "wrong"); err == nil {
		t.Error("wrong password decrypted")
	}
}

func TestLoadEncryptedFile(t *testing.T) {
	dir := t.TempDir()
	enc, err := EncryptAge([]byte(baseYAML), "pw")
	if err != nil {
		t.Fatal(err)
	}
	p := writeTemp(t, dir, "config.yml.age", string(enc))
	if _, err := Load(p, "", ""); err == nil {
		t.Error("encrypted config loaded without password")
	}
	c, err := Load(p, "", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if c.Chains["cosmoshub"] == nil {
		t.Error("decrypted config missing chain")
	}
}

func TestLoadRemoteRequiresPassword(t *testing.T) {
	if _, err := Load("https://example.com/config.yml", "", ""); err == nil {
		t.Error("remote load without password allowed")
	}
}
