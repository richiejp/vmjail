package devcontainer

import (
	"reflect"
	"testing"
)

func TestStripJSONC(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"line comment", "{\n// hi\n\"a\":1}", "{\n\n\"a\":1}"},
		{"block comment", "{/* x */\"a\":1}", "{\"a\":1}"},
		{"trailing comma object", `{"a":1,}`, `{"a":1}`},
		{"trailing comma array", `{"a":[1,2,]}`, `{"a":[1,2]}`},
		{"trailing comma before nested close", `{"a":1, }`, `{"a":1 }`},
		// A // inside a string (e.g. a URL) must survive untouched.
		{"slashes in string", `{"u":"https://x/y"}`, `{"u":"https://x/y"}`},
		// A ",}" inside a string must not lose its comma.
		{"comma-brace in string", `{"s":"a,}"}`, `{"s":"a,}"}`},
		// An escaped quote must not end the string early, so the // after
		// it stays protected.
		{"escaped quote then slashes", `{"s":"x\"//y"}`, `{"s":"x\"//y"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(stripJSONC([]byte(tc.in))); got != tc.want {
				t.Errorf("stripJSONC(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestJSONCRoundTripsThroughParse confirms the stripping yields valid
// JSON the decoder accepts, including a comment-laden block.
func TestJSONCRoundTripsThroughParse(t *testing.T) {
	in := `{
  /* block */ "image": "x", // trailing
  "forwardPorts": [1, 2,],
}`
	c, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Image != "x" || !reflect.DeepEqual(c.ForwardPorts, []Port{1, 2}) {
		t.Errorf("parsed = %+v", c)
	}
}

func TestScalarMapCoercions(t *testing.T) {
	c, err := Parse([]byte(`{"image":"x","build":{"args":{"s":"v","n":7,"b":true,"z":null}}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := ScalarMap{"s": "v", "n": "7", "b": "true", "z": ""}
	if !reflect.DeepEqual(c.Build.Args, want) {
		t.Errorf("Args = %+v, want %+v", c.Build.Args, want)
	}
	// build present but image too is caught at Resolve.
	if _, err := c.Resolve(Substitutions{}); err == nil {
		t.Error("want ErrImageAndBuild")
	}
}

func TestScalarMapRejectsNonScalar(t *testing.T) {
	if _, err := Parse([]byte(`{"build":{"args":{"k":[1,2]}}}`)); err == nil {
		t.Error("array arg value: want error")
	}
	if _, err := Parse([]byte(`{"build":{"args":[1]}}`)); err == nil {
		t.Error("non-object args: want error")
	}
}

func TestPolymorphicDecodeErrors(t *testing.T) {
	cases := []string{
		`{"dockerComposeFile": 7}`,        // StringOrSlice: not string/[]string
		`{"forwardPorts": [true]}`,        // Port: not int/string
		`{"forwardPorts": ["notaport"]}`,  // Port: unparseable string
		`{"postCreateCommand": 7}`,        // command: not string/array/object
		`{"postCreateCommand": {"a": 7}}`, // command object value: not string/array
		`{"mounts": [7]}`,                 // mount: not string/object
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c)); err == nil {
			t.Errorf("Parse(%s): want error", c)
		}
	}
}

func TestSubstitutionVariables(t *testing.T) {
	cfg := `{
  "image": "x",
  "containerEnv": {
    "WS": "${localWorkspaceFolder}",
    "WSB": "${localWorkspaceFolderBasename}",
    "CWB": "${containerWorkspaceFolderBasename}",
    "ID": "${devcontainerId}",
    "CE": "${containerEnv:WS}-suffix",
    "UNTERM": "${oops"
  }
}`
	spec, err := Resolve([]byte(cfg), Substitutions{
		LocalWorkspaceFolder:     "/home/dev/proj",
		ContainerWorkspaceFolder: "/work/proj",
		DevcontainerID:           "abc123",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{
		"CE=${localWorkspaceFolder}-suffix", // containerEnv:WS reads the raw WS value
		"CWB=proj",
		"ID=abc123",
		"UNTERM=${oops", // unterminated ${ left verbatim
		"WS=/home/dev/proj",
		"WSB=proj",
	}
	if !reflect.DeepEqual(spec.Env, want) {
		t.Errorf("Env = %#v\nwant %#v", spec.Env, want)
	}
}

func TestLifecycleIsZeroAndEmptyFields(t *testing.T) {
	spec, err := Resolve([]byte(`{"image":"x"}`), Substitutions{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !(LifecycleCommand{}).IsZero() {
		t.Error("empty LifecycleCommand.IsZero() = false")
	}
	if spec.Env != nil || spec.ForwardPorts != nil || spec.Mounts != nil ||
		spec.Features != nil || spec.Build != nil || spec.Workspace.Mount != nil {
		t.Errorf("bare image spec should have nil slices/pointers: %+v", spec)
	}
	if len(spec.Lifecycle.PostCreate) != 0 {
		t.Error("no lifecycle hooks expected")
	}
}

func TestTrivialBranches(t *testing.T) {
	// nil LocalEnv with no fallback → empty; base("") → ""; an object
	// mount with a non-bool/non-string readonly is treated as rw.
	spec, err := Resolve([]byte(`{
	  "image": "x",
	  "workspaceFolder": "",
	  "containerEnv": {"E": "${localEnv:NOPE}"},
	  "mounts": [{"source":"/a","target":"/a","readonly":0}]
	}`), Substitutions{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reflect.DeepEqual(spec.Env, []string{"E="}) {
		t.Errorf("Env = %v, want E= (nil LocalEnv)", spec.Env)
	}
	if base("") != "" {
		t.Errorf("base(\"\") = %q, want empty", base(""))
	}
	if spec.Mounts[0].ReadOnly {
		t.Error("readonly:0 (number) should be treated as rw")
	}
}

func TestMountStringReadonlyValueForms(t *testing.T) {
	// readonly with an explicit false, and the ro alias.
	c, err := Parse([]byte(`{"image":"x","mounts":[
	  "source=/a,target=/a,readonly=false",
	  "source=/b,target=/b,ro"
	]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Mounts[0].ReadOnly {
		t.Error("readonly=false should be rw")
	}
	if !c.Mounts[1].ReadOnly {
		t.Error("ro should be readonly")
	}
}
