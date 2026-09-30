package pluginkit

import "testing"

// The control panel re-serialises the plugin config block with 4-space
// indentation and quoted keys. Parsing that shape used to fail: keys kept their
// quotes, so a lookup for pat missed the stored key and every setting silently
// fell back to its default.
func TestParseConfigHandlesControlPanelSerialisation(t *testing.T) {
	raw := []byte("\"enabled\": true\n" +
		"\"priority\": 10\n" +
		"\"pat\": \"pt-secret\"\n" +
		"\"region\": \"china\"\n")
	cfg, errParse := ParseConfig(raw)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if got := cfg.String("pat", ""); got != "pt-secret" {
		t.Fatalf("pat = %q, want pt-secret (the quoted key was likely not unquoted)", got)
	}
	if got := cfg.String("region", ""); got != "china" {
		t.Fatalf("region = %q, want china", got)
	}
	if !cfg.Bool("enabled", false) {
		t.Fatal("enabled should be true")
	}
}

// TestParseConfigHandlesTwoSpaceIndentation checks the writer's own output.
func TestParseConfigHandlesTwoSpaceIndentation(t *testing.T) {
	cfg, errParse := ParseConfig([]byte("pat: pt-a\nregion: global\n"))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if got := cfg.String("pat", ""); got != "pt-a" {
		t.Fatalf("pat = %q", got)
	}
	if got := cfg.String("region", ""); got != "global" {
		t.Fatalf("region = %q", got)
	}
}

// TestParseConfigIgnoresCommentsAndDeeperNesting makes sure a deeper mapping is
// skipped rather than mis-attributed to the parent level.
func TestParseConfigIgnoresCommentsAndDeeperNesting(t *testing.T) {
	raw := []byte("enabled: true\n" +
		"# a comment\n" +
		"\n" +
		"nested:\n" +
		"    keep: yes\n" +
		"        deeper: no\n")
	cfg, errParse := ParseConfig(raw)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if !cfg.Bool("enabled", false) {
		t.Fatal("enabled should survive comments and blank lines")
	}
	if got := cfg.NestedString("nested", "keep", ""); got != "yes" {
		t.Fatalf("nested.keep = %q", got)
	}
	if got := cfg.NestedString("nested", "deeper", ""); got != "" {
		t.Fatalf("a deeper key must not be attributed to the parent level, got %q", got)
	}
}
