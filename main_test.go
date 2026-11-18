package main

import (
	"flag"
	"reflect"
	"strings"
	"testing"
)

func TestSplitGlobalFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("config", "", "")
	fs.String("context", "", "")
	fs.Bool("readonly", false, "")
	g, r := splitGlobalFlags(fs, []string{"add", "claude", "--provider", "anthropic", "--config", "c.yaml", "--readonly", "--model=x", "--context=a,b"})
	if !reflect.DeepEqual(g, []string{"--config", "c.yaml", "--readonly", "--context=a,b"}) {
		t.Fatalf("globals = %v", g)
	}
	if !reflect.DeepEqual(r, []string{"add", "claude", "--provider", "anthropic", "--model=x"}) {
		t.Fatalf("rest = %v", r)
	}
	_, r = splitGlobalFlags(fs, []string{"why", "--", "--config", "is", "odd"})
	if !reflect.DeepEqual(r, []string{"why", "--config", "is", "odd"}) {
		t.Fatalf("-- should stop flag parsing: %v", r)
	}
}

func TestFindSubcommand(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("context", "", "")
	fs.String("n", "", "")
	fs.Bool("A", false, "")
	fs.Bool("readonly", false, "")
	cases := []struct {
		in   []string
		sub  string
		rest []string
	}{
		{[]string{"--context", "dev", "scan", "-n", "shop"}, "scan", []string{"--context", "dev", "-n", "shop"}},
		{[]string{"--context=dev", "-A", "report", "--post"}, "report", []string{"--context=dev", "-A", "--post"}},
		{[]string{"--readonly", "serve", "--allow-changes"}, "serve", []string{"--readonly", "--allow-changes"}},
		{[]string{"scan", "--policy"}, "scan", []string{"--policy"}},
		{[]string{"-n", "shop", "--readonly"}, "", []string{"-n", "shop", "--readonly"}},
		{[]string{"--", "scan"}, "", []string{"--", "scan"}},
	}
	for _, c := range cases {
		sub, rest := findSubcommand(fs, c.in)
		if sub != c.sub || strings.Join(rest, " ") != strings.Join(c.rest, " ") {
			t.Errorf("%v: sub=%q rest=%v, want %q %v", c.in, sub, rest, c.sub, c.rest)
		}
	}
}
