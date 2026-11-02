package llm

import "testing"

func TestPackageOf(t *testing.T) {
	cases := []struct {
		args string
		want string
	}{
		{`{"name":"ripgrep"}`, "ripgrep"},
		{`{\"name\":\"ripgrep\"}`, "ripgrep"},
		{`name="ripgrep"`, "ripgrep"},
		{`name: ripgrep`, "ripgrep"},
		{`{"name": "rg"}`, "rg"},
		{`{}`, ""},
	}
	for _, c := range cases {
		tc := ToolCall{}
		tc.Func.Arguments = []byte(c.args)
		got, ok := PackageOf(tc)
		if c.want == "" {
			if ok {
				t.Errorf("PackageOf(%q) = %q, want failure", c.args, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("PackageOf(%q) = (%q,%v), want %q", c.args, got, ok, c.want)
		}
	}
}
