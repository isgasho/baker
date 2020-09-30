package baker

import (
	"io/ioutil"
	"strings"
	"testing"
)

func TestFillCreateRecordDefault(t *testing.T) {
	tests := []struct {
		name    string
		field   string
		want    byte
		wantErr bool
	}{
		{
			name:  "default",
			field: "",
			want:  DefaultLogLineFieldSeparator,
		},
		{
			name:  "explicit comma",
			field: ",",
			want:  DefaultLogLineFieldSeparator,
		},
		{
			name:  "record separator",
			field: "\u001e",
			want:  0x1e,
		},
		{
			name:  "dot",
			field: ".",
			want:  '.',
		},
		{
			name:    "not ascii",
			field:   "à",
			wantErr: true,
		},
		{
			name:    "2 chars",
			field:   ",,",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				CSV: ConfigCSV{
					FieldSeparator: tt.field,
				},
			}
			err := cfg.fillCreateRecordDefault()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Config.fillCreateRecordDefault() err: %v, wantErr: %v", err, tt.wantErr)
				}
				return
			}

			if sep := cfg.createRecord().(*LogLine).FieldSeparator; sep != tt.want {
				t.Errorf(`got separator "%c" (%v), want "%c" (%v)`, sep, sep, tt.want, tt.want)
			}
		})
	}
}

func TestEnvVarBaseReplace(t *testing.T) {
	src := `
	[general]
	dont_validate_fields = ${DNT_VAL_FIELDS}
	alt_form = "$ALT_FORM"
	unexisting_var = "${THIS_DOESNT_EXIST}"
	`

	want := `
	[general]
	dont_validate_fields = true
	alt_form = "ok"
	unexisting_var = ""
	`

	mapper := func(v string) string {
		switch v {
		case "DNT_VAL_FIELDS":
			return "true"
		case "ALT_FORM":
			return "ok"
		}
		return ""
	}

	s, err := replaceEnvVars(strings.NewReader(src), mapper)
	if err != nil {
		t.Fatalf("replaceEnvVars err: %v", err)
	}
	buf, _ := ioutil.ReadAll(s)

	if want != string(buf) {
		t.Fatalf("wrong toml: %s", string(buf))
	}
}