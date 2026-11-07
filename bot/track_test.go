package bot

import "testing"

func TestWidgetNameDropsTheAttachment(t *testing.T) {
	for in, want := range map[string]string{
		"matrev:ok:1234567890":  "matrev:ok",
		"matrep:rm:42":          "matrep:rm",
		"reg:email":             "reg:email",
		"materials:upload:form": "materials:upload:form",
		"mcp:guide":             "mcp:guide",
	} {
		if got := WidgetName(in); got != want {
			t.Errorf("WidgetName(%q) = %q, want %q", in, got, want)
		}
	}
}
