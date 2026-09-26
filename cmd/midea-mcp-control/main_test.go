package main

import (
	"bufio"
	"flag"
	"strings"
	"testing"
)

func newTestFlagSet(t *testing.T) *flag.FlagSet {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.SetOutput(nil)
	flags.String("config", "", "")
	flags.Duration("timeout", 0, "")
	flags.Bool("json", false, "")
	flags.Bool("confirm", false, "")
	flags.String("mode", "", "")
	flags.Float64("temp", 0, "")
	var triState optionalBool
	flags.Var(&triState, "eco", "")
	return flags
}

func TestSplitArgsAllowsFlagsAroundOneSelector(t *testing.T) {
	selectors, flags, err := splitArgs(newTestFlagSet(t),
		[]string{"bedroom", "--confirm", "--timeout", "30s"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(selectors) != 1 || selectors[0] != "bedroom" {
		t.Fatalf("selectors = %#v", selectors)
	}
	want := []string{"--confirm", "--timeout", "30s"}
	if len(flags) != len(want) {
		t.Fatalf("flags = %#v, want %#v", flags, want)
	}
	for i := range want {
		if flags[i] != want[i] {
			t.Fatalf("flags = %#v, want %#v", flags, want)
		}
	}
}

// A value-taking flag must swallow its value rather than turning it into a
// selector.
func TestSplitArgsDoesNotTreatFlagValuesAsSelectors(t *testing.T) {
	for _, args := range [][]string{
		{"bedroom", "--temp", "21.5", "--confirm"},
		{"--temp", "21.5", "bedroom", "--confirm"},
		{"--config", "/tmp/devices.json", "bedroom", "--mode", "cool"},
	} {
		selectors, _, err := splitArgs(newTestFlagSet(t), args, false)
		if err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
		if len(selectors) != 1 || selectors[0] != "bedroom" {
			t.Errorf("args %v produced selectors %#v", args, selectors)
		}
	}
}

func TestSplitArgsHandlesAttachedValues(t *testing.T) {
	selectors, flagArgs, err := splitArgs(newTestFlagSet(t),
		[]string{"bedroom", "--temp=21.5", "--confirm"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(selectors) != 1 {
		t.Fatalf("selectors = %#v", selectors)
	}
	if len(flagArgs) != 2 || flagArgs[0] != "--temp=21.5" {
		t.Errorf("flagArgs = %#v", flagArgs)
	}
}

func TestSplitArgsRejectsMultipleSelectorsForPower(t *testing.T) {
	if _, _, err := splitArgs(newTestFlagSet(t), []string{"one", "two"}, false); err == nil {
		t.Fatal("splitArgs accepted multiple power selectors")
	}
}

func TestSplitArgsAllowsMultipleSelectorsForStatus(t *testing.T) {
	selectors, _, err := splitArgs(newTestFlagSet(t), []string{"one", "two", "--json"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(selectors) != 2 || selectors[0] != "one" || selectors[1] != "two" {
		t.Fatalf("selectors = %#v", selectors)
	}
}

func TestSplitArgsHandlesBooleanSpellings(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"unit", "--eco", "true"}, []string{"--eco=true"}},
		{[]string{"unit", "--eco", "false"}, []string{"--eco=false"}},
		{[]string{"unit", "--eco=true"}, []string{"--eco=true"}},
		{[]string{"unit", "--eco"}, []string{"--eco"}},
		// A boolean followed by something else must not swallow it.
		{[]string{"unit", "--eco", "--temp", "21"}, []string{"--eco", "--temp", "21"}},
	}
	for _, testCase := range cases {
		selectors, flagArgs, err := splitArgs(newTestFlagSet(t), testCase.args, false)
		if err != nil {
			t.Fatalf("args %v: %v", testCase.args, err)
		}
		if len(selectors) != 1 || selectors[0] != "unit" {
			t.Errorf("args %v produced selectors %#v", testCase.args, selectors)
		}
		if len(flagArgs) != len(testCase.want) {
			t.Fatalf("args %v produced %#v, want %#v", testCase.args, flagArgs, testCase.want)
		}
		for i := range testCase.want {
			if flagArgs[i] != testCase.want[i] {
				t.Errorf("args %v produced %#v, want %#v", testCase.args, flagArgs, testCase.want)
			}
		}
	}
}

// The two prompts must share one reader. A per-prompt bufio.Reader drops
// whatever the previous one buffered, which silently loses the second line of
// piped input.
func TestReadLineConsumesSequentialLines(t *testing.T) {
	original := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader("first@example.com\r\nsecond-line\nthird\n"))
	t.Cleanup(func() { stdinReader = original })

	for _, want := range []string{"first@example.com", "second-line", "third"} {
		got, err := readLine()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("readLine = %q, want %q", got, want)
		}
	}
}

func TestReadLineHandlesEOFWithoutNewline(t *testing.T) {
	original := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader("no-trailing-newline"))
	t.Cleanup(func() { stdinReader = original })

	got, err := readLine()
	if err != nil {
		t.Fatal(err)
	}
	if got != "no-trailing-newline" {
		t.Fatalf("readLine = %q", got)
	}
}

