package exec

import "testing"

func TestBuildRemoteShell(t *testing.T) {
	s, err := BuildRemoteShell("echo", []string{"a"}, map[string]string{"X": "y"})
	if err != nil {
		t.Fatal(err)
	}
	if s == "" {
		t.Fatal("empty")
	}
}

func TestExitCodeFromExecError(t *testing.T) {
	if ExitCodeFromExecError(nil) != 0 {
		t.Fatal("expected 0")
	}
	if ExitCodeFromExecError(&fakeErr{"command terminated with exit code 7"}) != 7 {
		t.Fatal("expected 7")
	}
	if ExitCodeFromExecError(&fakeErr{"other"}) != 1 {
		t.Fatal("expected 1")
	}
}

type fakeErr struct{ s string }

func (f *fakeErr) Error() string { return f.s }
