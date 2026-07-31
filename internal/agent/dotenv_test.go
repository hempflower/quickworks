package agent

import "testing"

func TestParseDotenv(t *testing.T) {
	values, err := ParseDotenv("# comment\nDEEPSEEK_API_KEY=secret value\nEMPTY=\n")
	if err != nil {
		t.Fatal(err)
	}
	if values["DEEPSEEK_API_KEY"] != "secret value" || values["EMPTY"] != "" {
		t.Fatalf("unexpected values: %#v", values)
	}
	if _, err := ParseDotenv("export TOKEN=value\n"); err == nil {
		t.Fatal("expected shell syntax rejection")
	}
}
