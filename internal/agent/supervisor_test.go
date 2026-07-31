package agent

import "testing"

func TestMergeEnvironmentOverridesWithoutDuplicates(t *testing.T) {
	merged := mergeEnvironment([]string{"A=one", "B=two"}, map[string]string{"A": "replacement", "C": "three"}, "D=four")
	values := make(map[string]string)
	for _, item := range merged {
		key, value, _ := splitEnvironment(item)
		if _, duplicate := values[key]; duplicate {
			t.Fatalf("duplicate key %q", key)
		}
		values[key] = value
	}
	if values["A"] != "replacement" || values["B"] != "two" || values["C"] != "three" || values["D"] != "four" {
		t.Fatalf("unexpected environment: %#v", values)
	}
}

func splitEnvironment(item string) (string, string, bool) {
	for index, character := range item {
		if character == '=' {
			return item[:index], item[index+1:], true
		}
	}
	return "", "", false
}
