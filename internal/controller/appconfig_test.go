package controller

import (
	"strings"
	"testing"
)

func TestMergeAppConfig(t *testing.T) {
	tests := []struct {
		name, live, desired string
		wantChanged         bool
		wantContains        []string
		wantAbsent          []string
	}{
		{"no live config", "", `{"a":1}`, true, []string{`"a": 1`}, nil},
		{"null live config", "null", `{"a":1}`, true, []string{`"a": 1`}, nil},
		{"keeps unmanaged keys", `{"a":1,"keep":"me","nested":{"x":[1,2]}}`, `{"a":2}`, true,
			[]string{`"a": 2`, `"keep": "me"`, `"x": [`}, nil},
		{"equal value", `{"a":1,"b":"x"}`, `{"a":1}`, false, nil, nil},
		{"numbers compare by value", `{"ttl":30}`, `{"ttl":30.0}`, false, nil, nil},
		{"windows newlines in live", "{\r\n  \"a\": true\r\n}", `{"a":true}`, false, nil, nil},
		{"nested object differs", `{"o":{"x":1,"y":2}}`, `{"o":{"x":1}}`, true, []string{`"o": {`}, []string{`"y"`}},
		{"array order matters", `{"l":[1,2]}`, `{"l":[2,1]}`, true, nil, nil},
		{"type differs", `{"a":"1"}`, `{"a":1}`, true, []string{`"a": 1`}, nil},
		{"html is not escaped", `{}`, `{"a":"<b>&"}`, true, []string{`"<b>&"`}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			merged, changed, err := mergeAppConfig(tc.live, []byte(tc.desired))
			if err != nil {
				t.Fatal(err)
			}
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v (merged %s)", changed, tc.wantChanged, merged)
			}
			if !changed && merged != tc.live {
				t.Fatalf("unchanged config was rewritten: %q", merged)
			}
			for _, s := range tc.wantContains {
				if !strings.Contains(merged, s) {
					t.Errorf("merged lacks %q:\n%s", s, merged)
				}
			}
			for _, s := range tc.wantAbsent {
				if strings.Contains(merged, s) {
					t.Errorf("merged contains %q:\n%s", s, merged)
				}
			}
		})
	}
}

func TestMergeAppConfigRejectsNonObjects(t *testing.T) {
	if _, _, err := mergeAppConfig(`[1]`, []byte(`{"a":1}`)); err == nil {
		t.Fatal("live array accepted")
	}
	if _, _, err := mergeAppConfig(`{}`, []byte(`[1]`)); err == nil {
		t.Fatal("spec array accepted")
	}
	if _, _, err := mergeAppConfig(`{not json`, []byte(`{}`)); err == nil {
		t.Fatal("broken live config accepted")
	}
}
