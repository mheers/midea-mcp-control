package main

import "testing"

func TestSplitArgsAllowsFlagsAroundOneSelector(t *testing.T) {
	selectors, flags, err := splitArgs([]string{"bedroom", "--confirm", "--timeout", "30s"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(selectors) != 1 || selectors[0] != "bedroom" {
		t.Fatalf("selectors = %#v", selectors)
	}
	wantFlags := []string{"--confirm", "--timeout", "30s"}
	if len(flags) != len(wantFlags) {
		t.Fatalf("flags = %#v, want %#v", flags, wantFlags)
	}
	for i := range wantFlags {
		if flags[i] != wantFlags[i] {
			t.Fatalf("flags = %#v, want %#v", flags, wantFlags)
		}
	}
}

func TestSplitArgsRejectsMultipleSelectorsForPower(t *testing.T) {
	if _, _, err := splitArgs([]string{"one", "two"}, false); err == nil {
		t.Fatal("splitArgs accepted multiple power selectors")
	}
}

func TestSplitArgsAllowsMultipleSelectorsForStatus(t *testing.T) {
	selectors, _, err := splitArgs([]string{"one", "two", "--json"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(selectors) != 2 || selectors[0] != "one" || selectors[1] != "two" {
		t.Fatalf("selectors = %#v", selectors)
	}
}