func TestResolveCredentialsFromStdin(t *testing.T) {
	t.Setenv("MIDEA_ACCOUNT", "")
	t.Setenv("MIDEA_PASSWORD", "")
	original := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader("piped@example.com\npiped-password\n"))
	t.Cleanup(func() { stdinReader = original })

	account, password, err := resolveCredentials("", true)
	if err != nil {
		t.Fatal(err)
	}
	if account != "piped@example.com" || password != "piped-password" {
		t.Fatalf("account=%q password=%q", account, password)
	}
}

func TestResolveCredentialsRejectsEmptyInput(t *testing.T) {
	t.Setenv("MIDEA_ACCOUNT", "")
	t.Setenv("MIDEA_PASSWORD", "")
	original := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader("\n\n"))
	t.Cleanup(func() { stdinReader = original })

	if _, _, err := resolveCredentials("", true); err == nil {
		t.Fatal("empty stdin was accepted as credentials")
	}
}

func TestResolveCredentialsPrefersFlagAndEnvironment(t *testing.T) {
	// The documented rule: --credentials-stdin reads only the credentials that
	// are still missing, one per line, account first. Supplying the account
	// elsewhere therefore means the first stdin line is the password.
	original := stdinReader
	t.Cleanup(func() { stdinReader = original })

	t.Setenv("MIDEA_PASSWORD", "")
	stdinReader = bufio.NewReader(strings.NewReader("piped-password\n"))
	account, password, err := resolveCredentials("flag@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	if account != "flag@example.com" || password != "piped-password" {
		t.Fatalf("account=%q password=%q", account, password)
	}

	// A password in the environment wins over stdin, so a piped account can be
	// paired with a secret held out of band.
	t.Setenv("MIDEA_PASSWORD", "from-env")
	stdinReader = bufio.NewReader(strings.NewReader("piped@example.com\n"))
	account, password, err = resolveCredentials("", true)
	if err != nil {
		t.Fatal(err)
	}
	if account != "piped@example.com" || password != "from-env" {
		t.Fatalf("account=%q password=%q", account, password)
	}
}

func TestOptionalBoolTriState(t *testing.T) {
	var value optionalBool
	if value.set {
		t.Fatal("a fresh optionalBool reported itself as set")
	}
	if value.pointer() != nil {
		t.Fatal("an unset optionalBool produced a pointer")
	}
	if err := value.Set("true"); err != nil {
		t.Fatal(err)
	}
	if !value.set || !value.value || value.pointer() == nil || !*value.pointer() {
		t.Fatalf("optionalBool(true) = %+v", value)
	}
	if err := value.Set("false"); err != nil {
		t.Fatal(err)
	}
	pointer := value.pointer()
	if pointer == nil || *pointer {
		t.Fatalf("optionalBool(false) = %+v", value)
	}
	// A later read of the pointer must not be able to mutate the flag.
	*pointer = true
	if value.value {
		t.Error("mutating the returned pointer changed the flag")
	}
	if err := value.Set("maybe"); err == nil {
		t.Error("optionalBool accepted a non-boolean value")
	}
	if !value.IsBoolFlag() {
		t.Error("optionalBool must advertise itself as a boolean flag")
	}
}
