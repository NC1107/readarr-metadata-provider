package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	const content = `# comment line
ENVFILE_TEST_PLAIN=value

  ENVFILE_TEST_SPACED  =  spaced value  
ENVFILE_TEST_DQ="double quoted"
ENVFILE_TEST_SQ='single quoted'
ENVFILE_TEST_EQ=a=b=c
ENVFILE_TEST_EMPTY=
ENVFILE_TEST_REAL=from-file
   # indented comment
NOEQUALS
=novalue
`
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	keys := []string{"ENVFILE_TEST_PLAIN", "ENVFILE_TEST_SPACED", "ENVFILE_TEST_DQ", "ENVFILE_TEST_SQ",
		"ENVFILE_TEST_EQ", "ENVFILE_TEST_EMPTY", "ENVFILE_TEST_REAL", "NOEQUALS"}
	for _, k := range keys {
		os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for _, k := range keys {
			os.Unsetenv(k)
		}
	})
	// A real environment variable must win over the file.
	t.Setenv("ENVFILE_TEST_REAL", "from-env")

	Load(path)

	want := map[string]string{
		"ENVFILE_TEST_PLAIN":  "value",
		"ENVFILE_TEST_SPACED": "spaced value",
		"ENVFILE_TEST_DQ":     "double quoted",
		"ENVFILE_TEST_SQ":     "single quoted",
		"ENVFILE_TEST_EQ":     "a=b=c",
		"ENVFILE_TEST_EMPTY":  "",
		"ENVFILE_TEST_REAL":   "from-env",
	}
	for k, w := range want {
		got, ok := os.LookupEnv(k)
		if !ok {
			t.Errorf("%s not set", k)
			continue
		}
		if got != w {
			t.Errorf("%s = %q, want %q", k, got, w)
		}
	}
	if _, ok := os.LookupEnv("NOEQUALS"); ok {
		t.Error("a line without '=' must be ignored")
	}
	if _, ok := os.LookupEnv(""); ok {
		t.Error("empty key must be ignored")
	}
}

func TestLoadMissingFileIsNoop(t *testing.T) {
	Load(filepath.Join(t.TempDir(), "nope.env")) // must not panic
}
