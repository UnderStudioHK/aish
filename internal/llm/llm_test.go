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

func TestDecodeArgs(t *testing.T) {
	type args struct {
		Name string `json:"name"`
	}
	cases := []string{
		`{"name":"x"}`,
		`"{\"name\":\"x\"}"`,
	}
	for _, raw := range cases {
		var a args
		tc := ToolCall{}
		tc.Func.Arguments = []byte(raw)
		if err := DecodeArgs(tc.Func.Arguments, &a); err != nil {
			t.Errorf("DecodeArgs(%q): %v", raw, err)
		} else if a.Name != "x" {
			t.Errorf("DecodeArgs(%q) name = %q", raw, a.Name)
		}
	}
}
